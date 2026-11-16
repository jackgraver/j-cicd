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
	// The lock, not the run history, is what makes this exclusive: two
	// launches a moment apart would both see no run in progress.
	unlock, err := st.LockProject(t.Name)
	if errors.Is(err, store.ErrLocked) {
		if active, busy, _ := Busy(st, t.Name); busy {
			return active, fmt.Errorf("%s: %w (run %s)", t.Name, ErrBusy, active.ID)
		}
		return store.Run{}, fmt.Errorf("%s: %w", t.Name, ErrBusy)
	}
	if err != nil {
		return store.Run{}, err
	}
	defer unlock()

	dir, err := filepath.Abs(t.Dir)
	if err != nil {
		return store.Run{}, err
	}
	configPath, err := filepath.Abs(t.ConfigPath)
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

	h, err := st.StartRun(store.Run{
		Project:  t.Name,
		Dir:      dir,
		Config:   configPath,
		Revision: revision,
		Push:     opts.Push,
	})
	if err != nil {
		return store.Run{}, err
	}

	out := io.Writer(h.Log)
	if opts.Echo != nil {
		out = io.MultiWriter(h.Log, opts.Echo)
	}
	logger := log.New(out, "", log.LstdFlags)
	logger.Printf("run %s of %q in %s", h.Run.ID, t.Name, dir)
	if !hasRevision && relErr == nil {
		logger.Printf("no git revision for %s — images will only be tagged \"latest\"", dir)
	}

	runErr := relErr
	if runErr == nil {
		runErr = prepareAndExecute(ctx, st.Root(), t.Name, dir, configPath, revision, rel, opts, out, logger)
	}

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
	runner := opts.Runner
	if runner == nil {
		// One writer for both streams keeps stdout and stderr in the order
		// they were written.
		runner = pipeline.ExecRunner{Stdout: out, Stderr: out}
	}
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
