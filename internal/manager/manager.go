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
	"time"

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
	// Progress, if set (and Echo isn't), receives a concise account of the
	// run instead: one line per stage, any warnings, and, if the run
	// fails, the last lines of the failing command's output. The full
	// output is still recorded in the run log either way.
	Progress io.Writer
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

	// How the built commit relates to its remote, so nobody has to guess
	// whether "main" meant the local branch or origin's.
	ref := "HEAD"
	if rel != nil {
		ref = rel.branch
	}
	syncNote := syncWarning(ctx, dir, ref)

	return record(ctx, st, store.Run{
		Project:  t.Name,
		Dir:      dir,
		Config:   configPath,
		Revision: revision,
		Push:     opts.Push,
	}, opts, fmt.Sprintf("of %q in %s", t.Name, dir), func(env *runEnv) error {
		if !hasRevision && relErr == nil {
			env.log.Printf("no git revision for %s — images will only be tagged \"latest\"", dir)
		}
		if relErr != nil {
			return relErr
		}
		if syncNote != "" {
			env.warnf("%s", syncNote)
		}
		return prepareAndExecute(ctx, st.Root(), t.Name, dir, configPath, revision, rel, opts, env)
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
	}, opts, fmt.Sprintf("rolling back %q's last deploy", t.Name), func(env *runEnv) error {
		cfg, err := config.Load(configPath, "")
		if err != nil {
			return &ConfigError{Err: err}
		}
		p := pipeline.New(dir, pipeline.Options{Runner: runnerFor(opts, env.out), Logger: env.log, Progress: env.stages, OnStage: env.onStage, Detail: env.detail})
		err = p.Rollback(ctx, cfg)
		recordStages(env.run, p.Stages())
		if err != nil {
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

// runEnv is what a run's body gets from record: where its output goes,
// and the run record it can add results to before it is finished.
type runEnv struct {
	// out receives command output: the run log, plus the echo and the
	// failure tail when those are on.
	out io.Writer
	// log writes timestamped lines to the run log (and the echo).
	log *log.Logger
	// progress is the concise stream, or nil (see Options.Progress).
	progress io.Writer
	// stages is where the pipeline writes its stage lines: the concise
	// stream, or nil.
	stages io.Writer
	// onStage is called as each pipeline stage begins. It writes the stage's
	// marker to the run log and discards the output kept so far, so a
	// failure's tail is that stage's output alone.
	onStage func(kind, name string)
	// detail receives lines that explain a stage's outcome but that no
	// command printed: the failure tail includes them.
	detail io.Writer
	run    *store.Run
	// warnings collects every warnf, for callers that report them
	// themselves (Plan).
	warnings []string
}

// warnf records a warning in the log and, in concise mode, on the
// progress stream, where it would otherwise be lost among hidden output.
func (e *runEnv) warnf(format string, args ...any) {
	e.warnings = append(e.warnings, fmt.Sprintf(format, args...))
	e.log.Printf("warning: "+format, args...)
	if e.progress != nil {
		fmt.Fprintf(e.progress, "warning: "+format+"\n", args...)
	}
}

// tailLines is how much of a failed command's output concise mode shows.
const tailLines = 40

// record starts r in the store, logs its ID and what it is, runs fn with
// the run's output wiring, and records how it ended. A run is recorded
// even when fn fails before doing anything, so it shows up in history.
func record(ctx context.Context, st *store.Store, r store.Run, opts Options, what string, fn func(*runEnv) error) (store.Run, error) {
	h, err := st.StartRun(r)
	if err != nil {
		return store.Run{}, err
	}

	// The tail is kept in every mode: it is recorded with a failed run, so
	// whoever reads the result later needn't fetch the log.
	tail := newTail(tailLines)
	logOut := io.Writer(h.Log)
	var progress io.Writer
	cmdOut := io.MultiWriter(h.Log, tail)
	if opts.Echo != nil {
		logOut = io.MultiWriter(h.Log, opts.Echo)
		cmdOut = io.MultiWriter(logOut, tail)
	} else if opts.Progress != nil {
		progress = opts.Progress
		fmt.Fprintf(progress, "run %s started\n", h.Run.ID)
	}
	logger := log.New(logOut, "", log.LstdFlags)
	logger.Printf("run %s %s", h.Run.ID, what)

	// Stage markers go to the run log in every mode (concise, quiet and Echo
	// alike), because `logs --stage` and a failure's tail rely on them. A
	// stage that prints no progress line still gets one and still starts a
	// fresh tail, so the tail never shows an earlier stage's output.
	env := &runEnv{out: cmdOut, log: logger, progress: progress, run: &h.Run, stages: progress}
	var stageStart time.Time
	env.onStage = func(kind, name string) {
		tail.reset()
		fmt.Fprintf(h.Log, "%s%s ==\n", StageMarker, StageMarkerText(kind, name))
		// Record the stage now, not when the run ends, so history and the
		// dashboard show a running run's progress. The final list from the
		// pipeline replaces this one (see recordStages).
		now := time.Now()
		if n := len(h.Run.Stages); n > 0 && h.Run.Stages[n-1].Status == pipeline.StageRunning {
			h.Run.Stages[n-1].Status = pipeline.StageOK
			h.Run.Stages[n-1].Seconds = now.Sub(stageStart).Round(100 * time.Millisecond).Seconds()
		}
		h.Run.Stages = append(h.Run.Stages, store.RunStage{Name: name, Kind: kind, Status: pipeline.StageRunning})
		stageStart = now
		_ = h.Save() // progress only: the final record is written by Finish
	}
	env.detail = tail
	runErr := fn(env)

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
		if lines := tailLinesOf(tail); progress != nil && len(lines) > 0 {
			fmt.Fprintf(progress, "--- last %d lines of output (full log: j-cicd logs %s) ---\n%s\n---\n", len(lines), h.Run.ID, strings.Join(lines, "\n"))
		}
		h.Run.Failure = failureOf(ctx, runErr, tailLinesOf(tail))
	} else {
		logger.Printf("run succeeded")
	}

	if err := h.Finish(status, errMsg); err != nil {
		return h.Run, errors.Join(runErr, err)
	}
	return h.Run, runErr
}

// StageMarker starts the line that marks a stage's start in the run log:
// "== stage <kind> <name> ==", e.g. "== stage step test ==" or
// "== stage build app ==".
const StageMarker = "== stage "

// StageMarkerText is what follows StageMarker for a stage: "<kind> <name>",
// or just the kind for a stage that has no name of its own (name == kind).
func StageMarkerText(kind, name string) string {
	if name == kind || name == "" {
		return kind
	}
	return kind + " " + name
}

// failureOf describes why a run failed, from the error that stopped it
// and the end of the failing stage's output.
func failureOf(ctx context.Context, err error, tail []string) *store.RunFailure {
	f := &store.RunFailure{Kind: store.FailureRun, Message: err.Error(), Tail: tail}
	var se *pipeline.StageError
	if errors.As(err, &se) {
		f.Kind, f.Stage, f.TimedOut = se.Kind, se.Stage, se.TimedOut
		if se.ExitCode >= 0 {
			code := se.ExitCode
			f.ExitCode = &code
		}
	}
	var ce *ConfigError
	switch {
	case ctx.Err() != nil:
		f.Kind = store.FailureCancelled
	case errors.As(err, &ce):
		f.Kind = store.FailureConfig
	}
	return f
}

// recordStages copies the pipeline's stage results into the run.
func recordStages(run *store.Run, stages []pipeline.StageResult) {
	run.Stages = nil // the live list onStage kept; this one has the outcomes
	for _, s := range stages {
		rs := store.RunStage{Name: s.Name, Kind: s.Kind, Status: s.Status, Seconds: s.Duration.Round(100 * time.Millisecond).Seconds(), TimedOut: s.TimedOut}
		if s.ExitCode >= 0 {
			code := s.ExitCode
			rs.ExitCode = &code
		}
		run.Stages = append(run.Stages, rs)
	}
}

func tailLinesOf(t *tailWriter) []string {
	if t == nil {
		return nil
	}
	return t.Lines()
}

// runnerFor is opts.Runner, else real processes writing to out. One
// writer for both streams keeps stdout and stderr in the order they were
// written.
func runnerFor(opts Options, out io.Writer, env ...string) pipeline.CommandRunner {
	if opts.Runner != nil {
		return opts.Runner
	}
	return pipeline.ExecRunner{Stdout: out, Stderr: out, Env: env}
}

// stepEnv is what every step and docker command gets on top of j-cicd's
// own environment: J_CICD_REVISION, the revision the images are tagged
// with, so a step can stamp it into what it builds (a version.json that a
// deploy.verify check then looks for).
func stepEnv(revision string) []string {
	if revision == "" {
		return nil
	}
	return []string{"J_CICD_REVISION=" + revision}
}

// prepared is a pipeline ready to run: the directory to run it in, its
// config, and how to clean up (remove the snapshot, if there is one).
type prepared struct {
	dir     string
	cfg     *config.Config
	cleanup func()
}

// prepare resolves what a run builds. Without a release it's the project
// directory and its config. With one it's a fresh snapshot of the release
// commit, with the config as committed there. snapDir is where the
// snapshot goes.
func prepare(ctx context.Context, snapDir, dir, configPath, revision string, rel *release, env *runEnv) (*prepared, error) {
	if rel == nil {
		cfg, err := config.Load(configPath, revision)
		if err != nil {
			return nil, &ConfigError{Err: err}
		}
		return &prepared{dir: dir, cfg: cfg, cleanup: func() {}}, nil
	}

	env.log.Printf("building the local branch %s at %s from a clean checkout (uncommitted changes and other branches in %s are not included)", rel.branch, rel.revision, dir)
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

	// The config is part of what's released, so it comes from the commit
	// too, unless it lives outside the project (--config elsewhere).
	if inside, err := filepath.Rel(dir, configPath); err == nil && inside != ".." && !strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		configPath = filepath.Join(buildDir, inside)
		if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
			cleanup()
			return nil, &ConfigError{Err: fmt.Errorf("%s at %s has no %s: commit it to %s", rel.branch, rel.revision, inside, rel.branch)}
		}
	}
	cfg, err := config.Load(configPath, revision)
	if err != nil {
		cleanup()
		return nil, &ConfigError{Err: err}
	}
	if cfg.Branch != rel.branch {
		env.warnf("the config committed on %s doesn't say branch: %s (only your working copy does) — commit it so the setting is part of the project", rel.branch, rel.branch)
	}
	return &prepared{dir: buildDir, cfg: cfg, cleanup: cleanup}, nil
}

// prepareAndExecute runs the pipeline in dir, or, when rel is set, in a
// fresh snapshot of the release commit that is removed afterwards.
func prepareAndExecute(ctx context.Context, root, project, dir, configPath, revision string, rel *release, opts Options, env *runEnv) error {
	prep, err := prepare(ctx, snapshotDir(root, project), dir, configPath, revision, rel, env)
	if err != nil {
		return err
	}
	defer prep.cleanup()
	return execute(ctx, prep.dir, prep.cfg, revision, opts, env)
}

func execute(ctx context.Context, dir string, cfg *config.Config, revision string, opts Options, env *runEnv) error {
	p := pipeline.New(dir, pipeline.Options{
		Push:        opts.Push,
		Deploy:      opts.Deploy,
		Revision:    revision,
		DeployDirty: opts.DeployDirty,
		Runner:      runnerFor(opts, env.out, stepEnv(revision)...),
		Logger:      env.log,
		Progress:    env.stages,
		OnStage:     env.onStage,
		Detail:      env.detail,
	})
	err := p.Run(ctx, cfg)
	recordStages(env.run, p.Stages())
	for _, img := range p.Images() {
		env.run.Images = append(env.run.Images, store.RunImage{Name: img.Name, Repository: img.Repository, Tags: img.Tags, Digest: img.Digest})
	}
	for _, v := range p.Verified() {
		env.run.Verified = append(env.run.Verified, store.RunVerify{URL: v.URL, Attempts: v.Attempts, Seconds: v.Duration.Round(100 * time.Millisecond).Seconds()})
	}
	for _, a := range p.Artifacts() {
		env.run.Artifacts = append(env.run.Artifacts, store.RunArtifact{Name: a.Name, Path: filepath.ToSlash(a.Path), SHA256: a.SHA256, Size: a.Size})
	}
	if p.Deployed() {
		env.run.Deployed, env.run.DeployHost = true, cfg.Deploy.Target()
	}
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("cancelled: %w", err)
		}
		return err
	}
	return nil
}
