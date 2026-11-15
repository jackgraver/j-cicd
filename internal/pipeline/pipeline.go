// Package pipeline runs a project's steps in order and packages its
// images, stopping at the first failure.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"j-cicd/internal/config"
)

// DefaultTimeout bounds a single step or docker command when the project's
// config doesn't say otherwise. It exists so a command that waits on
// something that never arrives can't pin the runner forever; projects with
// genuinely long builds can raise it (or set `timeout: 0` for no limit)
// in their own config.
const DefaultTimeout = 60 * time.Minute

// Options are the parts of a run that come from the invocation rather than
// from the project's config.
type Options struct {
	// Push reports whether built images should be pushed to their
	// registry (false for --no-push).
	Push bool
	// Runner executes the individual commands. Nil means ExecRunner —
	// tests substitute a fake here.
	Runner CommandRunner
	// Logger receives progress output. Nil means the standard logger.
	Logger *log.Logger
}

// Pipeline runs one project's pipeline.
type Pipeline struct {
	projectDir string
	push       bool
	runner     CommandRunner
	log        *log.Logger
}

// New returns a Pipeline that runs commands against projectDir.
func New(projectDir string, opts Options) *Pipeline {
	p := &Pipeline{
		projectDir: projectDir,
		push:       opts.Push,
		runner:     opts.Runner,
		log:        opts.Logger,
	}
	if p.runner == nil {
		p.runner = ExecRunner{}
	}
	if p.log == nil {
		p.log = log.Default()
	}
	return p
}

// Run executes cfg's steps in order, then builds (and optionally pushes)
// each configured image. It stops at the first failure, and returns
// ctx.Err() if the run is cancelled.
func (p *Pipeline) Run(ctx context.Context, cfg *config.Config) error {
	p.log.Printf("running pipeline for %q", cfg.Name)

	for i := range cfg.Steps {
		step := &cfg.Steps[i]
		dir := p.projectDir
		if step.Dir != "" {
			dir = filepath.Join(p.projectDir, step.Dir)
		}
		p.log.Printf("[%s] %s", step.Name, step.Run)
		timeout := cfg.StepTimeout(step, DefaultTimeout)
		if err := p.exec(ctx, timeout, dir, "sh", "-c", step.Run); err != nil {
			return fmt.Errorf("step %q failed: %w", step.Name, err)
		}
	}

	timeout := cfg.ImageTimeout(DefaultTimeout)
	for i := range cfg.Images {
		img := &cfg.Images[i]
		if err := p.buildImage(ctx, timeout, img); err != nil {
			return fmt.Errorf("docker build (%s) failed: %w", img.Label(), err)
		}
		if !p.push {
			p.log.Printf("skipping push for %s (--no-push)", img.Label())
			continue
		}
		if err := p.pushImage(ctx, timeout, img); err != nil {
			return fmt.Errorf("docker push (%s) failed: %w", img.Label(), err)
		}
	}

	p.log.Printf("pipeline for %q completed successfully", cfg.Name)
	return nil
}

// exec runs one command under its own deadline, turning an expired
// deadline into a message that says so rather than the runner's bare
// "signal: killed".
func (p *Pipeline) exec(ctx context.Context, timeout time.Duration, dir, name string, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	err := p.runner.Run(ctx, dir, name, args...)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s", timeout)
	}
	return err
}
