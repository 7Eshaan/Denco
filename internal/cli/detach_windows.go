//go:build windows

package cli

import (
	"os/exec"
	"syscall"
)

const detachedProcess = 0x00000008 // DETACHED_PROCESS: no console

// detach starts cmd without a console window, in its own process group, so
// closing the terminal or Ctrl-C there doesn't reach it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}
