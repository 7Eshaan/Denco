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
	"sync"
	"syscall"
	"time"

	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/ipc"
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
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	dir := fs.String("dir", defaultDownloadDir(), "default download directory")
	port := fs.Uint("port", 6881, "TCP port for incoming peers (0 = random; falls back to random if busy)")
	maxActive := fs.Int("max-active", 5, "torrents downloading at once (seeding ones don't count)")
	verbose := fs.Bool("v", false, "debug logging (peers, trackers)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *port > 65535 || *maxActive < 1 {
		fmt.Fprintln(e.stderr, "usage: denco daemon [-dir DIR] [-port N] [-max-active N] [-v]")
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	downloads, err := filepath.Abs(*dir)
	if err != nil {
		return fail(err)
	}

	// Only one daemon per home: both would own the same resume files.
	if c, err := ipc.Connect(ctx, e.daemonFile()); err == nil {
		st, _ := c.Status(ctx)
		return fail(fmt.Errorf("a daemon is already running (pid %d)", st.PID))
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: level}))

	eng, err := engine.Open(engine.Config{
		StateDir:    e.stateDir(),
		DownloadDir: downloads,
		ListenAddr:  fmt.Sprintf(":%d", *port),
		MaxActive:   *maxActive,
		Logger:      log,
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
	fmt.Fprintf(e.stdout, "Denco daemon running (pid %d). %d torrents, peer port %d.\n", os.Getpid(), ov.Torrents, ov.ListenPort)
	fmt.Fprintf(e.stdout, "Downloads go to %s by default. Stop with 'denco stop' or Ctrl-C.\n", downloads)

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

// defaultDownloadDir is ~/Downloads/Denco (configurable in Phase 9).
func defaultDownloadDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "Denco"
	}
	return filepath.Join(home, "Downloads", "Denco")
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
