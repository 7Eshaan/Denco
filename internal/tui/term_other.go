//go:build !darwin && !linux && !windows

package tui

import (
	"errors"
	"os"
)

// Other systems (BSDs and the like) aren't supported targets. The TUI
// still runs there in its line-mode fallback: output is assumed to be an
// ANSI terminal when $TERM says so, keys arrive after Enter, and the size
// comes from $COLUMNS/$LINES.

func isTerminal(f *os.File) bool {
	return f == os.Stdout && os.Getenv("TERM") != "" && os.Getenv("TERM") != "dumb"
}

func makeRaw(*os.File) (func() error, error) {
	return nil, errors.New("raw terminal input isn't supported on this OS")
}

func termSize(*os.File) (int, int, error) {
	return 0, 0, errors.New("terminal size isn't available on this OS")
}

func enableVT(*os.File) (func() error, error) { return nil, nil }

func watchResize(*os.File, chan struct{}) (stop func()) { return func() {} }

// QuitSignals: an interrupt only.
func QuitSignals() []os.Signal { return []os.Signal{os.Interrupt} }
