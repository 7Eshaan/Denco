package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/7Eshaan/Denco/internal/config"
	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/ipc"
	"github.com/7Eshaan/Denco/internal/torrent"
)

// cmdDaemon runs the engine in the foreground until Ctrl-C, SIGTERM or
// `denco stop`. Run it in its own terminal (or under launchd/systemd).
func cmdDaemon(e *env, args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Once a clean shutdown has begun, give the signals back to the OS, so a
	// second Ctrl-C quits at once (resume data is crash-safe, D27).
	return runDaemon(ctx, e, args, func() { stop() })
}

// runDaemon is cmdDaemon with an explicit context, so tests can stop it.
// stopping, if non-nil, is called when shutdown begins.
func runDaemon(ctx context.Context, e *env, args []string, stopping func()) int {
	fail := func(err error) int {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return fail(err)
	}
	cfg, verbose, code := daemonFlags(e, cfg, args)
	if code != 0 {
		return code
	}

	// Only one daemon per home: both would own the same resume files.
	if c, err := ipc.Connect(ctx, e.daemonFile()); err == nil {
		st, _ := c.Status(ctx)
		return fail(fmt.Errorf("a daemon is already running (pid %d)", st.PID))
	}

	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logOut := e.stderr
	if cfg.LogFile != "" {
		lf, err := openLogFile(cfg.LogFile, maxLogSize)
		if err != nil {
			return fail(err)
		}
		defer lf.Close()
		logOut = lf
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: level}))

	stateDir := cfg.ResumeDir
	if !cfg.ResumeEnabled {
		// The engine always keeps state; give it a throwaway directory so
		// nothing survives this run.
		if stateDir, err = os.MkdirTemp("", "denco-noresume-*"); err != nil {
			return fail(err)
		}
		defer os.RemoveAll(stateDir)
	}
	eng, err := engine.Open(engine.Config{
		StateDir:      stateDir,
		DownloadDir:   cfg.DownloadDir,
		ListenAddr:    fmt.Sprintf(":%d", cfg.ListenPort),
		MaxActive:     cfg.MaxActiveDownloads,
		DownloadLimit: cfg.DownloadLimit,
		UploadLimit:   cfg.UploadLimit,
		Session:       torrent.Config{MaxPeers: cfg.MaxConnectionsPerTorrent, PEX: cfg.EnablePEX},
		EnableDHT:     cfg.EnableDHT,
		PortForward:   cfg.PortForwarding,
		SeedRatio:     cfg.SeedRatio,
		SeedTime:      cfg.SeedTime,
		DHTBootstrap:  cfg.DHTRouters(),
		Logger:        log,
	})
	if err != nil {
		return fail(err)
	}

	shutdown := make(chan struct{})
	var once sync.Once
	token := ipc.NewToken()
	srv, addr, err := ipc.Serve(ipc.NewHandler(eng, token, func() { once.Do(func() { close(shutdown) }) }, log), log)
	if err != nil {
		eng.Close()
		return fail(err)
	}
	if err := ipc.WriteInfo(e.daemonFile(), ipc.Info{Addr: addr.String(), Token: token, PID: os.Getpid()}); err != nil {
		srv.Close()
		eng.Close()
		return fail(err)
	}

	ov := eng.Overview()
	dhtNote := "DHT off"
	if ov.DHT {
		dhtNote = fmt.Sprintf("DHT on UDP port %d", ov.DHTPort)
	}
	fmt.Fprintf(e.stdout, "Denco daemon running (pid %d). %d torrents, peer port %d, %s.\n", os.Getpid(), ov.Torrents, ov.ListenPort, dhtNote)
	fmt.Fprintf(e.stdout, "Downloads go to %s by default. Limits: ↓ %s  ↑ %s. Seeding: %s.\n",
		cfg.DownloadDir, config.DescribeRate(cfg.DownloadLimit), config.DescribeRate(cfg.UploadLimit),
		describeSeedLimits(cfg.SeedRatio, cfg.SeedTime))
	if ov.PortMap != nil {
		fmt.Fprintln(e.stdout, "Asking your router to forward the peer port (UPnP / NAT-PMP); 'denco status' shows the result.")
	}
	if cfg.LogFile != "" {
		fmt.Fprintf(e.stdout, "Logging to %s.\n", cfg.LogFile)
	}
	for _, note := range daemonNotes(e, cfg) {
		fmt.Fprintln(e.stdout, note)
	}
	fmt.Fprintln(e.stdout, "Stop with 'denco stop' or Ctrl-C.")

	select {
	case <-ctx.Done():
	case <-shutdown:
	}
	if stopping != nil {
		stopping()
	}
	fmt.Fprintln(e.stdout, "Stopping: saving progress and telling trackers… (Ctrl-C again to quit now)")
	// Keep answering clients while torrents stop; then retire the daemon
	// file so `denco stop` (which waits for it) knows we're done.
	eng.Close()
	removeDaemonFile(e.daemonFile(), token)
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	srv.Shutdown(sctx)
	cancel()
	fmt.Fprintln(e.stdout, "Stopped.")
	return 0
}

// removeDaemonFile deletes the daemon file if it is still ours (another
// daemon may have replaced it after we were thought dead).
func removeDaemonFile(path, token string) {
	if info, err := ipc.ReadInfo(path); err == nil && info.Token == token {
		os.Remove(path)
	}
}

// daemonFlagKeys maps daemon flags to the config settings they override.
var daemonFlagKeys = map[string]string{
	"dir":        "download_dir",
	"port":       "listen_port",
	"max-active": "max_active_downloads",
	"max-peers":  "max_connections_per_torrent",
	"down-limit": "download_limit",
	"up-limit":   "upload_limit",
	"seed-ratio": "seed_ratio",
	"seed-time":  "seed_time",
	"log":        "log_file",
}

// daemonFlags parses the daemon's flags over cfg (config file values are
// the defaults; flags given explicitly win). Values are validated by the
// same rules as the config file.
func daemonFlags(e *env, cfg config.Config, args []string) (config.Config, bool, int) {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	def := func(key string) string { v, _ := cfg.Get(key); return v }
	fs.String("dir", def("download_dir"), "default download directory")
	fs.String("port", def("listen_port"), "TCP port for incoming peers (0 = random; a random one is used if busy)")
	fs.String("max-active", def("max_active_downloads"), "torrents downloading at once (seeding ones don't count)")
	fs.String("max-peers", def("max_connections_per_torrent"), "peer connections per torrent")
	fs.String("down-limit", def("download_limit"), "total download limit, e.g. 2M or 500K (0 = unlimited)")
	fs.String("up-limit", def("upload_limit"), "total upload limit (0 = unlimited)")
	fs.String("seed-ratio", def("seed_ratio"), "stop seeding at this ratio, e.g. 2.0 (0 = no limit)")
	fs.String("seed-time", def("seed_time"), "stop seeding after this long, e.g. 24h or 2d (0 = no limit)")
	fs.String("log", def("log_file"), "write the log to this file instead of the terminal (none = terminal)")
	verbose := fs.Bool("v", false, "debug logging (peers, trackers)")
	usageLine := "usage: denco daemon [-dir DIR] [-port N] [-max-active N] [-max-peers N] [-down-limit R] [-up-limit R] [-seed-ratio X] [-seed-time D] [-log FILE] [-v]"
	if err := fs.Parse(args); err != nil {
		return cfg, false, 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(e.stderr, usageLine)
		return cfg, false, 2
	}
	code := 0
	fs.Visit(func(f *flag.Flag) {
		key, ok := daemonFlagKeys[f.Name]
		if !ok || code != 0 {
			return
		}
		v := f.Value.String()
		if (f.Name == "dir" || f.Name == "log") && !strings.EqualFold(v, "none") {
			v, _ = filepath.Abs(v) // flags may be relative to where you are
		}
		if err := cfg.Set(key, v); err != nil {
			fmt.Fprintf(e.stderr, "denco: -%s: %v\n%s\n", f.Name, err, usageLine)
			code = 2
		}
	})
	return cfg, *verbose, code
}

// daemonNotes explains settings that are on but can't do anything yet.
func daemonNotes(e *env, cfg config.Config) []string {
	var notes []string
	if !cfg.ResumeEnabled {
		notes = append(notes, "Note: resume_enabled is off: torrents are forgotten when the daemon stops.")
	}
	return notes
}

// cmdStop asks the daemon to stop and waits until it has.
func cmdStop(e *env, args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(e.stderr, "usage: denco stop")
		return 2
	}
	ctx := context.Background()
	c, code := connect(ctx, e)
	if c == nil {
		return code
	}
	if err := c.Shutdown(ctx); err != nil {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	fmt.Fprint(e.stdout, "Stopping daemon…")
	// Stopping can take a few seconds: every torrent announces "stopped".
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if _, err := ipc.Connect(ctx, e.daemonFile()); errors.Is(err, ipc.ErrNoDaemon) {
			fmt.Fprintln(e.stdout, " stopped.")
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Fprintln(e.stdout)
	fmt.Fprintln(e.stderr, "denco: the daemon hasn't stopped after a minute")
	return 1
}
