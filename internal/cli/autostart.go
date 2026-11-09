package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/config"
	"github.com/7Eshaan/Denco/internal/ipc"
)

// Starting the daemon in the background (DECISIONS D52). A command that
// needs the daemon starts one when none is running (setting auto_start,
// default on), and `denco start` does it explicitly. The daemon runs as a
// separate, detached process (detach_*.go): closing the terminal doesn't
// stop it; `denco stop` does.

const (
	startWait      = 30 * time.Second // for the new daemon to answer
	autostartLock  = "autostart.lock" // one CLI starts the daemon at a time
	daemonOutFile  = "daemon.out"     // the background daemon's own output
	defaultLogName = "daemon.log"     // its log, when log_file is none
)

// spawnFunc starts `denco daemon args…` in the background with stdout and
// stderr going to out. exited receives when it stops. Tests replace it
// (env.spawn) with an in-process daemon.
type spawnFunc func(e *env, args []string, out *os.File) (exited <-chan error, err error)

// spawnProcess runs this executable as a detached daemon.
func spawnProcess(e *env, args []string, out *os.File) (<-chan error, error) {
	if testing.Testing() {
		// A test binary would start a copy of itself; tests use env.spawn.
		return nil, errors.New("starting a background daemon is disabled in tests")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, append([]string{"daemon"}, args...)...)
	cmd.Env = append(os.Environ(), "DENCO_HOME="+e.home)
	cmd.Dir = e.home // don't keep the caller's directory busy
	cmd.Stdout, cmd.Stderr = out, out
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return exited, nil
}

// startBackground starts a background daemon (args are daemon flags) and
// returns a client for it. If another `denco` is already starting one, it
// waits for that one instead.
func startBackground(ctx context.Context, e *env, cfg config.Config, args []string) (*ipc.Client, error) {
	lock := filepath.Join(e.home, autostartLock)
	if err := os.MkdirAll(e.home, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		if fi, serr := os.Stat(lock); serr == nil && time.Since(fi.ModTime()) > startWait+5*time.Second {
			os.Remove(lock) // left by a `denco` that was killed while starting
			return startBackground(ctx, e, cfg, args)
		}
		return waitDaemon(ctx, e, nil, nil) // someone else is starting it
	}
	if err != nil {
		return nil, err
	}
	f.Close()
	defer os.Remove(lock)

	// Somebody may have finished starting one just before we took the lock.
	if c, err := ipc.Connect(ctx, e.daemonFile()); err == nil {
		return c, nil
	}
	if _, ok := flagValue(args, "log"); !ok && cfg.LogFile == "" {
		// No terminal to log to: keep the log next to the other state.
		args = append([]string{"-log", filepath.Join(e.home, defaultLogName)}, args...)
	}
	outPath := filepath.Join(e.home, daemonOutFile)
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer out.Close() // the daemon has its own copy
	spawn := e.spawn
	if spawn == nil {
		spawn = spawnProcess
	}
	exited, err := spawn(e, args, out)
	if err != nil {
		return nil, fmt.Errorf("starting the daemon: %w", err)
	}
	return waitDaemon(ctx, e, exited, func() string { return tail(outPath, 15) })
}

// waitDaemon polls until a daemon answers. exited (may be nil) reports a
// daemon that stopped before answering; output then explains why.
func waitDaemon(ctx context.Context, e *env, exited <-chan error, output func() string) (*ipc.Client, error) {
	deadline := time.Now().Add(startWait)
	for {
		if c, err := ipc.Connect(ctx, e.daemonFile()); err == nil {
			return c, nil
		}
		select {
		case err := <-exited:
			msg := "the daemon stopped while starting"
			if err != nil {
				msg += " (" + err.Error() + ")"
			}
			if out := output(); out != "" {
				msg += ":\n" + out
			}
			return nil, errors.New(msg)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the daemon didn't answer within %s (see %s)", startWait, filepath.Join(e.home, daemonOutFile))
		}
	}
}

// flagValue finds -name VALUE or -name=VALUE (also with --) in daemon flags.
func flagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		a = strings.TrimLeft(a, "-")
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v, true
		}
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// tail returns the last n lines of a file.
func tail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// cmdStart starts the daemon in the background (args are daemon flags).
func cmdStart(e *env, args []string) int {
	ctx, cancel := interruptible()
	defer cancel()
	if c, err := ipc.Connect(ctx, e.daemonFile()); err == nil {
		st, _ := c.Status(ctx)
		fmt.Fprintf(e.stdout, "The daemon is already running (pid %d).\n", st.PID)
		return 0
	}
	cfg, err := e.loadConfig()
	if err != nil {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	c, err := startBackground(ctx, e, cfg, args)
	if err != nil {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	describeStarted(ctx, e, e.stdout, c, cfg, args)
	return 0
}

// describeStarted says where the new daemon runs and logs.
func describeStarted(ctx context.Context, e *env, w io.Writer, c *ipc.Client, cfg config.Config, args []string) {
	logPath, ok := flagValue(args, "log")
	switch {
	case ok && strings.EqualFold(logPath, "none"):
		logPath = filepath.Join(e.home, daemonOutFile) // its "terminal"
	case ok:
		logPath, _ = filepath.Abs(logPath)
	}
	if !ok {
		if logPath = cfg.LogFile; logPath == "" {
			logPath = filepath.Join(e.home, defaultLogName)
		}
	}
	st, err := c.Status(ctx)
	if err != nil {
		fmt.Fprintf(w, "Started the daemon in the background. Log: %s\n", logPath)
		return
	}
	fmt.Fprintf(w, "Started the daemon in the background (pid %d). Log: %s. Stop it with 'denco stop'.\n", st.PID, logPath)
}
