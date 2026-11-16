package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

// Docker is checked before the first step of any run that builds images,
// so "Docker isn't usable" is the answer up front, in the run's error,
// rather than a failed `docker build` at the end of the steps with the
// reason somewhere in the log.

const (
	dockerCheckTimeout = 30 * time.Second
	dockerStartTimeout = 90 * time.Second
)

// DockerError says why Docker can't be used and how to fix it, in one
// line, so it reads on its own in history and the dashboard.
type DockerError struct {
	Problem string
	Fix     string
}

func (e *DockerError) Error() string {
	return "Docker isn't usable: " + e.Problem + ". Fix: " + e.Fix
}

// dockerProblem classifies why the docker CLI couldn't reach a daemon.
type dockerProblem int

const (
	problemUnknown dockerProblem = iota
	problemNotInstalled
	problemNotRunning
	// problemNoAccess: the socket exists but this user may not open it,
	// and isn't in its group at all.
	problemNoAccess
	// problemStaleSession: the user *is* in the socket's group, but this
	// process started before they were added, so it doesn't have it yet.
	problemStaleSession
)

// dockerDiagnosis is what inspecting the local Docker setup found.
type dockerDiagnosis struct {
	problem dockerProblem
	socket  string
	group   string // the socket's group, for access problems
}

// checkDocker makes sure the docker CLI can reach a daemon, starting the
// daemon if it simply isn't running and that can be done without a
// password prompt.
func (p *Pipeline) checkDocker(ctx context.Context) error {
	probeErr := p.dockerReachable(ctx)
	if probeErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	diag := dockerDiagnosis{problem: problemNotInstalled}
	if !errors.Is(probeErr, exec.ErrNotFound) {
		diag = p.inspectDocker()
	}
	if diag.problem == problemNotRunning && p.startDocker(ctx) {
		return nil
	}
	return diag.err()
}

// dockerReachable asks the daemon for its version: the cheapest call
// that fails exactly when builds would.
func (p *Pipeline) dockerReachable(ctx context.Context) error {
	_, err := withDeadline(ctx, dockerCheckTimeout, func(ctx context.Context) ([]byte, error) {
		return p.runner.Output(ctx, "", "docker", "version", "--format", "{{.Server.Version}}")
	})
	return err
}

// startDocker tries `systemctl start docker` without prompting for a
// password (a background run has no one to type it), and reports whether
// Docker is reachable afterwards.
func (p *Pipeline) startDocker(ctx context.Context) bool {
	if !startsDockerOnThisOS() {
		return false
	}
	p.log.Printf("Docker daemon isn't running; trying to start it (systemctl start docker)")
	err := p.exec(ctx, dockerStartTimeout, "", "systemctl", "start", "--no-ask-password", "docker")
	if err != nil {
		p.log.Printf("couldn't start Docker without admin rights: %v", err)
		return false
	}
	if err := p.dockerReachable(ctx); err != nil {
		p.log.Printf("started Docker but still can't reach it: %v", err)
		return false
	}
	p.log.Printf("started Docker")
	return true
}

// startsDockerOnThisOS: only Linux has a systemd unit to start; elsewhere
// Docker is an app (Docker Desktop) the user starts themselves.
func startsDockerOnThisOS() bool { return runtime.GOOS == "linux" }

func (d dockerDiagnosis) err() error {
	switch d.problem {
	case problemNotInstalled:
		return &DockerError{
			Problem: "the docker command isn't installed",
			Fix:     "install Docker and make sure `docker` is on PATH",
		}
	case problemNotRunning:
		fix := "start Docker"
		if runtime.GOOS == "linux" {
			fix = "run `sudo systemctl enable --now docker` (starts it now and at every boot)"
		}
		return &DockerError{
			Problem: "the Docker daemon isn't running and j-cicd couldn't start it without a password",
			Fix:     fix,
		}
	case problemStaleSession:
		return &DockerError{
			Problem: fmt.Sprintf("permission denied on %s. You're in the %s group, but this login session started before you were added", d.socket, d.group),
			Fix:     fmt.Sprintf("reboot so the group applies (logging out isn't always enough); until then, prefix commands with `sg %s -c '...'`", d.group),
		}
	case problemNoAccess:
		return &DockerError{
			Problem: fmt.Sprintf("permission denied on %s (your user isn't in the %s group)", d.socket, d.group),
			Fix:     fmt.Sprintf("run `sudo usermod -aG %s $USER`, then reboot", d.group),
		}
	}
	return &DockerError{
		Problem: "the docker CLI can't reach a Docker daemon",
		Fix:     "see the run log or `docker info` for why",
	}
}
