// Package manager executes a project's pipeline as a recorded run: it
// resolves what to build, records the run in the store, sends all output
// to the run's log, and records how it ended. The CLI and the interactive
// UI both run projects through here, so every run lands in history the
// same way whichever started it.
package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"j-cicd/internal/config"
	"j-cicd/internal/detect"
	"j-cicd/internal/pipeline"
	"j-cicd/internal/store"
)

// Target is what to run: a project name to record the run under, the
// directory the pipeline runs in, and its config.
type Target struct {
	Name       string
	Dir        string
	ConfigPath string
}

// TargetFor returns the target for a registered project.
func TargetFor(p store.Project) Target {
	return Target{Name: p.Name, Dir: p.Dir, ConfigPath: p.ConfigPath()}
}

// Options are the per-invocation settings of a run.
type Options struct {
	Push bool
	// Deploy runs the config's deploy: stage after pushing; DeployDirty
	// allows that for a build of uncommitted changes.
	Deploy, DeployDirty bool
	// Echo, if set, also receives everything written to the run log (the
	// CLI passes stderr so a foreground run is visible as it happens).
	Echo io.Writer
	// Runner overrides how commands execute; nil means real processes.
	// Tests substitute a fake here.
	Runner pipeline.CommandRunner
}

// ErrBusy means the project already has a run in progress. Two pipelines
// in one project directory would race on its build output and images.
var ErrBusy = errors.New("already running")

// ConfigError is a run that failed because its config couldn't be loaded,
// as opposed to a pipeline that ran and failed.
type ConfigError struct{ Err error }

func (e *ConfigError) Error() string { return "load config: " + e.Err.Error() }
func (e *ConfigError) Unwrap() error { return e.Err }

// Busy returns the project's in-progress run, if it has one.
func Busy(st *store.Store, project string) (store.Run, bool, error) {
	runs, err := st.Runs(project, 1)
	if err != nil {
		return store.Run{}, false, err
	}
	if len(runs) == 1 && !runs[0].Status.Done() {
		return runs[0], true, nil
	}
	return store.Run{}, false, nil
}

// Run executes t's pipeline and records it in st. It returns the recorded
// run along with the pipeline's error, if any. A run is recorded even when
// its config fails to load, so a broken config shows up in history rather
// than vanishing.
func Run(ctx context.Context, st *store.Store, t Target, opts Options) (store.Run, error) {
	unlock, err := lock(st, t)
	if err != nil {
		return store.Run{}, err
	}
	defer unlock()

	dir, configPath, err := absPaths(t)
	if err != nil {
		return store.Run{}, err
	}

	// The revision tags each image with the commit it was built from, so
	// a deploy has something immutable to pin even though "latest" moves.
	revision, hasRevision := detect.GitRevision(dir)

	// A release branch means building that branch's commit rather than
	// the directory, so the revision is the branch's, never "-dirty". A
	// config that doesn't load is left for execute to record as such.
	var rel *release
	var relErr error
	if cfg, err := config.Load(configPath, ""); err == nil && cfg.Branch != "" {
		rel, relErr = resolveRelease(ctx, dir, cfg.Branch)
		if relErr == nil {
			revision, hasRevision = rel.revision, true
		}
	}

	return record(ctx, st, store.Run{
		Project:  t.Name,
		Dir:      dir,
		Config:   configPath,
		Revision: revision,
		Push:     opts.Push,
	}, opts, fmt.Sprintf("of %q in %s", t.Name, dir), func(out io.Writer, logger *log.Logger) error {
		if !hasRevision && relErr == nil {
			logger.Printf("no git revision for %s — images will only be tagged \"latest\"", dir)
		}
		if relErr != nil {
			return relErr
		}
		return prepareAndExecute(ctx, st.Root(), t.Name, dir, configPath, revision, rel, opts, out, logger)
	})
}

// Rollback reverts t's last deploy: the deploy host's agent puts each
// service the last deploy changed back on the release it replaced, and
// health-checks it. It's recorded like a run (kind rollback) and holds the
// project's lock, so it can't interleave with a build of the same project.
// The deploy: section comes from the project's config as it is on disk.
func Rollback(ctx context.Context, st *store.Store, t Target, opts Options) (store.Run, error) {
	unlock, err := lock(st, t)
	if err != nil {
		return store.Run{}, err
	}
	defer unlock()

	dir, configPath, err := absPaths(t)
	if err != nil {
		return store.Run{}, err
	}
	return record(ctx, st, store.Run{
		Project: t.Name,
		Kind:    store.KindRollback,
		Dir:     dir,
		Config:  configPath,
	}, opts, fmt.Sprintf("rolling back %q's last deploy", t.Name), func(out io.Writer, logger *log.Logger) error {
		cfg, err := config.Load(configPath, "")
		if err != nil {
			return &ConfigError{Err: err}
		}
		p := pipeline.New(dir, pipeline.Options{Runner: runnerFor(opts, out), Logger: logger})
		if err := p.Rollback(ctx, cfg); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("cancelled: %w", err)
			}
			return err
		}
		return nil
	})
}

// lock takes the project's run lock. The lock, not the run history, is
// what makes a run exclusive: two launches a moment apart would both see
// no run in progress.
func lock(st *store.Store, t Target) (func(), error) {
	unlock, err := st.LockProject(t.Name)
	if errors.Is(err, store.ErrLocked) {
		if active, busy, _ := Busy(st, t.Name); busy {
			return nil, fmt.Errorf("%s: %w (run %s)", t.Name, ErrBusy, active.ID)
		}
		return nil, fmt.Errorf("%s: %w", t.Name, ErrBusy)
	}
	return unlock, err
}

func absPaths(t Target) (dir, configPath string, err error) {
	if dir, err = filepath.Abs(t.Dir); err != nil {
		return "", "", err
	}
	if configPath, err = filepath.Abs(t.ConfigPath); err != nil {
		return "", "", err
	}
	return dir, configPath, nil
}

// record starts r in the store, logs its ID and what it is, runs fn with a
// logger writing to the run's log (and opts.Echo), and records how it
// ended. A run is recorded even
// when fn fails before doing anything, so it shows up in history.
func record(ctx context.Context, st *store.Store, r store.Run, opts Options, what string, fn func(io.Writer, *log.Logger) error) (store.Run, error) {
	h, err := st.StartRun(r)
	if err != nil {
		return store.Run{}, err
	}

	out := io.Writer(h.Log)
	if opts.Echo != nil {
		out = io.MultiWriter(h.Log, opts.Echo)
	}
	logger := log.New(out, "", log.LstdFlags)
	logger.Printf("run %s %s", h.Run.ID, what)

	runErr := fn(out, logger)

	status := store.StatusSuccess
	switch {
	case runErr == nil:
	case ctx.Err() != nil:
		status = store.StatusCancelled
	default:
		status = store.StatusFailed
	}
	errMsg := ""
	if runErr != nil {
		errMsg = runErr.Error()
		logger.Printf("run %s: %v", status, runErr)
	} else {
		logger.Printf("run succeeded")
	}

	if err := h.Finish(status, errMsg); err != nil {
		return h.Run, errors.Join(runErr, err)
	}
	return h.Run, runErr
}

// runnerFor is opts.Runner, else real processes writing to out. One
// writer for both streams keeps stdout and stderr in the order they were
// written.
func runnerFor(opts Options, out io.Writer) pipeline.CommandRunner {
	if opts.Runner != nil {
		return opts.Runner
	}
	return pipeline.ExecRunner{Stdout: out, Stderr: out}
}

// prepareAndExecute runs the pipeline in dir, or, when rel is set, in a
// fresh snapshot of the release commit that is removed afterwards.
func prepareAndExecute(ctx context.Context, root, project, dir, configPath, revision string, rel *release, opts Options, out io.Writer, logger *log.Logger) error {
	if rel == nil {
		cfg, err := config.Load(configPath, revision)
		if err != nil {
			return &ConfigError{Err: err}
		}
		return execute(ctx, dir, cfg, revision, opts, out, logger)
	}

	logger.Printf("building branch %s at %s from a clean checkout (uncommitted changes and other branches in %s are not included)", rel.branch, rel.revision, dir)
	snap, err := createSnapshot(ctx, root, project, dir, rel)
	if err != nil {
		return err
	}
	defer func() {
		if err := snap.remove(); err != nil {
			logger.Printf("warning: couldn't remove snapshot %s: %v", snap.dir, err)
		}
	}()
	buildDir := filepath.Join(snap.dir, rel.prefix)

	// The config is part of what's released, so it comes from the commit
	// too, unless it lives outside the project (--config elsewhere).
	if inside, err := filepath.Rel(dir, configPath); err == nil && inside != ".." && !strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		configPath = filepath.Join(buildDir, inside)
		if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
			return &ConfigError{Err: fmt.Errorf("%s at %s has no %s: commit it to %s", rel.branch, rel.revision, inside, rel.branch)}
		}
	}
	cfg, err := config.Load(configPath, revision)
	if err != nil {
		return &ConfigError{Err: err}
	}
	if cfg.Branch != rel.branch {
		logger.Printf("warning: the config committed on %s doesn't say branch: %s (only your working copy does) — commit it so the setting is part of the project", rel.branch, rel.branch)
	}
	return execute(ctx, buildDir, cfg, revision, opts, out, logger)
}

func execute(ctx context.Context, dir string, cfg *config.Config, revision string, opts Options, out io.Writer, logger *log.Logger) error {
	runner := runnerFor(opts, out)
	p := pipeline.New(dir, pipeline.Options{
		Push:        opts.Push,
		Deploy:      opts.Deploy,
		Revision:    revision,
		DeployDirty: opts.DeployDirty,
		Runner:      runner,
		Logger:      logger,
	})
	if err := p.Run(ctx, cfg); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled: %w", err)
		}
		return err
	}
	return nil
}
