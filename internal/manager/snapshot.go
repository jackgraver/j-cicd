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
	branch   string
	commit   string // full SHA
	revision string // short SHA, the per-commit image tag
	// prefix is the project directory's path inside its repository ("" at
	// the top level), so a project in a monorepo subdirectory builds the
	// same subdirectory of the snapshot.
	prefix string
}

// resolveRelease finds the commit the local branch points at in the
// repository containing dir. It reads only refs, never the working tree,
// so what's checked out and any uncommitted changes don't matter.
func resolveRelease(ctx context.Context, dir, branch string) (*release, error) {
	prefix, err := git(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, fmt.Errorf("branch: %s is set, but %s is not in a git repository", branch, dir)
	}
	commit, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("release branch %q doesn't exist in %s: create it, or change branch: in the config", branch, dir)
	}
	revision, err := git(ctx, dir, "rev-parse", "--short=12", commit)
	if err != nil {
		return nil, err
	}
	return &release{branch: branch, commit: commit, revision: revision, prefix: strings.TrimSuffix(prefix, "/")}, nil
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

// createSnapshot checks rel.commit out at snapshotDir(root, project). A
// leftover from a run that was killed before cleaning up is removed first.
func createSnapshot(ctx context.Context, root, project, repo string, rel *release) (*snapshot, error) {
	s := &snapshot{repo: repo, dir: snapshotDir(root, project)}
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
