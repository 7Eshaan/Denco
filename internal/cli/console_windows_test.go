//go:build windows

package cli

import "testing"

// Tests run from a shell (or with no console at all, on CI): never "a
// console to itself".
func TestOwnConsoleFromShell(t *testing.T) {
	if ownConsole() {
		t.Error("a test process sharing its shell's console looks double-clicked")
	}
}
