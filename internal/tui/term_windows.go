//go:build windows

package tui

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// Windows consoles: console modes through kernel32 (loaded with
// syscall.NewLazyDLL, so no cgo and no x/sys). Raw input = no line
// editing, no echo, no Ctrl-C processing, and virtual-terminal input, so
// arrow keys arrive as the same escape sequences as on Unix. Output needs
// virtual-terminal processing for ANSI codes (Windows 10 1511 and later).
// There is no resize signal: the size is polled.

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

const (
	enableProcessedInput            = 0x0001
	enableLineInput                 = 0x0002
	enableEchoInput                 = 0x0004
	enableVirtualTerminalInput      = 0x0200
	enableProcessedOutput           = 0x0001
	enableVirtualTerminalProcessing = 0x0004
)

func getMode(f *os.File) (uint32, error) {
	var m uint32
	err := syscall.GetConsoleMode(syscall.Handle(f.Fd()), &m)
	return m, err
}

func setMode(f *os.File, m uint32) error {
	if r, _, err := procSetConsoleMode.Call(f.Fd(), uintptr(m)); r == 0 {
		return err
	}
	return nil
}

func isTerminal(f *os.File) bool {
	_, err := getMode(f)
	return err == nil
}

func makeRaw(f *os.File) (func() error, error) {
	old, err := getMode(f)
	if err != nil {
		return nil, err
	}
	raw := old&^(enableEchoInput|enableProcessedInput|enableLineInput) | enableVirtualTerminalInput
	if err := setMode(f, raw); err != nil {
		return nil, err
	}
	return func() error { return setMode(f, old) }, nil
}

// enableVT turns on ANSI escape processing for output. Without it the
// screen would fill with escape codes, so the TUI refuses to start.
func enableVT(f *os.File) (func() error, error) {
	old, err := getMode(f)
	if err != nil {
		return nil, err
	}
	if err := setMode(f, old|enableProcessedOutput|enableVirtualTerminalProcessing); err != nil {
		return nil, fmt.Errorf("this console doesn't support ANSI escape codes (needs Windows 10 or later): %w", err)
	}
	return func() error { return setMode(f, old) }, nil
}

type coord struct{ X, Y int16 }

type consoleInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            struct{ Left, Top, Right, Bottom int16 }
	MaximumWindowSize coord
}

func termSize(f *os.File) (int, int, error) {
	var ci consoleInfo
	if r, _, err := procGetConsoleScreenBufferInfo.Call(f.Fd(), uintptr(unsafe.Pointer(&ci))); r == 0 {
		return 0, 0, err
	}
	w := int(ci.Window.Right-ci.Window.Left) + 1
	h := int(ci.Window.Bottom-ci.Window.Top) + 1
	if w <= 0 || h <= 0 {
		return 0, 0, errors.New("console reports no window size")
	}
	return w, h, nil
}

// watchResize polls the window size (Windows has no SIGWINCH).
func watchResize(f *os.File, ch chan struct{}) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		w, h, _ := termSize(f)
		for {
			select {
			case <-t.C:
				nw, nh, err := termSize(f)
				if err == nil && (nw != w || nh != h) {
					w, h = nw, nh
					select {
					case ch <- struct{}{}:
					default:
					}
				}
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

// QuitSignals: Ctrl-C (when input isn't raw) and closing the console window.
func QuitSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }
