//go:build windows

package cli

import (
	"syscall"
	"unsafe"
)

var procGetConsoleProcessList = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// ownConsole reports whether Denco is the only process attached to its
// console: Windows made a new console window just for it, which happens
// when it is double-clicked in Explorer (or started from a shortcut). Run
// from cmd.exe, PowerShell or Windows Terminal, the shell shares the
// console, so there are at least two.
func ownConsole() bool {
	var ids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&ids[0])), uintptr(len(ids)))
	return n == 1
}
