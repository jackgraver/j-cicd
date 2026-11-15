package pipeline

import (
	"context"
	"os"
	"os/exec"
)

// CommandRunner executes one external command. The pipeline reaches the
// outside world only through this interface, which is what keeps step
// ordering, stop-at-first-failure and --no-push handling testable without
// a real shell or a live docker daemon.
type CommandRunner interface {
	Run(ctx context.Context, dir, name string, args ...string) error
}

// ExecRunner runs commands as real child processes, streaming their
// output straight to j-cicd's own stdout/stderr.
type ExecRunner struct{}

// Run implements CommandRunner. Cancelling ctx (a step timeout, Ctrl-C)
// kills the command.
func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// No stdin: a command that stops to prompt should fail rather than
	// block the runner forever.
	cmd.Stdin = nil
	setProcessGroup(cmd)
	return cmd.Run()
}
