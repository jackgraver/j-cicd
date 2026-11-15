//go:build unix

package pipeline

import (
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroup puts the child in its own process group and kills that
// whole group on cancellation. Every step is a `sh -c` wrapper, so
// signalling only the shell would leave the real work — a test binary, a
// docker build — running after the timeout fired.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Don't hang indefinitely on a grandchild still holding the output
	// streams open after the group has been killed.
	cmd.WaitDelay = 5 * time.Second
}
