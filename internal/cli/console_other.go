//go:build !windows

package cli

// ownConsole: on macOS and Linux a double-clicked program runs inside a
// terminal's shell like any other, so there is nothing to tell apart.
func ownConsole() bool { return false }
