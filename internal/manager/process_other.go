//go:build !unix

package manager

import (
	"os"
	"os/exec"
)

func detach(cmd *exec.Cmd) {}

// signalCancel kills the process outright where there's no SIGTERM; the
// run is then recorded as interrupted rather than cancelled.
func signalCancel(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
