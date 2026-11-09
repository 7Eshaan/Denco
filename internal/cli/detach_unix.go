//go:build !windows

package cli

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in its own session: no controlling terminal, so
// closing the terminal (SIGHUP) or Ctrl-C there doesn't reach it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
