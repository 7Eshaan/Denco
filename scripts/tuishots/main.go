// Command tuishots renders the README's screenshots of `denco tui`. It
// runs the real dashboard (tui.Run) against a demo daemon, presses keys
// like a user, captures what the TUI draws with a small terminal emulator
// (vt.go) and writes the screens as SVG (svg.go):
//
//	go run ./scripts/tuishots            # writes assets/tui-*.svg
//
// Nothing here is part of the denco binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/7Eshaan/Denco/internal/tui"
)

// shotTerm is a tui.Terminal: keys come from a pipe, output goes to vt.
type shotTerm struct {
	keys *io.PipeReader
	w, h int
	mu   sync.Mutex
	v    *vt
}

func (t *shotTerm) Read(p []byte) (int, error) { return t.keys.Read(p) }
func (t *shotTerm) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.v.Write(p)
}
func (t *shotTerm) Size() (int, int)         { return t.w, t.h }
func (t *shotTerm) Resized() <-chan struct{} { return nil }
func (t *shotTerm) Raw() bool                { return true }
func (t *shotTerm) Restore() error           { return nil }

// step is a key to press, then text to wait for, then (if name is set) a
// screenshot to save.
type step struct {
	keys, waitFor, name, title string
}

func main() {
	out := flag.String("out", "assets", "directory for the SVG files")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail(err)
	}
	shots := []struct {
		w, h  int
		steps []step
	}{
		{132, 16, []step{
			{"", "ubuntu-24.04", "", ""},
			{"j", "2/8", "tui-dashboard.svg", "denco tui"},
		}},
		{132, 20, []step{
			{"", "ubuntu-24.04", "", ""},
			{"jj\r", "Files (7)", "tui-details.svg", "denco tui — torrent details"},
		}},
	}
	for _, s := range shots {
		if err := shoot(s.w, s.h, s.steps, *out); err != nil {
			fail(err)
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "tuishots:", err)
	os.Exit(1)
}

// shoot runs the dashboard on a w×h screen through the steps.
func shoot(w, h int, steps []step, dir string) error {
	r, keys := io.Pipe()
	term := &shotTerm{keys: r, w: w, h: h, v: newVT(w, h)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// A refresh an hour: the screen stays put once the data is in.
		done <- tui.Run(ctx, term, tui.Config{Client: demo{}, Refresh: time.Hour, Color: true})
	}()
	for _, st := range steps {
		if st.keys != "" {
			keys.Write([]byte(st.keys))
		}
		if err := waitFor(term, st.waitFor); err != nil {
			return err
		}
		time.Sleep(200 * time.Millisecond) // let the frame after it settle
		if st.name == "" {
			continue
		}
		term.mu.Lock()
		img := svg(term.v, st.title)
		term.mu.Unlock()
		path := filepath.Join(dir, st.name)
		if err := os.WriteFile(path, []byte(img), 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", path)
	}
	keys.Write([]byte("q"))
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		return fmt.Errorf("the dashboard didn't quit")
	}
}

func waitFor(term *shotTerm, s string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		term.mu.Lock()
		ok := strings.Contains(term.v.text(), s)
		term.mu.Unlock()
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			term.mu.Lock()
			screen := term.v.text()
			term.mu.Unlock()
			return fmt.Errorf("%q never appeared; screen:\n%s", s, screen)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
