package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"j-cicd/internal/store"
)

const remoteConfig = "name: app\nbranch: main\nremote: origin\nsteps:\n  - name: test\n    run: make release-test\n"

// remoteRepo is releaseRepo with a bare "origin" that has main pushed and
// remote: origin in the working config. Local main is left where it is.
func remoteRepo(t *testing.T) (st *store.Store, target Target, origin, mainRev string) {
	t.Helper()
	st, target, mainRev = releaseRepo(t)
	origin = filepath.Join(t.TempDir(), "origin.git")
	gitT(t, target.Dir, "init", "-q", "--bare", "-b", "main", origin)
	gitT(t, target.Dir, "remote", "add", "origin", origin)
	gitT(t, target.Dir, "push", "-q", "origin", "main")
	write(t, target.ConfigPath, remoteConfig)
	return st, target, origin, mainRev
}

// mergeOnOrigin advances origin's main the way a merged PR does, from a
// separate clone, so the project's own repo isn't touched.
func mergeOnOrigin(t *testing.T, origin string) (rev string) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "other")
	gitT(t, filepath.Dir(other), "clone", "-q", "-b", "main", origin, other)
	gitT(t, other, "config", "user.email", "dev@example.com")
	gitT(t, other, "config", "user.name", "dev")
	write(t, filepath.Join(other, "merged.txt"), "from a merged PR\n")
	write(t, filepath.Join(other, ".cicd.yaml"), remoteConfig)
	gitT(t, other, "add", ".")
	gitT(t, other, "commit", "-q", "-m", "merge PR")
	gitT(t, other, "push", "-q", "origin", "main")
	return gitT(t, other, "rev-parse", "--short=12", "HEAD")
}

func TestRemoteBuildsOriginNotStaleLocalBranch(t *testing.T) {
	st, target, origin, mainRev := remoteRepo(t)
	mergedRev := mergeOnOrigin(t, origin)

	r := &recordingRunner{}
	run, err := Run(context.Background(), st, target, Options{Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if run.Revision != mergedRev || run.Revision == mainRev {
		t.Errorf("revision = %s, want origin's %s (local main is %s)", run.Revision, mergedRev, mainRev)
	}
	step := r.find(t, "sh -c make release-test")
	if !strings.Contains(strings.Join(step.files, " "), "merged.txt") {
		t.Errorf("snapshot held %v, want the merged PR's files", step.files)
	}
	// Local main is untouched: nothing was pulled or fast-forwarded.
	if got := gitT(t, target.Dir, "rev-parse", "--short=12", "main"); got != mainRev {
		t.Errorf("local main moved to %s", got)
	}
	log, _ := os.ReadFile(st.LogPath(run.ID))
	if !strings.Contains(string(log), "building origin/main at "+mergedRev) {
		t.Errorf("log doesn't say what it built:\n%s", log)
	}
}

func TestRemoteWarnsAboutUnpushedCommits(t *testing.T) {
	st, target, _, _ := remoteRepo(t)
	gitT(t, target.Dir, "checkout", "-q", "main")
	write(t, filepath.Join(target.Dir, "local.txt"), "never pushed\n")
	gitT(t, target.Dir, "add", "local.txt")
	gitT(t, target.Dir, "commit", "-q", "-m", "unpushed")
	write(t, target.ConfigPath, remoteConfig) // the checkout replaced it

	plan, err := MakePlan(context.Background(), st, target, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// (origin's committed config lacks remote:, which warns as well.)
	if len(plan.Warnings) == 0 || !strings.Contains(plan.Warnings[0], "1 commit not on origin/main (unpushed)") {
		t.Errorf("warnings = %q", plan.Warnings)
	}
	if plan.Remote != "origin" || !strings.Contains(plan.Describe(), "origin/main at "+plan.Revision+" (fetched just now)") {
		t.Errorf("plan = %q", plan.Describe())
	}
}

func TestRemoteBehindIsNotAWarning(t *testing.T) {
	st, target, origin, _ := remoteRepo(t)
	mergeOnOrigin(t, origin)
	plan, err := MakePlan(context.Background(), st, target, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("warnings = %q, want none: being behind is why the remote is built", plan.Warnings)
	}
	if plan.Behind != 0 || plan.Upstream != "origin/main" {
		t.Errorf("upstream = %q behind = %d, want origin/main and no behind", plan.Upstream, plan.Behind)
	}
}

func TestRemoteFetchFailureFailsTheRun(t *testing.T) {
	st, target, _, _ := remoteRepo(t)
	gitT(t, target.Dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))

	r := &recordingRunner{}
	_, err := Run(context.Background(), st, target, Options{Runner: r})
	if err == nil || !strings.Contains(err.Error(), "fetching origin/main") {
		t.Fatalf("err = %v, want a fetch failure", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("ran %d commands after a failed fetch", len(r.calls))
	}

	plan, err := MakePlan(context.Background(), st, target, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Problems) != 1 || !strings.Contains(plan.Problems[0], "fetching origin/main") {
		t.Errorf("problems = %q", plan.Problems)
	}
}

func TestRemoteBranchMissingOnRemote(t *testing.T) {
	st, target, _, _ := remoteRepo(t)
	write(t, target.ConfigPath, strings.Replace(remoteConfig, "branch: main", "branch: prod", 1))
	_, err := Run(context.Background(), st, target, Options{Runner: &recordingRunner{}})
	if err == nil || !strings.Contains(err.Error(), `release branch "prod" doesn't exist on remote "origin"`) {
		t.Errorf("err = %v", err)
	}
}

func TestCommitPinsTheRun(t *testing.T) {
	st, target, origin, mainRev := remoteRepo(t)
	mergeOnOrigin(t, origin)

	// Planned when origin was at mainRev; it has since moved. The pinned
	// run still builds what was reviewed.
	r := &recordingRunner{}
	run, err := Run(context.Background(), st, target, Options{Commit: mainRev, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if run.Revision != mainRev {
		t.Errorf("revision = %s, want the pinned %s", run.Revision, mainRev)
	}
	step := r.find(t, "sh -c make release-test")
	if strings.Contains(strings.Join(step.files, " "), "merged.txt") {
		t.Errorf("snapshot held %v, want the pinned commit's files only", step.files)
	}

	plan, err := MakePlan(context.Background(), st, target, Options{Commit: mainRev})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Revision != mainRev || !strings.Contains(plan.Describe(), "--commit "+mainRev) {
		t.Errorf("plan = %q", plan.Describe())
	}
}

func TestCommitMustBeInReleaseHistory(t *testing.T) {
	st, target, _, _ := remoteRepo(t)
	featureRev := gitT(t, target.Dir, "rev-parse", "--short=12", "feature")

	for _, tc := range []struct{ commit, want string }{
		{featureRev, "is not part of origin/main's history"},
		{"deadbeefdeadbeef", "is not a commit"},
		{"--output=x", "is not a commit"},
	} {
		_, err := Run(context.Background(), st, target, Options{Commit: tc.commit, Runner: &recordingRunner{}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("--commit %s: err = %v, want %q", tc.commit, err, tc.want)
		}
	}
}

func TestCommitNeedsReleaseBranch(t *testing.T) {
	st, target, _ := releaseRepo(t)
	write(t, target.ConfigPath, "name: app\nsteps:\n  - run: make\n")
	_, err := Run(context.Background(), st, target, Options{Commit: "abc", Runner: &recordingRunner{}})
	if err == nil || !strings.Contains(err.Error(), "--commit needs branch:") {
		t.Errorf("err = %v", err)
	}
	plan, err := MakePlan(context.Background(), st, target, Options{Commit: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Problems) != 1 || !strings.Contains(plan.Problems[0], "--commit needs branch:") {
		t.Errorf("problems = %q", plan.Problems)
	}
}

func TestCommitWithLocalBranchAndRefNames(t *testing.T) {
	st, target, mainRev := releaseRepo(t)
	gitT(t, target.Dir, "checkout", "-q", "main")
	write(t, filepath.Join(target.Dir, "two.txt"), "second\n")
	gitT(t, target.Dir, "add", "two.txt")
	gitT(t, target.Dir, "commit", "-q", "-m", "two")
	write(t, target.ConfigPath, releaseConfig)

	// A short sha and a ref name both pin to the first commit.
	for _, pin := range []string{mainRev[:7], "HEAD~1"} {
		run, err := Run(context.Background(), st, target, Options{Commit: pin, Runner: &recordingRunner{}})
		if err != nil {
			t.Fatalf("--commit %s: %v", pin, err)
		}
		if run.Revision != mainRev {
			t.Errorf("--commit %s: revision = %s, want %s", pin, run.Revision, mainRev)
		}
	}
	// Without --commit the plan still prints how to pin.
	plan, err := MakePlan(context.Background(), st, target, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Describe(), "pin:     j-cicd run app --commit "+plan.Revision) {
		t.Errorf("plan = %q", plan.Describe())
	}
}

func TestRemoteOnlyInWorkingConfigWarns(t *testing.T) {
	st, target, _, _ := remoteRepo(t)
	// origin/main's committed config has no remote:, only the working copy does.
	run, err := Run(context.Background(), st, target, Options{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(st.LogPath(run.ID))
	if !strings.Contains(string(log), `doesn't say remote: "origin"`) {
		t.Errorf("no warning about the uncommitted remote: setting:\n%s", log)
	}
}
