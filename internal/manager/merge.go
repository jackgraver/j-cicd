package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"j-cicd/internal/config"
	"j-cicd/internal/store"
)

// remote is the git remote a pull request lives on and merges back to.
const remote = "origin"

// ghOutput runs the GitHub CLI in dir and returns its trimmed stdout. It's a
// variable so tests can stand in for GitHub; everything else a merge does is
// plain git, which the tests run against real repositories.
var ghOutput = func(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("gh %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("gh %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// prInfo is what `gh pr view` says about a pull request.
type prInfo struct {
	Title             string `json:"title"`
	State             string `json:"state"`
	HeadRefOid        string `json:"headRefOid"`
	BaseRefName       string `json:"baseRefName"`
	IsCrossRepository bool   `json:"isCrossRepository"`
}

// prMerge is a pull request merged onto its base in the local repository:
// a merge commit that exists only as a git object until it's pushed.
type prMerge struct {
	number  int
	pr      prInfo
	base    string // branch name, e.g. "main"
	baseSHA string // origin's tip of base when the merge was made
	headSHA string // the PR's head when the merge was made
	rel     *release
	repo    string
}

// baseRef and headRef are where a merge keeps the fetched tips, out of the
// way of the user's own branches and remote-tracking refs.
func (m *prMerge) baseRef() string { return fmt.Sprintf("refs/j-cicd/pr-%d/base", m.number) }
func (m *prMerge) headRef() string { return fmt.Sprintf("refs/j-cicd/pr-%d/head", m.number) }

// forget deletes the refs fetch created. The merge commit stays reachable
// through the snapshot's HEAD for as long as a run needs it.
func (m *prMerge) forget() {
	for _, ref := range []string{m.baseRef(), m.headRef()} {
		_, _ = git(context.Background(), m.repo, "update-ref", "-d", ref)
	}
}

// configuredBase is the config's branch: as it is on disk, or "" if there
// isn't one (or the config doesn't load; the run reports that itself).
func configuredBase(configPath string) string {
	cfg, err := config.Load(configPath, "")
	if err != nil {
		return ""
	}
	return cfg.Branch
}

// mergePR looks the pull request up, fetches it and its base, and merges
// them in memory (git merge-tree), so the user's checkout, index and
// branches are never touched. wantBase is the branch the project releases
// from ("" accepts whatever the PR targets). The merge fails, naming why,
// when the PR isn't open, comes from a fork, targets another branch, adds
// nothing, or conflicts.
func mergePR(ctx context.Context, dir string, number int, wantBase string) (*prMerge, error) {
	prefix, err := git(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, fmt.Errorf("%s is not in a git repository", dir)
	}
	out, err := ghOutput(ctx, dir, "pr", "view", fmt.Sprint(number), "--json", "title,state,headRefOid,baseRefName,isCrossRepository")
	if err != nil {
		return nil, fmt.Errorf("looking up PR #%d: %w", number, err)
	}
	var pr prInfo
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return nil, fmt.Errorf("reading PR #%d from gh: %w", number, err)
	}
	switch {
	case pr.State != "OPEN":
		return nil, fmt.Errorf("PR #%d is %s, not open", number, strings.ToLower(pr.State))
	case pr.IsCrossRepository:
		return nil, fmt.Errorf("PR #%d comes from a fork, which j-cicd doesn't merge (its code would run on this machine unreviewed)", number)
	case wantBase != "" && pr.BaseRefName != wantBase:
		return nil, fmt.Errorf("PR #%d targets %s, but this project releases from %s (branch: in its config)", number, pr.BaseRefName, wantBase)
	}

	m := &prMerge{number: number, pr: pr, base: pr.BaseRefName, repo: dir}
	if _, err := git(ctx, dir, "fetch", "--quiet", remote,
		"+refs/heads/"+m.base+":"+m.baseRef(),
		fmt.Sprintf("+refs/pull/%d/head:%s", number, m.headRef())); err != nil {
		return nil, fmt.Errorf("fetching PR #%d: %w", number, err)
	}
	ok := false
	defer func() {
		if !ok {
			m.forget()
		}
	}()
	if m.baseSHA, err = git(ctx, dir, "rev-parse", "--verify", m.baseRef()+"^{commit}"); err != nil {
		return nil, err
	}
	if m.headSHA, err = git(ctx, dir, "rev-parse", "--verify", m.headRef()+"^{commit}"); err != nil {
		return nil, err
	}
	if _, err := git(ctx, dir, "merge-base", "--is-ancestor", m.headSHA, m.baseSHA); err == nil {
		return nil, fmt.Errorf("PR #%d has nothing to merge: its head is already in %s", number, m.base)
	}

	tree, err := git(ctx, dir, "merge-tree", "--write-tree", "--no-messages", m.baseSHA, m.headSHA)
	if err != nil {
		return nil, fmt.Errorf("PR #%d doesn't merge cleanly into %s (conflicts): rebase or merge %s into it first", number, m.base, m.base)
	}
	commit, err := git(ctx, dir, "commit-tree", tree, "-p", m.baseSHA, "-p", m.headSHA,
		"-m", fmt.Sprintf("Merge pull request #%d: %s", number, pr.Title))
	if err != nil {
		return nil, err
	}
	revision, err := git(ctx, dir, "rev-parse", "--short=12", commit)
	if err != nil {
		return nil, err
	}
	m.rel = &release{branch: m.base, commit: commit, revision: revision, prefix: strings.TrimSuffix(prefix, "/"), pr: number}
	ok = true
	return m, nil
}

// pushMerge makes the merge commit the tip of base on the remote. It's a
// plain fast-forward push (never forced), and refuses up front if base
// moved since the merge was made, so a green run can't land on top of
// commits it never tested.
func pushMerge(ctx context.Context, m *prMerge) error {
	out, err := git(ctx, m.repo, "ls-remote", remote, "refs/heads/"+m.base)
	if err != nil {
		return err
	}
	tip, _, _ := strings.Cut(out, "\t")
	if tip != m.baseSHA {
		return fmt.Errorf("%s moved on %s while this ran (was %s, now %s): nothing was pushed; run it again",
			m.base, remote, short(m.baseSHA), short(tip))
	}
	if _, err := git(ctx, m.repo, "push", "--quiet", remote, m.rel.commit+":refs/heads/"+m.base); err != nil {
		return fmt.Errorf("pushing the merge to %s/%s: %w", remote, m.base, err)
	}
	return nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// MergePR runs a pull request through the project's pipeline as if it were
// already merged, and publishes it if that goes well. The PR is merged onto
// its base in the repository (the user's checkout is untouched), the merge
// commit is checked out as a clean snapshot, and steps, image build and
// push, deploy and verify run there exactly as for a release branch, with
// the images tagged by the merge commit. Only if all of that succeeds is
// that same commit pushed to the base branch, which GitHub shows as the PR
// merged. noMerge stops short of the push. The run is recorded (kind merge)
// and holds the project's lock. opts.Push is always on, since a deploy needs
// the images in a registry; opts.Deploy is the caller's choice.
//
// A deploy happens before the push, so the push can fail with the release
// live: the error then says so and names the rollback; it never rolls back
// by itself.
func MergePR(ctx context.Context, st *store.Store, t Target, number int, noMerge bool, opts Options) (store.Run, error) {
	unlock, err := lock(st, t)
	if err != nil {
		return store.Run{}, err
	}
	defer unlock()

	dir, configPath, err := absPaths(t)
	if err != nil {
		return store.Run{}, err
	}
	opts.Push = true
	return record(ctx, st, store.Run{
		Project: t.Name,
		Kind:    store.KindMerge,
		Dir:     dir,
		Config:  configPath,
		Push:    true,
		PR:      number,
	}, opts, fmt.Sprintf("of PR #%d in %q (%s)", number, t.Name, dir), func(env *runEnv) error {
		m, err := mergePR(ctx, dir, number, configuredBase(configPath))
		if err != nil {
			return err
		}
		defer m.forget()
		env.run.Revision, env.run.PRHeadSHA = m.rel.revision, m.headSHA
		env.log.Printf("merged PR #%d (%s) onto %s/%s at %s locally: merge commit %s", number, m.pr.Title, remote, m.base, short(m.baseSHA), m.rel.revision)

		if err := prepareAndExecute(ctx, st.Root(), t.Name, dir, configPath, m.rel.revision, m.rel, opts, env); err != nil {
			return err
		}
		if noMerge {
			env.log.Printf("--no-merge: tests passed, but %s/%s was not updated", remote, m.base)
			return nil
		}
		if err := pushMerge(ctx, m); err != nil {
			if env.run.Deployed {
				return fmt.Errorf("%w (the release IS deployed to %s but is not on %s: j-cicd rollback %s to undo it)", err, env.run.DeployHost, m.base, t.Name)
			}
			return err
		}
		env.run.PRMerged, env.run.MergedSHA = true, m.rel.commit
		env.log.Printf("pushed %s to %s/%s: PR #%d is merged", m.rel.revision, remote, m.base, number)
		return nil
	})
}

// MergePlan is what MergePR would merge, without running anything.
type MergePlan struct {
	PR       int    `json:"pr"`
	Title    string `json:"title"`
	Base     string `json:"base"`
	BaseSHA  string `json:"base_sha"`
	HeadSHA  string `json:"head_sha"`
	Revision string `json:"revision"` // the merge commit's short SHA, i.e. the image tag
}

// PlanMerge checks that the pull request can be merged (open, same
// repository, right base, no conflicts) and says what the merge would be. It
// takes no lock and records nothing; the only side effect is fetching the
// PR into the repository's object store.
func PlanMerge(ctx context.Context, t Target, number int) (*MergePlan, error) {
	dir, configPath, err := absPaths(t)
	if err != nil {
		return nil, err
	}
	m, err := mergePR(ctx, dir, number, configuredBase(configPath))
	if err != nil {
		return nil, err
	}
	defer m.forget()
	return &MergePlan{PR: number, Title: m.pr.Title, Base: m.base, BaseSHA: m.baseSHA, HeadSHA: m.headSHA, Revision: m.rel.revision}, nil
}
