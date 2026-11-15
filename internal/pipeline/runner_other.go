//go:build !unix

package pipeline

import "os/exec"

// setProcessGroup is a no-op where process groups aren't available:
// exec.CommandContext still kills the command itself on cancellation.
func setProcessGroup(cmd *exec.Cmd) {}
