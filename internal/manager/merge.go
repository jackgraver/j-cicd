package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"j-cicd/internal/config"
	"j-cicd/internal/detect"
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
	IsDraft           bool   `json:"isDraft"`
	ReviewDecision    string `json:"reviewDecision"`
	Mergeable         string `json:"mergeable"`
}

// githubRepoFor is the owner/repo of dir's origin, which every gh call is
// pinned to (gh would otherwise pick a repo by its own rules, e.g. an
// "upstream" remote, while git fetches origin). A variable for tests.
var githubRepoFor = detect.GitHubRepo

// prMerge is a pull request merged onto its base in the local repository:
// a merge commit that exists only as a git object until it's pushed.
type prMerge struct {
	number  int
	scope   string // "run" or "plan": keeps their scratch refs apart
	pr      prInfo
	base    string // branch name, e.g. "main"
	baseSHA string // origin's tip of base when the merge was made
	headSHA string // the PR's head when the merge was made
	rel     *release
	repo    string
}

// The scratch refs hold the fetched tips and the merge commit, out of the
// way of the user's own branches. The merge ref keeps the commit reachable
// after its snapshot is gone, until it's been pushed.
func (m *prMerge) refPrefix() string { return fmt.Sprintf("refs/j-cicd/%s-%d/", m.scope, m.number) }
func (m *prMerge) baseRef() string   { return m.refPrefix() + "base" }
func (m *prMerge) headRef() string   { return m.refPrefix() + "head" }
func (m *prMerge) mergeRef() string  { return m.refPrefix() + "merge" }

// forget deletes the scratch refs.
func (m *prMerge) forget() {
	for _, ref := range []string{m.baseRef(), m.headRef(), m.mergeRef()} {
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

// remoteTip is where ref points on the remote, "" if it doesn't exist.
func remoteTip(ctx context.Context, repo, ref string) (string, error) {
	out, err := git(ctx, repo, "ls-remote", remote, ref)
	if err != nil {
		return "", err
	}
	tip, _, _ := strings.Cut(out, "\t")
	return tip, nil
}

// mergeTree merges head into base without a working tree. Exit status 1 is
// conflicts; anything else (unrelated histories, a git too old for
// --write-tree) is an error to show as it is.
func mergeTree(ctx context.Context, dir, base, head string) (tree string, conflicts bool, err error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "merge-tree", "--write-tree", "--no-messages", base, head)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		tree, _, _ = strings.Cut(strings.TrimSpace(string(out)), "\n")
		return tree, false, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", true, nil
	}
	return "", false, fmt.Errorf("git merge-tree: %s", strings.TrimSpace(stderr.String()+" "+err.Error()))
}

// mergePR looks the pull request up, fetches it and its base, and merges
// them in memory, so the user's checkout, index and branches are never
// touched. wantBase is the branch the project releases from; "" means the
// repository's default branch. The merge fails, naming why, when the PR
// isn't open, is a draft, lacks (or is refused) review, comes from a fork,
// targets another branch, adds nothing, changed while it was fetched, or
// conflicts. scope keeps concurrent plans and runs' scratch refs apart.
func mergePR(ctx context.Context, dir string, number int, wantBase, scope string) (*prMerge, error) {
	prefix, err := git(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, fmt.Errorf("%s is not in a git repository", dir)
	}
	owner, name, ok := githubRepoFor(dir)
	if !ok {
		return nil, fmt.Errorf("the %s remote of %s isn't a GitHub repository", remote, dir)
	}
	slug := owner + "/" + name
	if wantBase == "" {
		if wantBase, err = ghOutput(ctx, dir, "repo", "view", "-R", slug, "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name"); err != nil || wantBase == "" {
			return nil, fmt.Errorf("finding %s's default branch (set branch: in the config to name it): %v", slug, err)
		}
	}
	out, err := ghOutput(ctx, dir, "pr", "view", fmt.Sprint(number), "-R", slug, "--json",
		"title,state,headRefOid,baseRefName,isCrossRepository,isDraft,reviewDecision,mergeable")
	if err != nil {
		return nil, fmt.Errorf("looking up PR #%d: %w", number, err)
	}
	var pr prInfo
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return nil, fmt.Errorf("reading PR #%d from gh: %w", number, err)
	}
	// This merge's config decides what runs here and where it deploys, so
	// a PR nobody has approved isn't something to run.
	switch {
	case pr.State != "OPEN":
		return nil, fmt.Errorf("PR #%d is %s, not open", number, strings.ToLower(pr.State))
	case pr.IsDraft:
		return nil, fmt.Errorf("PR #%d is a draft", number)
	case pr.ReviewDecision == "CHANGES_REQUESTED" || pr.ReviewDecision == "REVIEW_REQUIRED":
		return nil, fmt.Errorf("PR #%d isn't approved (review: %s)", number, strings.ToLower(strings.ReplaceAll(pr.ReviewDecision, "_", " ")))
	case pr.IsCrossRepository:
		return nil, fmt.Errorf("PR #%d comes from a fork, which j-cicd doesn't merge (its code would run on this machine unreviewed)", number)
	case pr.BaseRefName != wantBase:
		return nil, fmt.Errorf("PR #%d targets %s, but this project releases from %s (branch: in its config, else the default branch)", number, pr.BaseRefName, wantBase)
	case pr.Mergeable == "CONFLICTING":
		return nil, fmt.Errorf("PR #%d has conflicts with %s: rebase or merge %s into it first", number, wantBase, wantBase)
	}

	m := &prMerge{number: number, scope: scope, pr: pr, base: pr.BaseRefName, repo: dir}
	// --no-tags and an empty refmap keep this to exactly the two refs: no
	// remote-tracking branch or tag in the user's repository changes.
	if _, err := git(ctx, dir, "fetch", "--quiet", "--no-tags", "--refmap=", remote,
		"+refs/heads/"+m.base+":"+m.baseRef(),
		fmt.Sprintf("+refs/pull/%d/head:%s", number, m.headRef())); err != nil {
		m.forget()
		return nil, fmt.Errorf("fetching PR #%d: %w", number, err)
	}
	ok = false
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
	if pr.HeadRefOid != m.headSHA {
		return nil, fmt.Errorf("PR #%d changed while it was being read (GitHub says %s, fetched %s): run it again", number, short(pr.HeadRefOid), short(m.headSHA))
	}
	if _, err := git(ctx, dir, "merge-base", "--is-ancestor", m.headSHA, m.baseSHA); err == nil {
		return nil, fmt.Errorf("PR #%d has nothing to merge: its head is already in %s", number, m.base)
	}

	tree, conflicts, err := mergeTree(ctx, dir, m.baseSHA, m.headSHA)
	if err != nil {
		return nil, err
	}
	if conflicts {
		return nil, fmt.Errorf("PR #%d doesn't merge cleanly into %s (conflicts): rebase or merge %s into it first", number, m.base, m.base)
	}
	// --no-gpg-sign: this commit is scaffolding that never reaches anyone's
	// history unsigned-by-surprise, and a signing prompt would hang a run
	// started from the dashboard, which has no terminal.
	commit, err := git(ctx, dir, "commit-tree", "--no-gpg-sign", tree, "-p", m.baseSHA, "-p", m.headSHA,
		"-m", fmt.Sprintf("Merge pull request #%d: %s", number, pr.Title))
	if err != nil {
		if strings.Contains(err.Error(), "identity") {
			return nil, fmt.Errorf("%w (set git user.name and user.email)", err)
		}
		return nil, err
	}
	if _, err := git(ctx, dir, "update-ref", m.mergeRef(), commit); err != nil {
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

// unmoved checks that nothing the merge was tested against has changed on
// the remote: the base is still where the merge was made, and the PR still
// has the head that was merged (else GitHub wouldn't see the PR as merged).
func (m *prMerge) unmoved(ctx context.Context) error {
	tip, err := remoteTip(ctx, m.repo, "refs/heads/"+m.base)
	if err != nil {
		return err
	}
	if tip != m.baseSHA {
		return fmt.Errorf("%s moved on %s while this ran (was %s, now %s): run it again", m.base, remote, short(m.baseSHA), short(tip))
	}
	head, err := remoteTip(ctx, m.repo, fmt.Sprintf("refs/pull/%d/head", m.number))
	if err != nil {
		return err
	}
	if head != m.headSHA {
		return fmt.Errorf("PR #%d was updated while this ran (was %s, now %s): run it again", m.number, short(m.headSHA), short(head))
	}
	return nil
}

// pushMerge makes the merge commit the tip of base on the remote. The push
// is a compare-and-swap on the base the merge was tested on (never a
// blind or forced push), so commits that landed meanwhile can't be
// overwritten. It reports whether the commit is on the remote, which after
// a cancelled push is asked of the remote rather than assumed.
func pushMerge(ctx context.Context, m *prMerge) (pushed bool, err error) {
	if err := m.unmoved(ctx); err != nil {
		return false, err
	}
	_, err = git(ctx, m.repo, "push", "--quiet", "--force-with-lease=refs/heads/"+m.base+":"+m.baseSHA,
		remote, m.rel.commit+":refs/heads/"+m.base)
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		// git may have been killed after the remote took the ref.
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if tip, tipErr := remoteTip(bg, m.repo, "refs/heads/"+m.base); tipErr == nil && tip == m.rel.commit {
			return true, nil
		}
	}
	return false, fmt.Errorf("pushing the merge to %s/%s: %w", remote, m.base, err)
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
		m, err := mergePR(ctx, dir, number, configuredBase(configPath), "run")
		if err != nil {
			return err
		}
		defer m.forget()
		env.run.Revision, env.run.PRHeadSHA = m.rel.revision, m.headSHA
		env.log.Printf("merged PR #%d (%s) onto %s/%s at %s locally: merge commit %s", number, m.pr.Title, remote, m.base, short(m.baseSHA), m.rel.revision)

		if !noMerge {
			// Refuse before anything goes live, not only after: a deploy
			// of a merge that lacks what has landed on the base since would
			// put production behind it.
			opts.beforeDeploy = m.unmoved
		}
		if err := prepareAndExecute(ctx, st.Root(), t.Name, dir, configPath, m.rel.revision, m.rel, opts, env); err != nil {
			return err
		}
		if noMerge {
			env.log.Printf("--no-merge: tests passed, but %s/%s was not updated", remote, m.base)
			return nil
		}
		pushed, err := pushMerge(ctx, m)
		if pushed {
			env.run.PRMerged, env.run.MergedSHA = true, m.rel.commit
		}
		if err != nil {
			if env.run.Deployed {
				return fmt.Errorf("%w (the release IS deployed to %s but is not on %s: j-cicd rollback %s to undo it)", err, env.run.DeployHost, m.base, t.Name)
			}
			return err
		}
		env.log.Printf("pushed %s to %s/%s: PR #%d is merged", m.rel.revision, remote, m.base, number)
		return nil
	})
}

// MergePlan is what MergePR would merge, without running anything.
type MergePlan struct {
	PR      int    `json:"pr"`
	Title   string `json:"title"`
	Base    string `json:"base"`
	BaseSHA string `json:"base_sha"`
	HeadSHA string `json:"head_sha"`
	// Revision is the merge commit's short SHA as made for this plan. A run
	// makes its own merge commit (its timestamp differs), so its image tag
	// will not be this one.
	Revision string `json:"revision"`
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
	m, err := mergePR(ctx, dir, number, configuredBase(configPath), "plan")
	if err != nil {
		return nil, err
	}
	defer m.forget()
	return &MergePlan{PR: number, Title: m.pr.Title, Base: m.base, BaseSHA: m.baseSHA, HeadSHA: m.headSHA, Revision: m.rel.revision}, nil
}
