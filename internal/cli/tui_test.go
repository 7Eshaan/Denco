package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/ipc"
)

// pipeTerm is a terminal for runTUI: keys are written to a pipe, the
// screen goes to a buffer.
type pipeTerm struct {
	keys *io.PipeWriter
	in   *io.PipeReader
	mu   sync.Mutex
	out  bytes.Buffer
}

func newPipeTerm() *pipeTerm {
	r, w := io.Pipe()
	return &pipeTerm{keys: w, in: r}
}

func (p *pipeTerm) Read(b []byte) (int, error) { return p.in.Read(b) }
func (p *pipeTerm) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.Write(b)
}
func (p *pipeTerm) Size() (int, int)         { return 140, 30 }
func (p *pipeTerm) Resized() <-chan struct{} { return nil }
func (p *pipeTerm) Raw() bool                { return true }
func (p *pipeTerm) Restore() error           { return nil }
func (p *pipeTerm) screen() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

// The TUI against a real daemon: every operation goes through the daemon,
// and quitting leaves it running.
func TestTUIAgainstRealDaemon(t *testing.T) {
	tmp := t.TempDir()
	home, dl := filepath.Join(tmp, "home"), filepath.Join(tmp, "downloads")
	h := startDaemon(t, home, dl)
	torrentPath, _ := writeTorrent(t, tmp, "alpha", 1)

	ctx := context.Background()
	c, err := ipc.Connect(ctx, filepath.Join(home, ipc.DaemonFile))
	if err != nil {
		t.Fatal(err)
	}
	term := newPipeTerm()
	e := &env{home: home, stdout: io.Discard, stderr: io.Discard}
	done := make(chan int, 1)
	go func() { done <- runTUI(ctx, e, c, term, 20*time.Millisecond) }()

	keys := func(s string) {
		t.Helper()
		if _, err := term.keys.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				scr := term.screen()
				t.Fatalf("timed out waiting for %s; screen ends with:\n%q", what, scr[max(0, len(scr)-1500):])
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	state := func(ref string) engine.State {
		tr, err := c.Get(ctx, ref)
		if err != nil {
			return ""
		}
		return tr.State
	}
	onScreen := func(s string) func() bool { return func() bool { return strings.Contains(term.screen(), s) } }

	wait("the empty dashboard", onScreen("No torrents yet"))

	// Add a .torrent file, the way a terminal pastes a dragged-in path.
	keys("a" + torrentPath + "\r")
	wait("#1 added", func() bool { list, _ := c.List(ctx); return len(list) == 1 })
	wait("the added torrent on screen", onScreen("alpha"))
	wait("the result message", onScreen("Added #1 alpha"))

	// An add that fails shows why.
	keys("a" + filepath.Join(tmp, "missing.torrent") + "\r")
	wait("the error", onScreen("missing.torrent"))

	// A magnet link.
	keys("amagnet:?xt=urn:btih:" + strings.Repeat("ab", 20) + "&dn=bravo\r")
	wait("#2 added", func() bool { list, _ := c.List(ctx); return len(list) == 2 })

	keys("p")
	wait("#1 paused in the daemon", func() bool { return state("1") == engine.StatePaused })
	keys("r")
	wait("#1 resumed in the daemon", func() bool { return state("1") != engine.StatePaused })

	keys("\r")
	wait("details with the file list", onScreen("Files (1)"))
	wait("the info hash", onScreen("Info hash"))
	keys("\x1b")

	keys("d")
	wait("the confirmation", onScreen("Remove #1 alpha?"))
	keys("y")
	wait("#1 removed from the daemon", func() bool { _, err := c.Get(ctx, "1"); return err != nil })
	wait("the removal message", onScreen("Removed #1 alpha (files kept)"))

	keys("q")
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("runTUI exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the TUI didn't quit")
	}
	if !strings.HasSuffix(term.screen(), "\x1b[?25h\x1b[?1049l") {
		t.Error("the TUI should leave the alternate screen and show the cursor")
	}
	// Quitting the TUI left the daemon and the other torrent alone.
	if out := h.mustCmd("list"); !strings.Contains(out, "bravo") {
		t.Fatalf("after quitting, the daemon should still have #2:\n%s", out)
	}
}

func TestTUINeedsATerminal(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	h := &daemonHarness{t: t, home: home}
	// go test's stdout isn't a terminal, like `denco tui > file`.
	code, _, errOut := h.cmd("tui")
	if code != 1 || !strings.Contains(errOut, "interactive terminal") {
		t.Fatalf("denco tui without a terminal: exit %d, %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, ipc.DaemonFile)); err == nil {
		t.Fatal("refusing must not start a daemon first")
	}
	if code, _, _ := h.cmd("tui", "extra"); code != 2 {
		t.Errorf("tui with an argument: exit %d", code)
	}
}

func TestTypedPath(t *testing.T) {
	dir := t.TempDir()
	spaced := filepath.Join(dir, "my file.torrent")
	if err := os.WriteFile(spaced, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	homeDir, _ := os.UserHomeDir()
	tests := map[string]string{
		`  ` + spaced + `  `:                         spaced,
		`"` + spaced + `"`:                           spaced,
		`'` + spaced + `'`:                           spaced,
		strings.ReplaceAll(spaced, " ", `\ `):        spaced, // dragged into the terminal
		"~/x.torrent":                                filepath.Join(homeDir, "x.torrent"),
		"magnet:?xt=urn:btih:abc&dn=a\\ b":           "magnet:?xt=urn:btih:abc&dn=a\\ b",
		"https://example.org/~/x.torrent":            "https://example.org/~/x.torrent",
		filepath.Join(dir, "missing\\ file.torrent"): filepath.Join(dir, "missing\\ file.torrent"),
	}
	for in, want := range tests {
		if got := typedPath(in); got != want {
			t.Errorf("typedPath(%q) = %q, want %q", in, got, want)
		}
	}
}
