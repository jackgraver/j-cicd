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
	// Deploy reports whether the config's deploy: stage should run
	// (false for --no-deploy). It never runs without Push: the VPS can
	// only pull what was pushed.
	Deploy bool
	// Revision is the per-commit tag this run's images get (from
	// detect.GitRevision, "" if unknown). It is what the deploy stage
	// hands to the VPS.
	Revision string
	// DeployDirty allows deploying a build of uncommitted changes (a
	// "-dirty" revision). Off by default so work in progress can't reach
	// production by accident.
	DeployDirty bool
	// Runner executes the individual commands. Nil means ExecRunner —
	// tests substitute a fake here.
	Runner CommandRunner
	// Logger receives progress output. Nil means the standard logger.
	Logger *log.Logger
}

// Pipeline runs one project's pipeline.
type Pipeline struct {
	projectDir  string
	push        bool
	deploy      bool
	revision    string
	deployDirty bool
	runner      CommandRunner
	log         *log.Logger
}

// New returns a Pipeline that runs commands against projectDir.
func New(projectDir string, opts Options) *Pipeline {
	p := &Pipeline{
		projectDir:  projectDir,
		push:        opts.Push,
		deploy:      opts.Deploy,
		revision:    opts.Revision,
		deployDirty: opts.DeployDirty,
		runner:      opts.Runner,
		log:         opts.Logger,
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
// each configured image, then (optionally) has the deploy host roll out
// what was pushed. It stops at the first failure, and returns ctx.Err()
// if the run is cancelled.
func (p *Pipeline) Run(ctx context.Context, cfg *config.Config) error {
	p.log.Printf("running pipeline for %q", cfg.Name)

	deploying := cfg.Deploy != nil && p.push && p.deploy
	var deployRefs []string
	if deploying {
		// Checked up front: finding out after a long build and a push
		// that the result can't be deployed is the expensive way round.
		refs, err := p.deployRefs(cfg)
		if err != nil {
			return fmt.Errorf("cannot deploy this build: %w", err)
		}
		deployRefs = refs
	}

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

	switch {
	case cfg.Deploy == nil:
	case !p.push:
		p.log.Printf("skipping deploy (--no-push: nothing new in the registry to deploy)")
	case !p.deploy:
		p.log.Printf("skipping deploy (--no-deploy)")
	default:
		if err := p.runDeploy(ctx, cfg, deployRefs); err != nil {
			return fmt.Errorf("deploy to %s failed: %w", cfg.Deploy.Host, err)
		}
	}

	p.log.Printf("pipeline for %q completed successfully", cfg.Name)
	return nil
}

// exec runs one command under its own deadline, turning an expired
// deadline into a message that says so rather than the runner's bare
// "signal: killed".
func (p *Pipeline) exec(ctx context.Context, timeout time.Duration, dir, name string, args ...string) error {
	_, err := withDeadline(ctx, timeout, func(ctx context.Context) ([]byte, error) {
		return nil, p.runner.Run(ctx, dir, name, args...)
	})
	return err
}

// output is exec for a command whose stdout the caller needs.
func (p *Pipeline) output(ctx context.Context, timeout time.Duration, dir, name string, args ...string) ([]byte, error) {
	return withDeadline(ctx, timeout, func(ctx context.Context) ([]byte, error) {
		return p.runner.Output(ctx, dir, name, args...)
	})
}

func withDeadline(ctx context.Context, timeout time.Duration, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	out, err := fn(ctx)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("timed out after %s", timeout)
	}
	return out, err
}
