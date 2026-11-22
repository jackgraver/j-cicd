package pipeline

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"j-cicd/internal/config"
)

func demoCfg(t *testing.T) *config.Config {
	return loaded(t, &config.Config{
		Name: "demo",
		Steps: []config.Step{
			{Name: "build", Run: "make build"},
			{Name: "test", Run: "make test"},
		},
		Images: []config.Image{{Repository: "example.com/demo"}},
	})
}

func TestStagesRecordedOnSuccess(t *testing.T) {
	p := testPipeline("/proj", &fakeRunner{}, true)
	if err := p.Run(context.Background(), demoCfg(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var got []string
	for _, s := range p.Stages() {
		if s.Status != StageOK || s.ExitCode != -1 || s.TimedOut {
			t.Errorf("stage %s = %+v, want ok with no exit code", s.Name, s)
		}
		got = append(got, s.Kind+":"+s.Name)
	}
	assertCalls(t, got, []string{"docker-check:docker", "step:build", "step:test", "build:example.com/demo", "push:example.com/demo"})
}

func TestStageErrorCarriesExitCodeAndStage(t *testing.T) {
	exit3 := exec.Command("sh", "-c", "exit 3").Run()
	runner := &fakeRunner{failOn: "make test", err: exit3}
	p := testPipeline("/proj", runner, true)

	err := p.Run(context.Background(), demoCfg(t))
	var se *StageError
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not a StageError", err)
	}
	if se.Kind != KindStep || se.Stage != "test" || se.ExitCode != 3 || se.TimedOut {
		t.Errorf("StageError = %+v, want step/test exit 3", se)
	}
	// The message is what runs have always said.
	if want := `step "test" failed: exit status 3`; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	if !errors.Is(err, exit3) {
		t.Error("the underlying error should still be reachable with errors.Is")
	}

	stages := p.Stages()
	last := stages[len(stages)-1]
	if last.Name != "test" || last.Status != StageFailed || last.ExitCode != 3 {
		t.Errorf("last stage = %+v, want test failed with exit 3", last)
	}
	if stages[len(stages)-2].Status != StageOK {
		t.Errorf("the stage before the failure = %+v, want ok", stages[len(stages)-2])
	}
}

func TestStageErrorMarksTimeout(t *testing.T) {
	cfg := loaded(t, &config.Config{
		Name:  "demo",
		Steps: []config.Step{{Name: "hang", Run: "sleep forever", Timeout: &config.Duration{Duration: 10 * time.Millisecond}}},
	})
	p := testPipeline("/proj", &fakeRunner{block: true}, true)

	err := p.Run(context.Background(), cfg)
	var se *StageError
	if !errors.As(err, &se) || !se.TimedOut || se.ExitCode != -1 {
		t.Fatalf("error = %#v, want a timed-out StageError with no exit code", err)
	}
	if s := p.Stages()[0]; !s.TimedOut || s.Status != StageFailed {
		t.Errorf("stage = %+v, want failed and timed out", s)
	}
}

func TestBuildFailureIsAttributedToTheImage(t *testing.T) {
	runner := &fakeRunner{failOn: "docker build"}
	err := testPipeline("/proj", runner, true).Run(context.Background(), demoCfg(t))
	var se *StageError
	if !errors.As(err, &se) || se.Kind != KindBuild || se.Stage != "example.com/demo" {
		t.Fatalf("error = %#v, want a build failure of image example.com/demo", err)
	}
}
