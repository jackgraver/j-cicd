package manager

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"j-cicd/internal/store"
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

func TestFormatSync(t *testing.T) {
	cases := []struct {
		ahead, behind int
		want          string
	}{
		{0, 0, ""},
		{1, 0, "local main is 1 commit ahead of origin/main (not pushed)"},
		{3, 0, "3 commits ahead"},
		{0, 2, "2 commits behind"},
		{1, 1, "has diverged"},
	}
	for _, tc := range cases {
		got := formatSync("main", "origin/main", tc.ahead, tc.behind)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("formatSync(%d,%d) = %q, want it to contain %q", tc.ahead, tc.behind, got, tc.want)
		}
	}
}

// releaseRepo plus a bare "origin" that main is 1 commit ahead of.
func TestUnpushedReleaseCommitWarns(t *testing.T) {
	st, target, _ := releaseRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitT(t, target.Dir, "init", "-q", "--bare", origin)
	gitT(t, target.Dir, "remote", "add", "origin", origin)
	gitT(t, target.Dir, "push", "-q", "origin", "main")
	gitT(t, target.Dir, "branch", "-q", "--set-upstream-to=origin/main", "main")
	gitT(t, target.Dir, "checkout", "-q", "main")
	write(t, filepath.Join(target.Dir, "more.txt"), "unpushed\n")
	gitT(t, target.Dir, "add", "more.txt")
	gitT(t, target.Dir, "commit", "-q", "-m", "unpushed")

	run, err := Run(context.Background(), st, target, Options{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(st.LogPath(run.ID))
	if !strings.Contains(string(log), "warning: local main is 1 commit ahead of origin/main") {
		t.Errorf("log has no unpushed warning:\n%s", log)
	}

	plan, err := MakePlan(context.Background(), st, target, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "ahead of origin/main") {
		t.Errorf("plan warnings = %q", plan.Warnings)
	}
}

func TestPlanRunsNothingAndRecordsNothing(t *testing.T) {
	st, target, mainRev := releaseRepo(t)
	plan, err := MakePlan(context.Background(), st, target, Options{Push: true, Deploy: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Branch != "main" || plan.Revision != mainRev || plan.Dirty {
		t.Errorf("plan source = %+v, want main at %s", plan, mainRev)
	}
	// The committed config's step, not the edited working copy's.
	if len(plan.Steps) != 1 || plan.Steps[0].Run != "make release-test" {
		t.Errorf("steps = %+v", plan.Steps)
	}
	if plan.Deploy == nil || plan.Deploy.Host != "vps" || len(plan.Deploy.Images) != 1 || !strings.HasSuffix(plan.Deploy.Images[0], ":"+mainRev) {
		t.Errorf("deploy = %+v", plan.Deploy)
	}
	if runs, _ := st.Runs("", 0); len(runs) != 0 {
		t.Errorf("a plan recorded runs: %+v", runs)
	}
	if wts := gitT(t, target.Dir, "worktree", "list"); strings.Count(wts, "\n") != 0 {
		t.Errorf("plan left a worktree:\n%s", wts)
	}
	if !strings.Contains(plan.Describe(), "local branch main at "+mainRev) {
		t.Errorf("Describe:\n%s", plan.Describe())
	}
}

func TestPlanReportsRefusedDeploy(t *testing.T) {
	st, target := setup(t, "name: app\nimages:\n  - repository: r.example.com/app\ndeploy:\n  host: vps\n")
	plan, err := MakePlan(context.Background(), st, target, Options{Push: true, Deploy: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Problems) != 1 || !strings.Contains(plan.Problems[0], "cannot deploy") || plan.Deploy != nil {
		t.Errorf("plan = %+v, want the deploy refused (no git revision)", plan)
	}
}

func TestPlanBadConfig(t *testing.T) {
	st, target := setup(t, "name: app\nstepz: []\n")
	if _, err := MakePlan(context.Background(), st, target, Options{}); err == nil {
		t.Fatal("want a config error")
	} else if _, ok := err.(*ConfigError); !ok {
		t.Errorf("err = %T %v, want *ConfigError", err, err)
	}
	_ = store.StatusFailed
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

// Steps see the revision the images will be tagged with, so they can
// stamp it into what they build.
func TestStepsGetRevisionInEnvironment(t *testing.T) {
	st, target, mainRev := releaseRepo(t)
	write(t, filepath.Join(target.Dir, ".cicd.yaml"), "name: app\nbranch: main\nsteps:\n  - name: stamp\n    run: echo stamped-$J_CICD_REVISION\n")
	gitT(t, target.Dir, "checkout", "-q", "main")
	gitT(t, target.Dir, "add", ".")
	gitT(t, target.Dir, "commit", "-q", "-m", "stamp step")
	mainRev = gitT(t, target.Dir, "rev-parse", "--short=12", "main")

	var echo strings.Builder
	if _, err := Run(context.Background(), st, target, Options{Echo: &echo}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(echo.String(), "stamped-"+mainRev+"\n") {
		t.Errorf("output:\n%s", echo.String())
	}
}

func TestPlanSyncFields(t *testing.T) {
	st, target, _ := releaseRepo(t)

	// No upstream: the plan says there is nothing to compare with.
	plan, err := MakePlan(context.Background(), st, target, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Upstream != "" || !strings.Contains(plan.Describe(), "no upstream configured") || !strings.Contains(plan.Describe(), "not origin's copy") {
		t.Errorf("no-upstream plan:\n%s", plan.Describe())
	}

	// In sync with origin.
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitT(t, target.Dir, "init", "-q", "--bare", origin)
	gitT(t, target.Dir, "remote", "add", "origin", origin)
	gitT(t, target.Dir, "push", "-q", "origin", "main")
	gitT(t, target.Dir, "branch", "-q", "--set-upstream-to=origin/main", "main")
	plan, _ = MakePlan(context.Background(), st, target, Options{})
	if plan.Upstream != "origin/main" || plan.Ahead != 0 || !strings.Contains(plan.Describe(), "in sync with origin/main") {
		t.Errorf("in-sync plan:\n%s", plan.Describe())
	}

	// One unpushed commit on main.
	gitT(t, target.Dir, "checkout", "-q", "main")
	write(t, filepath.Join(target.Dir, "more.txt"), "x\n")
	gitT(t, target.Dir, "add", "more.txt")
	gitT(t, target.Dir, "commit", "-q", "-m", "unpushed")
	plan, _ = MakePlan(context.Background(), st, target, Options{})
	if plan.Ahead != 1 || plan.Behind != 0 || len(plan.Warnings) != 1 {
		t.Errorf("ahead plan = %+v", plan)
	}
}

// --plan --json is one object an agent can show as it is: the embedded
// pipeline plan is flattened into it, and sync state is named.
func TestPlanJSONShape(t *testing.T) {
	st, target, mainRev := releaseRepo(t)
	plan, err := MakePlan(context.Background(), st, target, Options{Push: true, Deploy: true})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"project", "dir", "branch", "revision", "steps", "images", "deploy"} {
		if _, ok := got[key]; !ok {
			t.Errorf("plan JSON has no %q: %s", key, data)
		}
	}
	if got["revision"] != mainRev || got["branch"] != "main" {
		t.Errorf("plan JSON = %s", data)
	}
	deploy := got["deploy"].(map[string]any)
	if deploy["host"] != "vps" {
		t.Errorf("deploy = %v", deploy)
	}
}
