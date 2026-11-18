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
	// Run executes the command with its output streamed through.
	Run(ctx context.Context, dir, name string, args ...string) error
	// Output executes the command and returns what it wrote to stdout,
	// for commands whose answer the caller needs (docker inspect, ...).
	// Stderr is still streamed through so failures stay visible.
	Output(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

// ExecRunner runs commands as real child processes.
type ExecRunner struct {
	// Stdout and Stderr receive the command's output. Nil means j-cicd's
	// own stdout/stderr. Passing the same writer for both interleaves them
	// in order, which is what a run log wants.
	Stdout, Stderr io.Writer
	// Env is added to j-cicd's own environment for every command.
	Env []string
}

// Run implements CommandRunner. Cancelling ctx (a step timeout, Ctrl-C)
// kills the command.
func (r ExecRunner) Run(ctx context.Context, dir, name string, args ...string) error {
	cmd := command(ctx, dir, name, args...)
	cmd.Stdout, cmd.Stderr = r.Stdout, r.Stderr
	r.addEnv(cmd)
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	return cmd.Run()
}

// Output implements CommandRunner. Stdout is captured and returned;
// stderr goes to r.Stderr (or j-cicd's own).
func (r ExecRunner) Output(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := command(ctx, dir, name, args...)
	cmd.Stderr = r.Stderr
	r.addEnv(cmd)
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	return cmd.Output()
}

func (r ExecRunner) addEnv(cmd *exec.Cmd) {
	if len(r.Env) > 0 {
		cmd.Env = append(os.Environ(), r.Env...)
	}
}

func command(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// No stdin: a command that stops to prompt should fail rather than
	// block the runner forever.
	cmd.Stdin = nil
	setProcessGroup(cmd)
	return cmd
}
