package agent

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func withPreDeploy(t *testing.T, cmd string) *Config {
	t.Helper()
	cfg := testConfig(t)
	cfg.Projects[0].PreDeploy = cmd
	return cfg
}

func (f *fakeDocker) preDeployCalls() []string {
	var out []string
	for _, c := range f.calls {
		if strings.Contains(c, " sh -c ") {
			out = append(out, c)
		}
	}
	return out
}

func TestPreDeployRunsAfterPullBeforeUp(t *testing.T) {
	cfg, docker := withPreDeploy(t, "backup.sh"), &fakeDocker{}
	a := testAgent(cfg, docker)
	if err := a.Deploy(context.Background(), "st", []string{beV1, feV1}, nil); err != nil {
		t.Fatal(err)
	}
	docker.calls = nil
	if err := a.Deploy(context.Background(), "st", []string{beV2, feV1}, nil); err != nil {
		t.Fatal(err)
	}

	// Only what changes is described: the frontend is already on c1.
	want := "env J_CICD_PROJECT=st J_CICD_SERVICES=backend J_CICD_RELEASE=backend=c2 sh -c backup.sh"
	if got := docker.preDeployCalls(); len(got) != 1 || got[0] != want {
		t.Fatalf("pre_deploy calls = %q, want [%q]", got, want)
	}
	pull := slices.IndexFunc(docker.calls, func(c string) bool { return strings.Contains(c, " pull ") })
	pre := slices.Index(docker.calls, want)
	up := slices.IndexFunc(docker.calls, func(c string) bool { return strings.Contains(c, " up -d ") })
	if !(pull < pre && pre < up) {
		t.Errorf("order pull=%d pre_deploy=%d up=%d, want pull < pre_deploy < up:\n%s", pull, pre, up, strings.Join(docker.calls, "\n"))
	}
}

func TestFailedPreDeployChangesNothing(t *testing.T) {
	cfg, docker := withPreDeploy(t, "backup.sh"), &fakeDocker{}
	a := testAgent(cfg, docker)
	if err := a.Deploy(context.Background(), "st", []string{beV1}, nil); err != nil {
		t.Fatal(err)
	}
	docker.calls, docker.failOn = nil, "sh -c backup.sh"

	err := a.Deploy(context.Background(), "st", []string{beV2}, nil)
	if err == nil || !strings.Contains(err.Error(), "pre_deploy failed") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	if ups := docker.composeCalls("up"); len(ups) != 0 {
		t.Errorf("containers were touched: %q", ups)
	}
	be := state(t, cfg).Services["backend"]
	if be.Current != beV1Digest || be.LastResult != resultPreDeployFailed {
		t.Errorf("backend state = %+v", be)
	}
}

func TestPreDeploySkippedWhenNothingChanges(t *testing.T) {
	cfg, docker := withPreDeploy(t, "backup.sh"), &fakeDocker{}
	a := testAgent(cfg, docker)
	for range 2 {
		if err := a.Deploy(context.Background(), "st", []string{beV1}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := docker.preDeployCalls(); len(got) != 1 {
		t.Errorf("pre_deploy ran %d times for one change", len(got))
	}
}

func TestRollbackRevertsOnlyTheLastDeploy(t *testing.T) {
	cfg, docker := withPreDeploy(t, "backup.sh"), &fakeDocker{}
	a := testAgent(cfg, docker)
	for _, refs := range [][]string{{beV1, feV1}, {beV2, feV1}} {
		if err := a.Deploy(context.Background(), "st", refs, nil); err != nil {
			t.Fatal(err)
		}
	}
	docker.calls = nil

	if err := a.Rollback(context.Background(), "st"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// The last deploy changed only the backend, so only it goes back;
	// the frontend's own previous release isn't touched.
	if ups := docker.composeCalls("up"); len(ups) != 1 || !strings.HasSuffix(ups[0], "up -d --no-deps backend") {
		t.Errorf("up calls = %q", ups)
	}
	if got := docker.running["BACKEND_IMAGE"]; got != beV1Digest {
		t.Errorf("backend running %s, want %s", got, beV1Digest)
	}
	// A rollback is a deploy: it takes the backup too.
	if want := "J_CICD_RELEASE=backend=c1"; len(docker.preDeployCalls()) != 1 || !strings.Contains(docker.preDeployCalls()[0], want) {
		t.Errorf("pre_deploy calls = %q, want one with %s", docker.preDeployCalls(), want)
	}
	be := state(t, cfg).Services["backend"]
	if be.Current != beV1Digest || be.CurrentTag != "c1" || be.Previous != beV2Digest || be.PreviousTag != "c2" {
		t.Errorf("backend state = %+v", be)
	}

	// Rolling back again undoes the rollback.
	if err := a.Rollback(context.Background(), "st"); err != nil {
		t.Fatalf("second Rollback: %v", err)
	}
	if got := state(t, cfg).Services["backend"].Current; got != beV2Digest {
		t.Errorf("after undoing the rollback backend = %s, want %s", got, beV2Digest)
	}
}

func TestRollbackToUnhealthyReleaseReturnsToCurrent(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{}
	a := testAgent(cfg, docker)
	for _, ref := range []string{beV1, beV2} {
		if err := a.Deploy(context.Background(), "st", []string{ref}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// v1 no longer comes up healthy (say, it can't read the new schema).
	docker.health = map[string]string{beV1Digest: "running unhealthy"}

	err := a.Rollback(context.Background(), "st")
	if err == nil || !strings.Contains(err.Error(), "rolled back to the previous release") {
		t.Fatalf("err = %v, want the failed rollback itself rolled back", err)
	}
	if got := docker.running["BACKEND_IMAGE"]; got != beV2Digest {
		t.Errorf("backend running %s, want it back on %s", got, beV2Digest)
	}
	if be := state(t, cfg).Services["backend"]; be.Current != beV2Digest || be.Previous != beV1Digest {
		t.Errorf("backend state = %+v", be)
	}
}

func TestRollbackNeedsAPreviousRelease(t *testing.T) {
	cfg, docker := testConfig(t), &fakeDocker{}
	a := testAgent(cfg, docker)
	if err := a.Rollback(context.Background(), "st"); err == nil || !strings.Contains(err.Error(), "never been deployed") {
		t.Errorf("never deployed: err = %v", err)
	}
	if err := a.Deploy(context.Background(), "st", []string{beV1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Rollback(context.Background(), "st"); err == nil || !strings.Contains(err.Error(), "first deploy") {
		t.Errorf("after first deploy: err = %v", err)
	}
	if err := a.Rollback(context.Background(), "nope"); err == nil {
		t.Error("unknown project: want an error")
	}
}

func TestRollbackOfStateWithoutCurrentSince(t *testing.T) {
	// State written before current_since existed (like simple-track's
	// live state) falls back to each service's last deploy time.
	cfg, docker := testConfig(t), &fakeDocker{}
	a := testAgent(cfg, docker)
	for _, refs := range [][]string{{beV1, feV1}, {beV2}} {
		if err := a.Deploy(context.Background(), "st", refs, nil); err != nil {
			t.Fatal(err)
		}
	}
	st := state(t, cfg)
	for _, ss := range st.Services {
		ss.CurrentSince = time.Time{}
	}
	if err := st.save(cfg.StateDir, "st"); err != nil {
		t.Fatal(err)
	}
	docker.calls = nil

	if err := a.Rollback(context.Background(), "st"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if ups := docker.composeCalls("up"); len(ups) != 1 || !strings.HasSuffix(ups[0], "up -d --no-deps backend") {
		t.Errorf("up calls = %q, want only the backend (the last deploy's service)", ups)
	}
}
