package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSpawner runs the daemon in-process in place of a background
// process: runDaemon with the args the CLI chose, output to its file.
type fakeSpawner struct {
	t     *testing.T
	calls atomic.Int32
	mu    sync.Mutex
	args  [][]string
	fail  string // non-empty: the "daemon" prints this and exits 1
}

func (f *fakeSpawner) spawn(e *env, args []string, out *os.File) (<-chan error, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.args = append(f.args, args)
	f.mu.Unlock()
	exited := make(chan error, 1)
	if f.fail != "" {
		fmt.Fprintln(out, f.fail)
		exited <- errors.New("exit status 1")
		return exited, nil
	}
	// The real daemon outlives the command; keep this one until the test ends.
	ctx, cancel := context.WithCancel(context.Background())
	var buf syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- runDaemon(ctx, &env{home: e.home, stdout: &buf, stderr: &buf}, append([]string{"-port", "0"}, args...), nil)
	}()
	f.t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			f.t.Error("daemon did not exit")
		}
	})
	go func() {
		code := <-done
		if code != 0 {
			exited <- fmt.Errorf("exit status %d", code)
		}
		done <- code // for Cleanup
	}()
	return exited, nil
}

func autoEnv(home string, f *fakeSpawner) (*env, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return &env{home: home, stdout: &out, stderr: &errOut, spawn: f.spawn}, &out, &errOut
}

func TestCommandStartsDaemon(t *testing.T) {
	home := t.TempDir()
	f := &fakeSpawner{t: t}
	e, out, errOut := autoEnv(home, f)
	if code := runEnv(e, []string{"list"}); code != 0 {
		t.Fatalf("list: exit %d\n%s%s", code, out, errOut)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("spawned %d daemons", f.calls.Load())
	}
	// It said so on stderr, and logs to a file: there's no terminal.
	log := filepath.Join(home, defaultLogName)
	if !strings.Contains(errOut.String(), "Started the daemon in the background") || !strings.Contains(errOut.String(), log) {
		t.Errorf("stderr: %q", errOut)
	}
	if strings.Contains(out.String(), "Started") {
		t.Errorf("the notice went to stdout: %q", out)
	}
	if got := strings.Join(f.args[0], " "); got != "-log "+log {
		t.Errorf("daemon args %q", got)
	}
	// The next command finds it running.
	e2, _, errOut2 := autoEnv(home, f)
	if code := runEnv(e2, []string{"status"}); code != 0 || f.calls.Load() != 1 || errOut2.Len() != 0 {
		t.Errorf("second command: exit %d, %d spawns, stderr %q", code, f.calls.Load(), errOut2)
	}
}

// Several commands at once start one daemon between them.
func TestConcurrentCommandsStartOneDaemon(t *testing.T) {
	home := t.TempDir()
	f := &fakeSpawner{t: t}
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, _, _ := autoEnv(home, f)
			codes[i] = runEnv(e, []string{"list"})
		}()
	}
	wg.Wait()
	if n := f.calls.Load(); n != 1 {
		t.Errorf("%d daemons started", n)
	}
	for i, c := range codes {
		if c != 0 {
			t.Errorf("command %d: exit %d", i, c)
		}
	}
	if _, err := os.Stat(filepath.Join(home, autostartLock)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file left behind: %v", err)
	}
}

func TestStartCommand(t *testing.T) {
	home := t.TempDir()
	f := &fakeSpawner{t: t}
	e, out, errOut := autoEnv(home, f)
	if code := runEnv(e, []string{"start", "-log", filepath.Join(home, "my.log")}); code != 0 {
		t.Fatalf("start: exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out.String(), "my.log") || strings.Contains(strings.Join(f.args[0], " "), defaultLogName) {
		t.Errorf("own -log not honoured: out %q, args %q", out, f.args[0])
	}
	e2, out2, _ := autoEnv(home, f)
	if code := runEnv(e2, []string{"start"}); code != 0 || !strings.Contains(out2.String(), "already running") || f.calls.Load() != 1 {
		t.Errorf("second start: exit %d, %q, %d spawns", code, out2, f.calls.Load())
	}
}

func TestStartFailureShowsWhy(t *testing.T) {
	home := t.TempDir()
	f := &fakeSpawner{t: t, fail: "denco: config.yaml line 3: unknown setting \"colour\""}
	e, _, errOut := autoEnv(home, f)
	if code := runEnv(e, []string{"add", "x.torrent"}); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errOut.String(), "stopped while starting") || !strings.Contains(errOut.String(), "unknown setting") {
		t.Errorf("stderr: %q", errOut)
	}
}

func TestNoAutoStartWhenOffOrStopping(t *testing.T) {
	home := t.TempDir()
	f := &fakeSpawner{t: t}
	e, out, _ := autoEnv(home, f)
	if code := runEnv(e, []string{"stop"}); code != 0 || !strings.Contains(out.String(), "isn't running") {
		t.Errorf("stop: exit %d, %q", code, out)
	}
	e, _, _ = autoEnv(home, f)
	if code := runEnv(e, []string{"config", "set", "auto_start", "off"}); code != 0 {
		t.Fatal("config set")
	}
	e, _, errOut := autoEnv(home, f)
	if code := runEnv(e, []string{"list"}); code != 1 || !strings.Contains(errOut.String(), "denco start") {
		t.Errorf("list with auto_start off: exit %d, %q", code, errOut)
	}
	if f.calls.Load() != 0 {
		t.Errorf("%d daemons started", f.calls.Load())
	}
}

// A lock left by a `denco` killed while starting doesn't block forever.
func TestStaleStartLockIsIgnored(t *testing.T) {
	home := t.TempDir()
	lock := filepath.Join(home, autostartLock)
	os.WriteFile(lock, nil, 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(lock, old, old)
	f := &fakeSpawner{t: t}
	e, out, errOut := autoEnv(home, f)
	if code := runEnv(e, []string{"list"}); code != 0 || f.calls.Load() != 1 {
		t.Fatalf("exit %d, %d spawns\n%s%s", code, f.calls.Load(), out, errOut)
	}
}
