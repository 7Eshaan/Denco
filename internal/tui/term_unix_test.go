//go:build darwin || linux

package tui

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestRawTermios(t *testing.T) {
	var cooked syscall.Termios
	cooked.Iflag = syscall.ICRNL | syscall.IXON | syscall.BRKINT
	cooked.Oflag = syscall.OPOST
	cooked.Lflag = syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	cooked.Cflag = syscall.PARENB
	raw := rawTermios(cooked)

	if raw.Lflag&(syscall.ECHO|syscall.ICANON|syscall.ISIG|syscall.IEXTEN) != 0 {
		t.Errorf("echo, line editing or signal keys still on: %#x", raw.Lflag)
	}
	if raw.Iflag&(syscall.ICRNL|syscall.IXON|syscall.BRKINT) != 0 {
		t.Errorf("input translation still on: %#x", raw.Iflag)
	}
	if raw.Oflag&syscall.OPOST == 0 {
		t.Error("output processing should stay on")
	}
	if raw.Cflag&syscall.CS8 == 0 || raw.Cflag&syscall.PARENB != 0 {
		t.Errorf("want 8-bit characters, no parity: %#x", raw.Cflag)
	}
	if raw.Cc[syscall.VMIN] != 1 || raw.Cc[syscall.VTIME] != 0 {
		t.Error("reads should return after one byte, without a timeout")
	}
}

func TestNotATerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if IsTerminal(r) || IsTerminal(w) {
		t.Fatal("a pipe isn't a terminal")
	}
	if _, err := Open(r, w); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("Open with redirected output: %v, want ErrNotTerminal", err)
	}
	if _, err := makeRaw(r); err == nil {
		t.Fatal("raw mode on a pipe should fail")
	}
	if _, _, err := termSize(w); err == nil {
		t.Fatal("a pipe has no window size")
	}
}

func TestEnvSize(t *testing.T) {
	t.Setenv("COLUMNS", "132")
	t.Setenv("LINES", "43")
	if w, h := envSize(); w != 132 || h != 43 {
		t.Errorf("envSize = %dx%d", w, h)
	}
	t.Setenv("COLUMNS", "junk")
	if w, h := envSize(); w != 80 || h != 24 {
		t.Errorf("fallback = %dx%d", w, h)
	}
}
