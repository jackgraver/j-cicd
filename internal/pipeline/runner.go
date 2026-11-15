package pipeline

import (
	"context"
	"io"
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

// ExecRunner runs commands as real child processes.
type ExecRunner struct {
	// Stdout and Stderr receive the command's output. Nil means j-cicd's
	// own stdout/stderr. Passing the same writer for both interleaves them
	// in order, which is what a run log wants.
	Stdout, Stderr io.Writer
}

// Run implements CommandRunner. Cancelling ctx (a step timeout, Ctrl-C)
// kills the command.
func (r ExecRunner) Run(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = r.Stdout, r.Stderr
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	// No stdin: a command that stops to prompt should fail rather than
	// block the runner forever.
	cmd.Stdin = nil
	setProcessGroup(cmd)
	return cmd.Run()
}
