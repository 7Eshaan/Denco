//go:build darwin || linux

package tui

import (
	"os"
	"os/signal"
	"syscall"
	"unsafe"
)

// Unix terminals (macOS, Linux): termios through ioctl, window size through
// TIOCGWINSZ, resizes through SIGWINCH. The ioctl request numbers differ
// per OS (term_darwin.go, term_linux.go).

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

func getTermios(f *os.File) (*syscall.Termios, error) {
	var t syscall.Termios
	if err := ioctl(f.Fd(), ioctlGetTermios, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	return &t, nil
}

func setTermios(f *os.File, t *syscall.Termios) error {
	return ioctl(f.Fd(), ioctlSetTermios, unsafe.Pointer(t))
}

func isTerminal(f *os.File) bool {
	_, err := getTermios(f)
	return err == nil
}

// rawTermios turns off line editing, echo, signal keys (Ctrl-C arrives as
// a key and quits cleanly) and input translation, like cfmakeraw(3). Output
// processing stays on, so "\n" still moves to the start of the next line.
func rawTermios(t syscall.Termios) syscall.Termios {
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	return t
}

func makeRaw(f *os.File) (func() error, error) {
	old, err := getTermios(f)
	if err != nil {
		return nil, err
	}
	raw := rawTermios(*old)
	if err := setTermios(f, &raw); err != nil {
		return nil, err
	}
	return func() error { return setTermios(f, old) }, nil
}

func termSize(f *os.File) (int, int, error) {
	var ws struct{ Row, Col, X, Y uint16 }
	if err := ioctl(f.Fd(), syscall.TIOCGWINSZ, unsafe.Pointer(&ws)); err != nil {
		return 0, 0, err
	}
	return int(ws.Col), int(ws.Row), nil
}

// enableVT: Unix terminals understand ANSI sequences already.
func enableVT(*os.File) (func() error, error) { return nil, nil }

// watchResize forwards SIGWINCH to ch until the returned stop is called.
func watchResize(_ *os.File, ch chan struct{}) (stop func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-sig:
				select {
				case ch <- struct{}{}:
				default: // one pending resize is enough
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
	}
}

// QuitSignals are the signals that should end the TUI (and restore the
// terminal): termination, and the terminal going away.
func QuitSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} }
