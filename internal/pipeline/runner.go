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
	// Run executes the command with its output streamed through.
	Run(ctx context.Context, dir, name string, args ...string) error
	// Output executes the command and returns what it wrote to stdout,
	// for commands whose answer the caller needs (docker inspect, ...).
	// Stderr is still streamed through so failures stay visible.
	Output(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

// ExecRunner runs commands as real child processes, streaming their
// output straight to j-cicd's own stdout/stderr.
type ExecRunner struct{}

// Run implements CommandRunner. Cancelling ctx (a step timeout, Ctrl-C)
// kills the command.
func (ExecRunner) Run(ctx context.Context, dir, name string, args ...string) error {
	cmd := command(ctx, dir, name, args...)
	cmd.Stdout = os.Stdout
	return cmd.Run()
}

// Output implements CommandRunner.
func (ExecRunner) Output(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	return command(ctx, dir, name, args...).Output()
}

func command(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	// No stdin: a command that stops to prompt should fail rather than
	// block the runner forever.
	cmd.Stdin = nil
	setProcessGroup(cmd)
	return cmd
}
