//go:build unix

package manager

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in its own session, so it isn't killed along with the
// terminal or the UI that launched it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// signalCancel asks the process executing a run to stop. j-cicd handles
// SIGTERM by cancelling the run, killing its steps, and recording it as
// cancelled.
func signalCancel(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
