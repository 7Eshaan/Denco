//go:build windows

package tui

import (
	"errors"
	"os"
	"testing"
)

func TestNotAConsole(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if IsTerminal(r) || IsTerminal(w) {
		t.Fatal("a pipe isn't a console")
	}
	if _, err := Open(r, w); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("Open with redirected output: %v, want ErrNotTerminal", err)
	}
	if _, err := makeRaw(r); err == nil {
		t.Fatal("raw mode on a pipe should fail")
	}
	if _, _, err := termSize(w); err == nil {
		t.Fatal("a pipe has no console window")
	}
}
