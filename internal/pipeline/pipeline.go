// Package pipeline runs a project's steps in order and packages its
// images, stopping at the first failure.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
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
	// HTTPClient makes the post-deploy verify requests. Nil means a client
	// with a short per-request timeout.
	HTTPClient *http.Client
	// VerifyInterval is the pause between verify attempts. Zero means
	// DefaultVerifyInterval.
	VerifyInterval time.Duration
	// Progress, if set, receives one short line per stage ("building
	// image backend") — what a caller that hides the commands' own output
	// still shows. Nil means none.
	Progress io.Writer
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
	progress    io.Writer
	httpClient  *http.Client
	verifyEvery time.Duration
	// Results a caller can read once Run returns.
	images    []ImageResult
	artifacts []ArtifactResult
	deployed  bool
	verified  []VerifyResult
	stages    []StageResult
	// inspectDocker diagnoses an unreachable Docker daemon. Tests replace
	// it; the real one probes the local socket.
	inspectDocker func() dockerDiagnosis
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
		progress:    opts.Progress,
		httpClient:  opts.HTTPClient,
		verifyEvery: opts.VerifyInterval,

		inspectDocker: inspectDockerHost,
	}
	if p.runner == nil {
		p.runner = ExecRunner{}
	}
	if p.log == nil {
		p.log = log.Default()
	}
	if p.httpClient == nil {
		p.httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	if p.verifyEvery <= 0 {
		p.verifyEvery = DefaultVerifyInterval
	}
	return p
}

// Run executes cfg's steps in order, then builds (and optionally pushes)
// each configured image, then (optionally) has the deploy host roll out
// what was pushed. It stops at the first failure, and returns ctx.Err()
// if the run is cancelled.
func (p *Pipeline) Run(ctx context.Context, cfg *config.Config) error {
	err := p.run(ctx, cfg)
	p.closeStage(err)
	return err
}

func (p *Pipeline) run(ctx context.Context, cfg *config.Config) error {
	p.log.Printf("running pipeline for %q", cfg.Name)

	deploying := cfg.Deploy != nil && p.push && p.deploy
	var deployRefs []string
	if deploying {
		// Checked up front: finding out after a long build and a push
		// that the result can't be deployed is the expensive way round.
		p.stage(KindPreflight, "deploy-check")
		refs, err := p.deployRefs(cfg)
		if err != nil {
			return stageErr(KindPreflight, "deploy-check", err, "cannot deploy this build: %v", err)
		}
		deployRefs = refs
	}

	if len(cfg.Images) > 0 {
		// Before the steps, not at the first docker build: a daemon that
		// isn't reachable should fail the run in a second, with the
		// reason as the run's error, not after a full build and test.
		p.stage(KindDocker, "docker")
		if err := p.checkDocker(ctx); err != nil {
			return stageErr(KindDocker, "docker", err, "%v", err)
		}
	}

	for i := range cfg.Steps {
		step := &cfg.Steps[i]
		dir := p.projectDir
		if step.Dir != "" {
			dir = filepath.Join(p.projectDir, step.Dir)
		}
		p.stage(KindStep, step.Name)
		p.say("step %s", step.Name)
		p.log.Printf("[%s] %s", step.Name, step.Run)
		timeout := cfg.StepTimeout(step, DefaultTimeout)
		if err := p.exec(ctx, timeout, dir, "sh", "-c", step.Run); err != nil {
			return stageErr(KindStep, step.Name, err, "step %q failed: %v", step.Name, err)
		}
	}

	timeout := cfg.ImageTimeout(DefaultTimeout)
	for i := range cfg.Images {
		img := &cfg.Images[i]
		p.stage(KindBuild, img.Label())
		p.say("building image %s", img.Label())
		if err := p.buildImage(ctx, timeout, img); err != nil {
			return stageErr(KindBuild, img.Label(), err, "docker build (%s) failed: %v", img.Label(), err)
		}
		if !p.push {
			p.log.Printf("skipping push for %s (--no-push)", img.Label())
			continue
		}
		p.stage(KindPush, img.Label())
		p.say("pushing image %s", img.Label())
		if err := p.pushImage(ctx, timeout, img); err != nil {
			return stageErr(KindPush, img.Label(), err, "docker push (%s) failed: %v", img.Label(), err)
		}
		p.images = append(p.images, ImageResult{
			Name:       img.Label(),
			Repository: img.Repository,
			Tags:       img.Tags,
			Digest:     p.pushedDigest(ctx, img),
		})
	}

	if len(cfg.Artifacts) > 0 {
		p.stage(KindArtifacts, "artifacts")
	}
	if err := p.collectArtifacts(cfg); err != nil {
		return stageErr(KindArtifacts, "artifacts", err, "%v", err)
	}

	switch {
	case cfg.Deploy == nil:
	case !p.push:
		p.log.Printf("skipping deploy (--no-push: a build-only run publishes nothing)")
	case !p.deploy:
		p.log.Printf("skipping deploy (--no-deploy)")
	default:
		target := cfg.Deploy.Target()
		p.stage(KindDeploy, target)
		p.say("deploying to %s", target)
		if err := p.runDeploy(ctx, cfg, deployRefs); err != nil {
			return stageErr(KindDeploy, target, err, "deploy to %s failed: %v", target, err)
		}
		p.deployed = true
		if len(cfg.Deploy.Verify) > 0 {
			p.stage(KindVerify, target)
		}
		if err := p.verify(ctx, cfg); err != nil {
			return stageErr(KindVerify, target, err, "deployed to %s, but %v (the new release is running; revert it with: j-cicd rollback %s)", target, err, cfg.Name)
		}
	}

	p.log.Printf("pipeline for %q completed successfully", cfg.Name)
	return nil
}

// say writes one progress line, if anyone is listening.
func (p *Pipeline) say(format string, args ...any) {
	if p.progress != nil {
		fmt.Fprintf(p.progress, "• "+format+"\n", args...)
	}
}

// ImageResult is an image Run pushed.
type ImageResult struct {
	Name, Repository string
	Tags             []string
	// Digest is the registry's content hash (sha256:...), "" if it
	// couldn't be read back.
	Digest string
}

// Images returns the images the last Run pushed, in images: order.
func (p *Pipeline) Images() []ImageResult { return p.images }

// Deployed reports whether the last Run's deploy stage completed.
func (p *Pipeline) Deployed() bool { return p.deployed }

// pushedDigest reads back the registry digest the push of img resolved to
// (docker records it as a RepoDigest once pushed). It's informational, so
// a failure logs and yields "" rather than failing a run that pushed fine.
func (p *Pipeline) pushedDigest(ctx context.Context, img *config.Image) string {
	if len(img.Tags) == 0 {
		return ""
	}
	out, err := p.output(ctx, time.Minute, "", "docker", "image", "inspect",
		"--format", "{{range .RepoDigests}}{{println .}}{{end}}", ref(img, img.Tags[0]))
	if err != nil {
		p.log.Printf("[%s] warning: couldn't read back the pushed digest: %v", img.Label(), err)
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if digest, ok := strings.CutPrefix(strings.TrimSpace(line), img.Repository+"@"); ok {
			return digest
		}
	}
	return ""
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
		return nil, &timeoutError{after: timeout}
	}
	return out, err
}
