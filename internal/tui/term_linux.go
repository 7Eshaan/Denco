package tui

import "syscall"

// termios ioctl requests on Linux.
const (
	ioctlGetTermios = syscall.TCGETS
	ioctlSetTermios = syscall.TCSETS
)
