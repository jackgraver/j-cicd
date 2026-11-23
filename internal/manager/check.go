package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"j-cicd/internal/config"
	"j-cicd/internal/detect"
	"j-cicd/internal/store"
)

// Check runs only t's steps, the way a pipeline run would, without building
// images, pushing or deploying: "will this pass the pipeline?" as one
// command that reads the same config. With ref == "" the project directory
// is checked as it is on disk (under the project's lock, since steps write
// build output there). With a ref (any commit-ish: a branch, a PR head, a
// SHA) that commit is checked out into a throwaway worktree and its own
// committed config is used, so a feature branch can be checked while a
// release branch is configured. It is recorded as a run of kind check.
func Check(ctx context.Context, st *store.Store, t Target, ref string, opts Options) (store.Run, error) {
	dir, configPath, err := absPaths(t)
	if err != nil {
		return store.Run{}, err
	}
	opts.StepsOnly = true

	var rel *release
	revision, _ := detect.GitRevision(dir)
	if ref == "" {
		unlock, err := lock(st, t)
		if err != nil {
			return store.Run{}, err
		}
		defer unlock()
	} else {
		if rel, err = resolveRef(ctx, dir, ref); err != nil {
			return store.Run{}, err
		}
		revision = rel.revision
	}

	what := fmt.Sprintf("check of %q in %s", t.Name, dir)
	if rel != nil {
		what = fmt.Sprintf("check of %q at %s (%s)", t.Name, ref, rel.revision)
	}
	return record(ctx, st, store.Run{
		Project:  t.Name,
		Kind:     store.KindCheck,
		Dir:      dir,
		Config:   configPath,
		Revision: revision,
	}, opts, what, func(env *runEnv) error {
		if rel == nil {
			return prepareAndExecute(ctx, st.Root(), t.Name, dir, configPath, revision, nil, opts, env)
		}
		// A check of a ref holds no lock, so its snapshot is unique per run:
		// it can't collide with a release run's, and two checks of one
		// commit can't remove each other's checkout (createSnapshot clears
		// whatever it finds).
		snapDir := filepath.Join(st.Root(), "snapshots", fmt.Sprintf("%s-check-%d-%d", t.Name, os.Getpid(), time.Now().UnixNano()))
		prep, err := prepareRef(ctx, snapDir, dir, configPath, rel, env)
		if err != nil {
			return err
		}
		defer prep.cleanup()
		return execute(ctx, prep.dir, prep.cfg, revision, opts, env)
	})
}

// resolveRef turns a commit-ish into the commit to check out.
func resolveRef(ctx context.Context, dir, ref string) (*release, error) {
	prefix, err := git(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, fmt.Errorf("--ref needs a git repository, but %s is not in one", dir)
	}
	if strings.HasPrefix(ref, "-") {
		return nil, fmt.Errorf("--ref %q is not a commit", ref)
	}
	commit, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("--ref %s is not a commit in %s", ref, dir)
	}
	revision, err := git(ctx, dir, "rev-parse", "--short=12", commit)
	if err != nil {
		return nil, err
	}
	return &release{branch: ref, commit: commit, revision: revision, prefix: strings.TrimSuffix(prefix, "/")}, nil
}

// prepareRef checks rel out at snapDir and loads the config committed
// there.
func prepareRef(ctx context.Context, snapDir, dir, configPath string, rel *release, env *runEnv) (*prepared, error) {
	env.log.Printf("checking %s at %s from a clean checkout (uncommitted changes in %s are not included)", rel.branch, rel.revision, dir)
	snap, err := createSnapshot(ctx, snapDir, dir, rel)
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		if err := snap.remove(); err != nil {
			env.warnf("couldn't remove snapshot %s: %v", snap.dir, err)
		}
	}
	buildDir := filepath.Join(snap.dir, rel.prefix)
	if inside, err := filepath.Rel(dir, configPath); err == nil && inside != ".." && !strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		configPath = filepath.Join(buildDir, inside)
	}
	cfg, err := config.Load(configPath, rel.revision)
	if err != nil {
		cleanup()
		return nil, &ConfigError{Err: fmt.Errorf("at %s: %w", rel.revision, err)}
	}
	return &prepared{dir: buildDir, cfg: cfg, cleanup: cleanup}, nil
}
