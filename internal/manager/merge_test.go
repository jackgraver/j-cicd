package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"j-cicd/internal/store"
)

const mergeConfig = `name: app
branch: main
steps:
  - name: test
    run: make test
`

// prRepo builds an origin with main and a PR (#1) branching from it, and a
// clone of it as the project checkout, left on main with no changes.
func prRepo(t *testing.T) (st *store.Store, target Target, origin string) {
	t.Helper()
	// commit-tree needs an identity, and the tests mustn't read the real one.
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	oldRepo := githubRepoFor
	t.Cleanup(func() { githubRepoFor = oldRepo })
	githubRepoFor = func(string) (string, string, bool) { return "owner", "app", true }

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin = filepath.Join(t.TempDir(), "origin.git")
	gitT(t, t.TempDir(), "init", "-q", "--bare", "-b", "main", origin)
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "remote", "add", "origin", origin)
	write(t, filepath.Join(dir, ".cicd.yaml"), mergeConfig)
	write(t, filepath.Join(dir, "base.txt"), "base\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "base")
	gitT(t, dir, "push", "-q", "origin", "main")

	gitT(t, dir, "checkout", "-q", "-b", "feature")
	write(t, filepath.Join(dir, "feature.txt"), "from the PR\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "feature")
	gitT(t, dir, "push", "-q", "origin", "feature:refs/pull/1/head")
	gitT(t, dir, "checkout", "-q", "main")
	return st, Target{Name: "app", Dir: dir, ConfigPath: filepath.Join(dir, ".cicd.yaml")}, origin
}

// fakeGH answers gh like GitHub would for PR #1 of owner/app: `pr view`
// returns pr (its head is origin's refs/pull/1/head unless pr sets one) and
// `repo view` says the default branch is main. It fails any call not pinned
// to the repository with -R.
func fakeGH(t *testing.T, pr prInfo, origin string) {
	t.Helper()
	old := ghOutput
	t.Cleanup(func() { ghOutput = old })
	ghOutput = func(ctx context.Context, dir string, args ...string) (string, error) {
		if !strings.Contains(strings.Join(args, " "), "-R owner/app") {
			return "", fmt.Errorf("gh %v is not pinned to the origin repo", args)
		}
		switch args[0] + " " + args[1] {
		case "repo view":
			return "main", nil
		case "pr view":
			if pr.HeadRefOid == "" {
				pr.HeadRefOid = gitT(t, origin, "rev-parse", "refs/pull/1/head")
			}
			b, _ := json.Marshal(pr)
			return string(b), nil
		}
		return "", fmt.Errorf("unexpected gh %v", args)
	}
}

var openPR = prInfo{Title: "Add feature", State: "OPEN", BaseRefName: "main"}

func originMain(t *testing.T, origin string) string {
	return gitT(t, origin, "rev-parse", "refs/heads/main")
}

func TestMergePRTestsMergeCommitThenPushesIt(t *testing.T) {
	st, target, origin := prRepo(t)
	fakeGH(t, openPR, origin)
	before := originMain(t, origin)
	r := &recordingRunner{}

	run, err := MergePR(context.Background(), st, target, 1, false, Options{Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	// The step ran on the merge: the PR's file next to main's.
	step := r.find(t, "sh -c make test")
	got := strings.Join(step.files, " ")
	if !strings.Contains(got, "feature.txt") || !strings.Contains(got, "base.txt") {
		t.Errorf("snapshot held %v, want PR and main files", step.files)
	}
	if run.Kind != store.KindMerge || run.PR != 1 || !run.PRMerged {
		t.Errorf("run = %+v", run)
	}
	after := originMain(t, origin)
	if after == before || after != run.MergedSHA || !strings.HasPrefix(after, run.Revision) {
		t.Errorf("origin main = %s (was %s), run merged %s rev %s", after, before, run.MergedSHA, run.Revision)
	}
	if parents := gitT(t, origin, "rev-list", "--parents", "-n1", after); len(strings.Fields(parents)) != 3 {
		t.Errorf("not a merge commit: %s", parents)
	}
	// The user's checkout is untouched, and no scratch refs are left.
	if _, err := os.Stat(filepath.Join(target.Dir, "feature.txt")); err == nil {
		t.Error("PR file appeared in the user's checkout")
	}
	if refs := gitT(t, target.Dir, "for-each-ref", "refs/j-cicd"); refs != "" {
		t.Errorf("leftover refs: %s", refs)
	}
}

func TestMergePRFailingCIDoesNotPush(t *testing.T) {
	st, target, origin := prRepo(t)
	fakeGH(t, openPR, origin)
	before := originMain(t, origin)
	run, err := MergePR(context.Background(), st, target, 1, false, Options{Runner: &fakeRunner{failOn: "make test"}})
	if err == nil || run.PRMerged {
		t.Fatalf("err = %v, run = %+v", err, run)
	}
	if originMain(t, origin) != before {
		t.Error("main moved after failed CI")
	}
}

func TestMergePRNoMergeStopsBeforePush(t *testing.T) {
	st, target, origin := prRepo(t)
	fakeGH(t, openPR, origin)
	before := originMain(t, origin)
	run, err := MergePR(context.Background(), st, target, 1, true, Options{Runner: &fakeRunner{}})
	if err != nil || run.PRMerged || originMain(t, origin) != before {
		t.Fatalf("err = %v, run = %+v", err, run)
	}
}

func TestMergePRRefusesWhatItCannotMerge(t *testing.T) {
	cases := map[string]struct {
		pr   prInfo
		want string
	}{
		"closed":      {prInfo{State: "CLOSED", BaseRefName: "main"}, "not open"},
		"fork":        {prInfo{State: "OPEN", BaseRefName: "main", IsCrossRepository: true}, "fork"},
		"wrongbase":   {prInfo{State: "OPEN", BaseRefName: "develop"}, "targets develop"},
		"draft":       {prInfo{State: "OPEN", BaseRefName: "main", IsDraft: true}, "draft"},
		"changes":     {prInfo{State: "OPEN", BaseRefName: "main", ReviewDecision: "CHANGES_REQUESTED"}, "isn't approved"},
		"unreviewed":  {prInfo{State: "OPEN", BaseRefName: "main", ReviewDecision: "REVIEW_REQUIRED"}, "isn't approved"},
		"conflicting": {prInfo{State: "OPEN", BaseRefName: "main", Mergeable: "CONFLICTING"}, "conflicts"},
		"headmoved":   {prInfo{State: "OPEN", BaseRefName: "main", HeadRefOid: "0123456789abcdef0123456789abcdef01234567"}, "changed while"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			st, target, origin := prRepo(t)
			fakeGH(t, c.pr, origin)
			before := originMain(t, origin)
			_, err := MergePR(context.Background(), st, target, 1, false, Options{Runner: &fakeRunner{}})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if originMain(t, origin) != before {
				t.Error("main moved")
			}
		})
	}
}

func TestMergePRConflictFailsWithoutPush(t *testing.T) {
	st, target, origin := prRepo(t)
	fakeGH(t, openPR, origin)
	// main and the PR both add conflicting copies of the same file.
	other := t.TempDir()
	gitT(t, other, "clone", "-q", origin, ".")
	write(t, filepath.Join(other, "feature.txt"), "from main\n")
	gitT(t, other, "add", ".")
	gitT(t, other, "commit", "-q", "-m", "conflicting")
	gitT(t, other, "push", "-q", "origin", "main")
	before := originMain(t, origin)

	_, err := MergePR(context.Background(), st, target, 1, false, Options{Runner: &fakeRunner{}})
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("err = %v", err)
	}
	if originMain(t, origin) != before {
		t.Error("main moved")
	}
}

func TestMergePRRefusesWhenMainMovedDuringRun(t *testing.T) {
	st, target, origin := prRepo(t)
	fakeGH(t, openPR, origin)
	other := t.TempDir()
	gitT(t, other, "clone", "-q", origin, ".")
	write(t, filepath.Join(other, "late.txt"), "landed during CI\n")
	gitT(t, other, "add", ".")
	gitT(t, other, "commit", "-q", "-m", "late")

	// Main moves while the tests run.
	r := &movingRunner{recordingRunner: &recordingRunner{}, move: func() { gitT(t, other, "push", "-q", "origin", "main") }}
	_, err := MergePR(context.Background(), st, target, 1, false, Options{Runner: r})
	if err == nil || !strings.Contains(err.Error(), "moved") {
		t.Fatalf("err = %v", err)
	}
	if got := gitT(t, origin, "log", "-1", "--format=%s", "main"); got != "late" {
		t.Errorf("origin main tip = %q, want the late commit untouched", got)
	}
}

type movingRunner struct {
	*recordingRunner
	move func()
}

func (m *movingRunner) Run(ctx context.Context, dir, name string, args ...string) error {
	m.move()
	return m.recordingRunner.Run(ctx, dir, name, args...)
}

func TestPlanMergeChangesNothing(t *testing.T) {
	_, target, origin := prRepo(t)
	fakeGH(t, openPR, origin)
	before := originMain(t, origin)
	p, err := PlanMerge(context.Background(), target, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.PR != 1 || p.Base != "main" || p.Revision == "" || originMain(t, origin) != before {
		t.Errorf("plan = %+v", p)
	}
	if refs := gitT(t, target.Dir, "for-each-ref", "refs/j-cicd"); refs != "" {
		t.Errorf("leftover refs: %s", refs)
	}
}

func TestMergePRWithoutBranchRequiresDefaultBranch(t *testing.T) {
	st, target, origin := prRepo(t)
	write(t, target.ConfigPath, strings.Replace(mergeConfig, "branch: main\n", "", 1))
	gitT(t, target.Dir, "add", ".")
	gitT(t, target.Dir, "commit", "-q", "-m", "no branch")
	gitT(t, target.Dir, "push", "-q", "origin", "main")
	// The PR must now be rebuilt on the new main; point it at a PR to develop.
	fakeGH(t, prInfo{State: "OPEN", BaseRefName: "develop"}, origin)
	_, err := MergePR(context.Background(), st, target, 1, false, Options{Runner: &fakeRunner{}})
	if err == nil || !strings.Contains(err.Error(), "targets develop") {
		t.Fatalf("err = %v, want refusal of a non-default base", err)
	}
}

func TestMergePRNothingToMerge(t *testing.T) {
	st, target, origin := prRepo(t)
	fakeGH(t, openPR, origin)
	// main already contains the PR's head.
	gitT(t, target.Dir, "fetch", "-q", "origin", "refs/pull/1/head")
	gitT(t, target.Dir, "merge", "-q", "--ff-only", "FETCH_HEAD")
	gitT(t, target.Dir, "push", "-q", "origin", "main")
	_, err := MergePR(context.Background(), st, target, 1, false, Options{Runner: &fakeRunner{}})
	if err == nil || !strings.Contains(err.Error(), "nothing to merge") {
		t.Fatalf("err = %v", err)
	}
}

func TestMergePRMainMovedBeforeDeployDeploysNothing(t *testing.T) {
	st, target, origin := prRepo(t)
	fakeGH(t, openPR, origin)
	write(t, target.ConfigPath, mergeConfig+"images:\n  - name: web\n    repository: registry.example.com/app\ndeploy:\n  host: vps\n")
	gitT(t, target.Dir, "add", ".")
	gitT(t, target.Dir, "commit", "-q", "-m", "deploy config")
	gitT(t, target.Dir, "push", "-q", "origin", "main")
	// The PR was branched before that commit; refresh it so it merges.
	gitT(t, target.Dir, "checkout", "-q", "feature")
	gitT(t, target.Dir, "merge", "-q", "-m", "sync", "main")
	gitT(t, target.Dir, "push", "-q", "origin", "feature:refs/pull/1/head")
	gitT(t, target.Dir, "checkout", "-q", "main")
	other := t.TempDir()
	gitT(t, other, "clone", "-q", origin, ".")
	write(t, filepath.Join(other, "late.txt"), "landed during CI\n")
	gitT(t, other, "add", ".")
	gitT(t, other, "commit", "-q", "-m", "late")

	r := &movingRunner{recordingRunner: &recordingRunner{}, move: func() { gitT(t, other, "push", "-q", "origin", "main") }}
	run, err := MergePR(context.Background(), st, target, 1, false, Options{Deploy: true, Runner: r})
	if err == nil || !strings.Contains(err.Error(), "not deploying") {
		t.Fatalf("err = %v", err)
	}
	if run.Deployed {
		t.Error("deployed after main moved")
	}
	for _, c := range r.calls {
		if strings.Contains(c.cmd, "j-cicd-agent") || strings.Contains(c.cmd, "ssh") {
			t.Errorf("deploy command ran: %s", c.cmd)
		}
	}
}

func TestMergePRCancelledLeavesNoRefsOrSnapshot(t *testing.T) {
	st, target, origin := prRepo(t)
	fakeGH(t, openPR, origin)
	_ = st
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run, err := MergePR(ctx, st, target, 1, false, Options{Runner: &fakeRunner{}})
	if err == nil || run.Status != store.StatusCancelled {
		t.Fatalf("err = %v, status = %v", err, run.Status)
	}
	if refs := gitT(t, target.Dir, "for-each-ref", "refs/j-cicd"); refs != "" {
		t.Errorf("leftover refs: %s", refs)
	}
	if _, err := os.Stat(snapshotDir(st.Root(), "app")); err == nil {
		t.Error("snapshot left behind")
	}
}
