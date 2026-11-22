package manager

import (
	"context"
	"os"
	"strings"
	"testing"

	"j-cicd/internal/store"
)

// A step that prints colour codes and a progress bar, then fails with exit 3.
const colourFailure = `name: app
steps:
  - name: setup
    run: echo ready
  - name: test
    run: |
      printf '\033[31mFAIL\033[0m TestThing\n'
      printf 'downloading 10%%\rdownloading 60%%\rdownloading 100%%\n'
      exit 3
`

func TestFailureIsRecordedWithStageExitCodeAndCleanTail(t *testing.T) {
	st, target := setup(t, colourFailure)
	// Quiet mode: no Echo, no Progress. The tail must be kept regardless.
	run, err := Run(context.Background(), st, target, Options{})
	if err == nil {
		t.Fatal("want failure")
	}
	got, err := st.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	f := got.Failure
	if f == nil {
		t.Fatal("failed run has no failure record")
	}
	if f.Kind != "step" || f.Stage != "test" || f.ExitCode == nil || *f.ExitCode != 3 || f.TimedOut {
		t.Errorf("failure = %+v, want step/test exit 3", f)
	}
	if f.Message != got.Error {
		t.Errorf("failure message %q != run error %q", f.Message, got.Error)
	}
	if want := []string{"FAIL TestThing", "downloading 100%"}; strings.Join(f.Tail, "|") != strings.Join(want, "|") {
		t.Errorf("tail = %q, want %q (no colour codes, one progress frame, nothing from the other step)", f.Tail, want)
	}

	var names []string
	for _, s := range got.Stages {
		names = append(names, s.Kind+":"+s.Name+":"+s.Status)
	}
	if want := "step:setup:ok step:test:failed"; strings.Join(names, " ") != want {
		t.Errorf("stages = %v, want %s", names, want)
	}
}

func TestSuccessHasStagesAndNoFailure(t *testing.T) {
	st, target := setup(t, twoSteps)
	run, err := Run(context.Background(), st, target, Options{Runner: &fakeRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetRun(run.ID)
	if got.Failure != nil || len(got.Stages) != 2 || got.Stages[1].Status != "ok" {
		t.Errorf("run = failure %+v, stages %+v", got.Failure, got.Stages)
	}
}

func TestBadConfigFailureKind(t *testing.T) {
	st, target := setup(t, "name: app\nbogus: 1\n")
	run, _ := Run(context.Background(), st, target, Options{Runner: &fakeRunner{}})
	got, _ := st.GetRun(run.ID)
	if got.Failure == nil || got.Failure.Kind != store.FailureConfig || got.Failure.Stage != "" {
		t.Errorf("failure = %+v, want kind config", got.Failure)
	}
}

func TestRunLogHasStageMarkers(t *testing.T) {
	st, target := setup(t, twoSteps)
	run, err := Run(context.Background(), st, target, Options{Runner: &fakeRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(st.LogPath(run.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\n== stage step build ==\n", "\n== stage step test ==\n"} {
		if !strings.Contains("\n"+string(data), want) {
			t.Errorf("log lacks %q:\n%s", want, data)
		}
	}
}

func TestCleanLine(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                                "plain",
		"\x1b[1;32mok\x1b[0m done":             "ok done",
		"a 1%\rb 50%\rc 100%":                  "c 100%",
		"x\r":                                  "x",
		"bar\r\x1b[2K":                         "bar",
		"\x1b]8;;http://x\x07link\x1b]8;;\x07": "link",
	} {
		if got := CleanLine(in); got != want {
			t.Errorf("CleanLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// A run in progress shows the stages it has reached, not only at the end.
func TestStagesAreSavedWhileTheRunIsGoing(t *testing.T) {
	st, target := setup(t, twoSteps)
	var midRun []store.RunStage
	runner := &hookRunner{fakeRunner: fakeRunner{}, onRun: func(cmd string) {
		if strings.Contains(cmd, "make test") {
			runs, _ := st.Runs(target.Name, 1)
			midRun = runs[0].Stages
		}
	}}
	if _, err := Run(context.Background(), st, target, Options{Runner: runner}); err != nil {
		t.Fatal(err)
	}
	if len(midRun) != 2 || midRun[0].Status != "ok" || midRun[1].Name != "test" || midRun[1].Status != "running" {
		t.Errorf("stages mid-run = %+v, want build ok then test running", midRun)
	}
	runs, _ := st.Runs(target.Name, 1)
	if got := runs[0].Stages; len(got) != 2 || got[1].Status != "ok" {
		t.Errorf("final stages = %+v, want both ok and none still running", got)
	}
}

type hookRunner struct {
	fakeRunner
	onRun func(cmd string)
}

func (h *hookRunner) Run(ctx context.Context, dir, name string, args ...string) error {
	h.onRun(strings.Join(append([]string{name}, args...), " "))
	return h.fakeRunner.Run(ctx, dir, name, args...)
}

func TestStageMarkerText(t *testing.T) {
	for _, c := range []struct{ kind, name, want string }{
		{"step", "test", "step test"}, {"artifacts", "artifacts", "artifacts"}, {"preflight", "preflight", "preflight"},
	} {
		if got := StageMarkerText(c.kind, c.name); got != c.want {
			t.Errorf("StageMarkerText(%q, %q) = %q, want %q", c.kind, c.name, got, c.want)
		}
	}
}
