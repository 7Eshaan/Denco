package tui

import (
	"errors"
	"io"
	"os"
	"strconv"
	"sync"
)

// Terminal is everything the TUI needs from the terminal. Open returns the
// real one; tests use a fake.
type Terminal interface {
	io.Reader // keyboard input
	io.Writer // screen output
	// Size is the screen size in cells.
	Size() (cols, rows int)
	// Resized receives when the size changed (nil: never).
	Resized() <-chan struct{}
	// Raw reports whether input arrives key by key. False means the
	// line-mode fallback: keys arrive after Enter (see model.line).
	Raw() bool
	// Restore puts the terminal back as it was. Safe to call twice.
	Restore() error
}

// ErrNotTerminal: output isn't a terminal (piped or redirected).
var ErrNotTerminal = errors.New("the TUI needs an interactive terminal (output is redirected); use 'denco list' instead")

// IsTerminal reports whether f is a terminal (checked before starting the
// daemon, so `denco tui > file` fails without side effects).
func IsTerminal(f *os.File) bool { return isTerminal(f) }

// osTerminal is the real terminal: platform code (term_*.go) switches
// input to raw mode and output to ANSI processing, and reports resizes.
type osTerminal struct {
	in, out *os.File
	raw     bool
	undo    []func() error // run in reverse by Restore
	resized chan struct{}
	stop    func()
	once    sync.Once
	err     error
}

// Open prepares the terminal for the TUI. If raw input isn't available
// (input is piped, or the platform can't do it) the terminal still opens,
// in line mode. Call Restore when done, also after errors and panics.
func Open(in, out *os.File) (Terminal, error) {
	if !isTerminal(out) {
		return nil, ErrNotTerminal
	}
	t := &osTerminal{in: in, out: out, resized: make(chan struct{}, 1)}
	undoVT, err := enableVT(out)
	if err != nil {
		return nil, err
	}
	if undoVT != nil {
		t.undo = append(t.undo, undoVT)
	}
	if isTerminal(in) {
		if undoRaw, err := makeRaw(in); err == nil {
			t.raw = true
			t.undo = append(t.undo, undoRaw)
		}
	}
	t.stop = watchResize(out, t.resized)
	return t, nil
}

func (t *osTerminal) Read(p []byte) (int, error)  { return t.in.Read(p) }
func (t *osTerminal) Write(p []byte) (int, error) { return t.out.Write(p) }
func (t *osTerminal) Resized() <-chan struct{}    { return t.resized }
func (t *osTerminal) Raw() bool                   { return t.raw }

func (t *osTerminal) Size() (int, int) {
	if w, h, err := termSize(t.out); err == nil && w > 0 && h > 0 {
		return w, h
	}
	return envSize()
}

func (t *osTerminal) Restore() error {
	t.once.Do(func() {
		t.stop()
		var errs []error
		for i := len(t.undo) - 1; i >= 0; i-- {
			errs = append(errs, t.undo[i]())
		}
		t.err = errors.Join(errs...)
	})
	return t.err
}

// envSize is the fallback when the OS can't tell: $COLUMNS × $LINES, or 80×24.
func envSize() (int, int) {
	w, err1 := strconv.Atoi(os.Getenv("COLUMNS"))
	h, err2 := strconv.Atoi(os.Getenv("LINES"))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}
