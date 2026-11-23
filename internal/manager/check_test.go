package manager

import (
	"context"
	"os"
	"strings"
	"testing"

	"j-cicd/internal/store"
)

// A check on the directory runs the steps from the on-disk config and
// nothing else: no docker, no images, no deploy.
func TestCheckRunsOnlyStepsOnDisk(t *testing.T) {
	st, target, _ := releaseRepo(t)
	rr := &recordingRunner{}
	run, err := Check(context.Background(), st, target, "", Options{Runner: rr})
	if err != nil {
		t.Fatal(err)
	}
	if run.Kind != store.KindCheck || run.Status != store.StatusSuccess {
		t.Errorf("run = kind %q status %q", run.Kind, run.Status)
	}
	if len(rr.calls) != 1 || !strings.Contains(rr.calls[0].cmd, "make wip-test") {
		t.Errorf("calls = %+v, want only the on-disk step", rr.calls)
	}
	if len(run.Images) != 0 || run.Deployed {
		t.Errorf("a check published something: %+v", run)
	}
	for _, s := range run.Stages {
		if s.Kind != "step" {
			t.Errorf("unexpected stage %+v", s)
		}
	}
}

// With --ref the commit is checked out cleanly with its own committed
// config, even though a different release branch is configured.
func TestCheckRefUsesCommittedConfig(t *testing.T) {
	st, target, mainRev := releaseRepo(t)
	rr := &recordingRunner{}
	run, err := Check(context.Background(), st, target, "main", Options{Runner: rr})
	if err != nil {
		t.Fatal(err)
	}
	if run.Revision != mainRev {
		t.Errorf("revision = %q, want %q", run.Revision, mainRev)
	}
	c := rr.find(t, "sh -c make release-test")
	if !strings.Contains(strings.Join(c.files, " "), "released.txt") || strings.Contains(strings.Join(c.files, " "), "wip.txt") {
		t.Errorf("checkout held %v, want a clean main", c.files)
	}
	entries, _ := os.ReadDir(st.Root() + "/snapshots")
	if len(entries) != 0 {
		t.Errorf("snapshot left behind: %v", entries)
	}
}

func TestCheckRefMustBeACommit(t *testing.T) {
	st, target, _ := releaseRepo(t)
	for _, ref := range []string{"nope", "--upload-pack=x"} {
		if _, err := Check(context.Background(), st, target, ref, Options{Runner: &recordingRunner{}}); err == nil {
			t.Errorf("ref %q accepted", ref)
		}
	}
}

func TestCheckFailureIsStructured(t *testing.T) {
	st, target := setup(t, twoSteps)
	run, err := Check(context.Background(), st, target, "", Options{Runner: &fakeRunner{failOn: "make test"}})
	if err == nil {
		t.Fatal("want failure")
	}
	if run.Failure == nil || run.Failure.Kind != "step" || run.Failure.Stage != "test" {
		t.Errorf("failure = %+v", run.Failure)
	}
}
