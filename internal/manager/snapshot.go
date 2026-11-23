package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// release is the commit a run with a release branch builds: config's
// branch: resolved in the project's repository when the run starts.
type release struct {
	branch string
	// remote is config's remote: ("" builds the local branch). When set the
	// run fetched the branch from it and builds the remote-tracking copy.
	remote   string
	commit   string // full SHA
	revision string // short SHA, the per-commit image tag
	// prefix is the project directory's path inside its repository ("" at
	// the top level), so a project in a monorepo subdirectory builds the
	// same subdirectory of the snapshot.
	prefix string
}

// source names what the release was resolved from, for messages:
// "origin/main" or "local branch main".
func (r *release) source() string {
	if r.remote != "" {
		return r.remote + "/" + r.branch
	}
	return "the local branch " + r.branch
}

// fetchTimeout bounds the fetch of a remote release branch, so an
// unreachable remote fails the run rather than hanging it.
const fetchTimeout = 2 * time.Minute

// resolveRelease finds the commit a release builds in the repository
// containing dir. Without a remote that's the local branch's tip, read from
// refs only, so what's checked out and any uncommitted changes don't
// matter. With one, the branch is fetched first and the remote's copy is
// built, so a PR merged on the remote is included without anyone pulling;
// a failed fetch fails the run rather than building something stale.
// A non-empty pin builds that commit instead of the tip, provided it is
// part of the release branch's history.
func resolveRelease(ctx context.Context, dir, branch, remote, pin string) (*release, error) {
	prefix, err := git(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, fmt.Errorf("branch: %s is set, but %s is not in a git repository", branch, dir)
	}
	ref := "refs/heads/" + branch
	if remote != "" {
		ref = "refs/remotes/" + remote + "/" + branch
		fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		defer cancel()
		if _, err := git(fctx, dir, "fetch", "--quiet", "--no-tags", remote, "+refs/heads/"+branch+":"+ref); err != nil {
			return nil, fmt.Errorf("fetching %s/%s: %w (remove remote: from the config to build the local branch instead)", remote, branch, err)
		}
	}
	commit, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		if remote != "" {
			return nil, fmt.Errorf("release branch %q doesn't exist on remote %q", branch, remote)
		}
		return nil, fmt.Errorf("release branch %q doesn't exist in %s: create it, or change branch: in the config", branch, dir)
	}
	if pin != "" {
		if strings.HasPrefix(pin, "-") {
			return nil, fmt.Errorf("--commit %q is not a commit", pin)
		}
		pinned, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", pin+"^{commit}")
		if err != nil {
			return nil, fmt.Errorf("--commit %s is not a commit in %s", pin, dir)
		}
		if _, err := git(ctx, dir, "merge-base", "--is-ancestor", pinned, commit); err != nil {
			return nil, fmt.Errorf("--commit %s is not part of %s's history, so it won't be built as a release", pin, strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/remotes/"))
		}
		commit = pinned
	}
	revision, err := git(ctx, dir, "rev-parse", "--short=12", commit)
	if err != nil {
		return nil, err
	}
	return &release{branch: branch, remote: remote, commit: commit, revision: revision, prefix: strings.TrimSuffix(prefix, "/")}, nil
}

// snapshot is a detached git worktree of a release commit that a run
// builds in. It holds exactly what the commit contains: no uncommitted or
// untracked files, and none of the ignored ones (node_modules, .env, build
// output), so the steps have to produce everything they need themselves.
type snapshot struct {
	repo string // a directory in the repository the worktree belongs to
	dir  string
}

// snapshotDir is where a project's snapshot lives under the store root.
// One per project is enough: the project lock means only one run can be
// using it at a time.
func snapshotDir(root, project string) string {
	return filepath.Join(root, "snapshots", project)
}

// createSnapshot checks rel.commit out at dir. A leftover from a run that
// was killed before cleaning up is removed first.
func createSnapshot(ctx context.Context, dir, repo string, rel *release) (*snapshot, error) {
	s := &snapshot{repo: repo, dir: dir}
	if err := s.remove(); err != nil {
		return nil, fmt.Errorf("removing stale snapshot %s: %w", s.dir, err)
	}
	if err := os.MkdirAll(filepath.Dir(s.dir), 0o755); err != nil {
		return nil, err
	}
	if _, err := git(ctx, repo, "worktree", "add", "--detach", "--quiet", s.dir, rel.commit); err != nil {
		return nil, fmt.Errorf("checking out %s (%s): %w", rel.branch, rel.revision, err)
	}
	return s, nil
}

// remove deletes the snapshot and the repository's record of it. It runs
// after the run's context may already be cancelled, so it has its own
// deadline rather than the run's.
func (s *snapshot) remove() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := os.Stat(s.dir); errors.Is(err, os.ErrNotExist) {
		_, _ = git(ctx, s.repo, "worktree", "prune")
		return nil
	}
	// --force: steps write build output into it, which is the point.
	if _, err := git(ctx, s.repo, "worktree", "remove", "--force", s.dir); err == nil {
		return nil
	}
	// Not a worktree of this repository (e.g. the project was registered
	// to a different directory since): just delete it.
	if err := os.RemoveAll(s.dir); err != nil {
		return err
	}
	_, _ = git(ctx, s.repo, "worktree", "prune")
	return nil
}

// git runs a git command in dir and returns its trimmed stdout, with
// stderr folded into the error.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// A fetch must fail, not wait on a credentials prompt nobody can see.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// syncWarning says how what a run builds stands against what it could have
// built instead, so it's never a guess whether a build includes commits
// that haven't been pushed. rel is nil for a run without a release branch,
// which builds the checked-out HEAD. It compares against the last fetch
// (a remote release was fetched just now); j-cicd never touches the
// network for this. "" means nothing to say, or no upstream to compare with.
func syncWarning(ctx context.Context, dir string, rel *release) string {
	s, ok := releaseSync(ctx, dir, rel)
	if !ok {
		return ""
	}
	if rel != nil && rel.remote != "" {
		return formatRemoteSync(rel.branch, s.Upstream, s.Ahead)
	}
	ref := "HEAD"
	if rel != nil {
		ref = rel.branch
	}
	return formatSync(refName(ref), s.Upstream, s.Ahead, s.Behind)
}

// releaseSync compares the ref a run is about to build with its upstream.
// For a remote release it's the local branch against the remote's copy
// (ok is false if there's no local branch), so Ahead counts commits that
// exist only locally and are not in the build.
func releaseSync(ctx context.Context, dir string, rel *release) (syncStatus, bool) {
	switch {
	case rel == nil:
		return upstreamSync(ctx, dir, "HEAD")
	case rel.remote == "":
		return upstreamSync(ctx, dir, rel.branch)
	}
	remoteRef := rel.remote + "/" + rel.branch
	counts, err := git(ctx, dir, "rev-list", "--left-right", "--count", "refs/heads/"+rel.branch+"...refs/remotes/"+remoteRef)
	if err != nil {
		return syncStatus{}, false
	}
	s := syncStatus{Upstream: remoteRef}
	if _, err := fmt.Sscanf(counts, "%d %d", &s.Ahead, &s.Behind); err != nil {
		return syncStatus{}, false
	}
	return s, true
}

// syncStatus is how a local ref stands against its upstream.
type syncStatus struct {
	Upstream      string
	Ahead, Behind int
}

// upstreamSync compares ref with its configured upstream as of the last
// fetch. ok is false when there's no upstream to compare with.
func upstreamSync(ctx context.Context, dir, ref string) (syncStatus, bool) {
	upstream, err := git(ctx, dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", ref+"@{upstream}")
	if err != nil {
		return syncStatus{}, false
	}
	counts, err := git(ctx, dir, "rev-list", "--left-right", "--count", ref+"..."+upstream)
	if err != nil {
		return syncStatus{}, false
	}
	s := syncStatus{Upstream: upstream}
	if _, err := fmt.Sscanf(counts, "%d %d", &s.Ahead, &s.Behind); err != nil {
		return syncStatus{}, false
	}
	return s, true
}

func refName(ref string) string {
	if ref == "HEAD" {
		return "the checked-out branch"
	}
	return ref
}

func formatSync(name, upstream string, ahead, behind int) string {
	plural := func(n int) string {
		if n == 1 {
			return "1 commit"
		}
		return fmt.Sprintf("%d commits", n)
	}
	switch {
	case ahead > 0 && behind > 0:
		return fmt.Sprintf("local %s has diverged from %s (%s ahead, %s behind, as of the last fetch); j-cicd builds the local one", name, upstream, plural(ahead), plural(behind))
	case ahead > 0:
		return fmt.Sprintf("local %s is %s ahead of %s (not pushed); j-cicd builds the local one, so those commits are included", name, plural(ahead), upstream)
	case behind > 0:
		return fmt.Sprintf("local %s is %s behind %s (as of the last fetch); j-cicd builds the local one, without them", name, plural(behind), upstream)
	}
	return ""
}

// formatRemoteSync warns about local commits a build of the remote's copy
// leaves out. Being behind is the point of building the remote, so it
// isn't mentioned.
func formatRemoteSync(branch, upstream string, ahead int) string {
	if ahead == 0 {
		return ""
	}
	n := fmt.Sprintf("%d commits", ahead)
	if ahead == 1 {
		n = "1 commit"
	}
	return fmt.Sprintf("local %s has %s not on %s (unpushed); they are not in this build", branch, n, upstream)
}
