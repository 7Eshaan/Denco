package tui

import "syscall"

// termios ioctl requests on macOS.
const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETA
)
