package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"j-cicd/internal/config"
)

// dockerCheck is the reachability probe every image-building run starts
// with.
const dockerCheck = "docker version --format {{.Server.Version}}"

// dockerRunner fails the docker probe until "systemctl start" succeeds
// (if it's allowed to), like a stopped daemon.
type dockerRunner struct {
	fakeRunner
	probeErr  error
	canStart  bool
	started   bool
	stepsSeen int
}

func (d *dockerRunner) Run(ctx context.Context, dir, name string, args ...string) error {
	if name == "systemctl" {
		d.fakeRunner.Run(ctx, dir, name, args...)
		if !d.canStart {
			return errors.New("exit status 1")
		}
		d.started = true
		return nil
	}
	if name == "sh" {
		d.stepsSeen++
	}
	return d.fakeRunner.Run(ctx, dir, name, args...)
}

func (d *dockerRunner) Output(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	d.fakeRunner.Run(ctx, dir, name, args...)
	if d.started {
		return []byte("28.0.0\n"), nil
	}
	return nil, d.probeErr
}

func imageConfig(t *testing.T) *config.Config {
	return loaded(t, &config.Config{
		Name:   "demo",
		Steps:  []config.Step{{Name: "test", Run: "make test"}},
		Images: []config.Image{{Repository: "example.com/demo", Tags: []string{"latest"}}},
	})
}

func runWithDocker(t *testing.T, runner *dockerRunner, diag dockerDiagnosis) error {
	t.Helper()
	p := testPipeline("/proj", runner, false)
	p.inspectDocker = func() dockerDiagnosis { return diag }
	return p.Run(context.Background(), imageConfig(t))
}

func TestDockerProblemsFailBeforeAnyStep(t *testing.T) {
	cases := []struct {
		name string
		diag dockerDiagnosis
		err  error
		want string // in the run's error
	}{
		{"not installed", dockerDiagnosis{}, fmt.Errorf("exec: %w", exec.ErrNotFound), "isn't installed"},
		{"not in group", dockerDiagnosis{problem: problemNoAccess, socket: "/var/run/docker.sock", group: "docker"}, errors.New("exit status 1"), "usermod -aG docker"},
		{"stale session", dockerDiagnosis{problem: problemStaleSession, socket: "/var/run/docker.sock", group: "docker"}, errors.New("exit status 1"), "reboot"},
		{"not running, can't start", dockerDiagnosis{problem: problemNotRunning, socket: "/var/run/docker.sock"}, errors.New("exit status 1"), "isn't running"},
		{"unknown", dockerDiagnosis{problem: problemUnknown}, errors.New("exit status 1"), "can't reach a Docker daemon"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &dockerRunner{probeErr: tc.err}
			err := runWithDocker(t, runner, tc.diag)
			var dockerErr *DockerError
			if !errors.As(err, &dockerErr) {
				t.Fatalf("err = %v, want a DockerError", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to mention %q", err, tc.want)
			}
			// One line, so it reads whole in history and the dashboard.
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("multi-line error: %q", err)
			}
			if runner.stepsSeen != 0 {
				t.Errorf("ran %d step(s) despite Docker being unusable", runner.stepsSeen)
			}
		})
	}
}

func TestStoppedDockerIsStartedThenRunContinues(t *testing.T) {
	if !startsDockerOnThisOS() {
		t.Skip("starting Docker is only attempted on Linux")
	}
	runner := &dockerRunner{probeErr: errors.New("exit status 1"), canStart: true}
	if err := runWithDocker(t, runner, dockerDiagnosis{problem: problemNotRunning}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertCalls(t, runner.calls, []string{
		dockerCheck,
		"systemctl start --no-ask-password docker",
		dockerCheck,
		"sh -c make test",
		"docker build -f /proj/Dockerfile -t example.com/demo:latest /proj",
	})
}

func TestDockerNotCheckedWithoutImages(t *testing.T) {
	runner := &dockerRunner{probeErr: errors.New("exit status 1")}
	p := testPipeline("/proj", runner, false)
	p.inspectDocker = func() dockerDiagnosis { t.Fatal("inspected Docker for a run with no images"); return dockerDiagnosis{} }
	cfg := loaded(t, &config.Config{Name: "demo", Steps: []config.Step{{Run: "make test"}}})
	if err := p.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertCalls(t, runner.calls, []string{"sh -c make test"})
}
