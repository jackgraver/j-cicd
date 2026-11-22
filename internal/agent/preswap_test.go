package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"j-cicd/internal/config"
)

func withPreSwap(t *testing.T, timeout time.Duration) *Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Projects[0].PreDeploy = "backup.sh"
	cfg.Projects[0].PreSwap = &PreSwap{Service: "backend", Command: []string{"/app/migrate", "up"}}
	if timeout > 0 {
		cfg.Projects[0].PreSwap.Timeout = &config.Duration{Duration: timeout}
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func (f *fakeDocker) preSwapCalls() []string {
	return f.composeCalls("run")
}

// steps reduces calls to the deploy's milestones, in order.
func (f *fakeDocker) steps() []string {
	var out []string
	for _, c := range f.calls {
		switch {
		case strings.Contains(c, " compose ") && strings.Contains(c, " pull "):
			out = append(out, "pull")
		case strings.Contains(c, " sh -c "):
			out = append(out, "pre_deploy")
		case strings.Contains(c, " compose ") && strings.Contains(c, " run "):
			out = append(out, "pre_swap")
		case strings.Contains(c, " up -d "):
			out = append(out, "up")
		case strings.HasPrefix(c, "docker stop "):
			out = append(out, "stop")
		case strings.HasPrefix(c, "docker rm -f "):
			out = append(out, "rm")
		}
	}
	return out
}

func TestPreSwapRunsInTheNewImageBetweenBackupAndSwap(t *testing.T) {
	cfg, docker := withPreSwap(t, 0), &fakeDocker{}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1, feV1}, "")
	docker.calls = nil
	deploy(t, a, []string{beV2, feV1}, "")

	if got, want := docker.steps(), []string{"pull", "pre_deploy", "pre_swap", "up"}; !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
	runs := docker.preSwapCalls()
	if len(runs) != 1 {
		t.Fatalf("run calls = %q", runs)
	}
	run := runs[0]
	// The new backend image via its image_var, the others pinned, and the
	// deploy's own compose and env files.
	for _, want := range []string{
		"env BACKEND_IMAGE=" + beV2Digest + " FRONTEND_IMAGE=" + feV1Digest + " docker compose -f /opt/st/compose.yml --env-file /opt/st/.env run --rm --no-deps -T --name st-pre-swap-",
		" backend /app/migrate up",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("run = %q, want it to contain %q", run, want)
		}
	}
}

func TestFailedPreSwapChangesNothing(t *testing.T) {
	cfg, docker := withPreSwap(t, 0), &fakeDocker{}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1}, "")
	docker.calls, docker.failOn = nil, "/app/migrate up"

	err := a.Deploy(context.Background(), "st", []string{beV2}, nil)
	if err == nil || !strings.Contains(err.Error(), "pre_swap failed") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	if got, want := docker.steps(), []string{"pull", "pre_deploy", "pre_swap"}; !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v (no swap, no rm for a plain failure)", got, want)
	}
	ss := state(t, cfg).Services["backend"]
	if ss.Current != beV1Digest || ss.LastResult != resultPreSwapFailed || ss.LastAttempt != beV2 {
		t.Errorf("backend state = %+v", ss)
	}
}

func TestPreSwapTimeoutStopsItsContainerAndChangesNothing(t *testing.T) {
	cfg, docker := withPreSwap(t, 20*time.Millisecond), &fakeDocker{}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1}, "")
	docker.calls, docker.hangOn = nil, "/app/migrate up"

	err := a.Deploy(context.Background(), "st", []string{beV2}, nil)
	if err == nil || !strings.Contains(err.Error(), "timed out after 20ms") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	// Stopped gracefully (SIGTERM first), so the command can cancel work
	// the database would otherwise carry on with after a SIGKILL.
	if got, want := docker.steps(), []string{"pull", "pre_deploy", "pre_swap", "stop"}; !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
	// The container stopped is the one that was started.
	run, stop := docker.preSwapCalls()[0], docker.calls[len(docker.calls)-1]
	name := strings.TrimPrefix(stop, "docker stop -t 30 ")
	if !strings.HasPrefix(name, "st-pre-swap-") || !strings.Contains(run, "--name "+name+" ") {
		t.Errorf("stop = %q for run %q", stop, run)
	}
	if got := docker.running["BACKEND_IMAGE"]; got != beV1Digest {
		t.Errorf("backend = %s, want still %s", got, beV1Digest)
	}
}

func TestPreSwapSkippedWhenItsServiceIsNotChanging(t *testing.T) {
	cfg, docker := withPreSwap(t, 0), &fakeDocker{}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1, feV1}, "")
	docker.calls = nil
	deploy(t, a, []string{beV1, "ghcr.io/o/fe:c2"}, "")
	if runs := docker.preSwapCalls(); len(runs) != 0 {
		t.Errorf("run = %q, want none: backend isn't changing", runs)
	}
	if ups := docker.composeCalls("up"); len(ups) != 1 {
		t.Errorf("up = %q", ups)
	}
}

func TestPreSwapIsNotRepeatedOrUndoneByTheAutomaticRollback(t *testing.T) {
	cfg := withPreSwap(t, 0)
	docker := &fakeDocker{health: map[string]string{beV2Digest: "running unhealthy"}}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1}, "")
	docker.calls = nil

	if err := a.Deploy(context.Background(), "st", []string{beV2}, nil); err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	if got, want := docker.steps(), []string{"pull", "pre_deploy", "pre_swap", "up", "pull", "up"}; !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

func TestManualRollbackRunsPreSwapInTheOlderImage(t *testing.T) {
	// A manual rollback is an ordinary deploy of the previous release, so
	// the hook runs in that image (for migrations: a no-op, the database
	// being ahead of it).
	cfg, docker := withPreSwap(t, 0), &fakeDocker{}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1}, "")
	deploy(t, a, []string{beV2}, "")
	docker.calls = nil

	if err := a.Rollback(context.Background(), "st"); err != nil {
		t.Fatal(err)
	}
	runs := docker.preSwapCalls()
	if len(runs) != 1 || !strings.Contains(runs[0], "BACKEND_IMAGE="+beV1Digest+" ") {
		t.Errorf("run = %q, want one in the v1 image", runs)
	}
}

func TestPreSwapUsesTheReleaseComposeFile(t *testing.T) {
	cfg, docker := withPreSwap(t, 0), &fakeDocker{}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1}, "v1")
	docker.calls = nil
	deploy(t, a, []string{beV2}, "v2")
	if runs := docker.preSwapCalls(); len(runs) != 1 || !usesCompose(cfg, runs[0], "v2") {
		t.Errorf("run = %q, want the new release's compose file", runs)
	}
}

func TestPreSwapContainerIsRemovedIfItWontStop(t *testing.T) {
	cfg, docker := withPreSwap(t, 20*time.Millisecond), &fakeDocker{}
	a := testAgent(cfg, docker)
	deploy(t, a, []string{beV1}, "")
	docker.calls, docker.hangOn, docker.failOn = nil, "/app/migrate up", "docker stop "

	if err := a.Deploy(context.Background(), "st", []string{beV2}, nil); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if got, want := docker.steps(), []string{"pull", "pre_deploy", "pre_swap", "stop", "rm"}; !slices.Equal(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}
