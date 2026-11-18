package manager

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestTailWriterKeepsLastLines(t *testing.T) {
	tw := newTail(3)
	tw.Write([]byte("a\nb\nc\nd"))
	tw.Write([]byte("e\nf\r\ng"))
	got := strings.Join(tw.Lines(), "|")
	if want := "de|f|g"; got != want {
		t.Errorf("Lines = %q, want %q", got, want)
	}
}

const noisyFailure = `name: app
steps:
  - name: setup
    run: echo layer1
  - name: build
    run: for i in $(seq 1 100); do echo noise$i; done; echo the-real-error >&2; exit 3
`

// A failed run in concise mode shows stage lines and the end of the
// failing command's output, not the hundred lines before it; the log
// still has everything.
func TestProgressShowsStagesAndFailureTailOnly(t *testing.T) {
	st, target := setup(t, noisyFailure)
	var progress strings.Builder
	run, err := Run(context.Background(), st, target, Options{Progress: &progress})
	if err == nil {
		t.Fatal("want the build step to fail")
	}
	got := progress.String()
	for _, want := range []string{"run " + run.ID + " started", "• step setup", "• step build", "the-real-error", "noise100", "j-cicd logs " + run.ID} {
		if !strings.Contains(got, want) {
			t.Errorf("progress missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"layer1", "noise1\n", "noise50\n"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("progress contains %q, which is noise:\n%s", unwanted, got)
		}
	}
	log, _ := os.ReadFile(st.LogPath(run.ID))
	if !strings.Contains(string(log), "noise1\n") || !strings.Contains(string(log), "layer1") {
		t.Errorf("the run log lost output:\n%s", log)
	}
}

func TestProgressSaysNothingExtraOnSuccess(t *testing.T) {
	st, target := setup(t, twoSteps)
	var progress strings.Builder
	if _, err := Run(context.Background(), st, target, Options{Progress: &progress, Runner: &fakeRunner{}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(progress.String(), "last") || !strings.Contains(progress.String(), "• step test") {
		t.Errorf("progress = %q", progress.String())
	}
}

func TestEchoWinsOverProgress(t *testing.T) {
	st, target := setup(t, twoSteps)
	var echo, progress strings.Builder
	if _, err := Run(context.Background(), st, target, Options{Echo: &echo, Progress: &progress, Runner: &fakeRunner{}}); err != nil {
		t.Fatal(err)
	}
	if progress.Len() != 0 {
		t.Errorf("progress = %q, want nothing when everything is echoed", progress.String())
	}
}

func TestRunRecordsPushedImagesAndDeploy(t *testing.T) {
	st, target := setup(t, `name: app
images:
  - name: web
    repository: registry.example.com/app
deploy:
  host: vps
`)
	// Not a git repo, so there's no per-commit tag to deploy: only the
	// push is recorded.
	run, err := Run(context.Background(), st, target, Options{Push: true, Runner: &fakeRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetRun(run.ID)
	if len(got.Images) != 1 || got.Images[0].Repository != "registry.example.com/app" || got.Deployed {
		t.Errorf("recorded run = %+v", got)
	}
}

// Output of an earlier, passing step isn't part of a later step's failure.
func TestFailureTailIsOnlyTheFailingStage(t *testing.T) {
	st, target := setup(t, "name: app\nsteps:\n  - name: one\n    run: echo from-step-one\n  - name: two\n    run: echo from-step-two; exit 1\n")
	var progress strings.Builder
	if _, err := Run(context.Background(), st, target, Options{Progress: &progress}); err == nil {
		t.Fatal("want failure")
	}
	if strings.Contains(progress.String(), "from-step-one") || !strings.Contains(progress.String(), "from-step-two") {
		t.Errorf("progress:\n%s", progress.String())
	}
}
