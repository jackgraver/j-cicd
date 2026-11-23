package manager

import (
	"context"
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

// Plan is what Run would do for a target, worked out without running a
// single step, building or pushing anything, or recording a run.
type Plan struct {
	Project string `json:"project"`
	Dir     string `json:"dir"`
	// Branch is the release branch being built ("" when the project
	// directory is built as it is on disk).
	Branch   string `json:"branch,omitempty"`
	Revision string `json:"revision,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
	// Busy is the ID of a run already in progress, which this one would
	// have to wait out or be refused for.
	Busy string `json:"busy_run,omitempty"`
	// Upstream is what the built branch tracks ("" if nothing), and
	// Ahead/Behind how many commits it differs by, as of the last fetch.
	// j-cicd builds the local branch whatever these say.
	Upstream string `json:"upstream,omitempty"`
	Ahead    int    `json:"ahead,omitempty"`
	Behind   int    `json:"behind,omitempty"`
	// Remote is the git remote the branch was fetched from ("" for the
	// local branch). Revision is the commit `run --commit` pins to.
	Remote   string   `json:"remote,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	pipeline.Plan
}

// Describe returns the plan as text for a person (or an agent) to read.
func (p *Plan) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "plan for %q (nothing was run)\n", p.Project)
	switch {
	case p.Branch != "" && p.Remote != "" && p.Revision != "":
		fmt.Fprintf(&b, "  source:  %s/%s at %s (fetched just now), from a clean checkout\n", p.Remote, p.Branch, p.Revision)
	case p.Branch != "" && p.Revision != "":
		fmt.Fprintf(&b, "  source:  local branch %s at %s, from a clean checkout (not origin's copy)\n", p.Branch, p.Revision)
	case p.Revision != "":
		fmt.Fprintf(&b, "  source:  %s as it is on disk, revision %s\n", p.Dir, p.Revision)
	default:
		fmt.Fprintf(&b, "  source:  %s as it is on disk (no git revision)\n", p.Dir)
	}
	switch {
	case p.Remote != "":
		// Built from the remote itself: only unpushed commits matter, and
		// those come through as a warning.
	case p.Upstream == "" && p.Revision != "":
		fmt.Fprintf(&b, "  sync:    no upstream configured, so there is nothing to compare the local commit with\n")
	case p.Upstream != "" && p.Ahead == 0 && p.Behind == 0:
		fmt.Fprintf(&b, "  sync:    in sync with %s (as of the last fetch)\n", p.Upstream)
	}
	if p.Branch != "" && p.Revision != "" {
		fmt.Fprintf(&b, "  pin:     j-cicd run %s --commit %s builds exactly this commit, even if the branch moves\n", p.Project, p.Revision)
	}
	for i, s := range p.Steps {
		fmt.Fprintf(&b, "  step %d:  %s: %s\n", i+1, s.Name, s.Run)
	}
	for _, img := range p.Images {
		action := "build"
		if img.Push {
			action = "build + push"
		}
		fmt.Fprintf(&b, "  image:   %s -> %s [%s] (%s)\n", img.Name, img.Repository, strings.Join(img.Tags, ", "), action)
	}
	for _, a := range p.Artifacts {
		fmt.Fprintf(&b, "  artifact: %s <- %s (must exist after the steps)\n", a.Name, a.Path)
	}
	if d := p.Deploy; d != nil {
		via := "ssh " + d.Host
		if d.Host == config.LocalTarget {
			via = "locally:"
		}
		what := strings.Join(d.Images, " ")
		if len(d.Artifacts) > 0 {
			what = strings.Join(d.Artifacts, " ") + " (sent by sha256 on stdin)"
		}
		fmt.Fprintf(&b, "  deploy:  %s %s deploy %s %s\n", via, d.Command, d.Project, what)
	}
	if d := p.Deploy; d != nil {
		for _, u := range d.Verify {
			fmt.Fprintf(&b, "  verify:  GET %s until it serves this release\n", u)
		}
	}
	for _, s := range p.Skipped {
		fmt.Fprintf(&b, "  skipped: %s\n", s)
	}
	if p.Busy != "" {
		fmt.Fprintf(&b, "  busy:    run %s is in progress for this project\n", p.Busy)
	}
	for _, w := range p.Warnings {
		fmt.Fprintf(&b, "  warning: %s\n", w)
	}
	for _, pr := range p.Problems {
		fmt.Fprintf(&b, "  PROBLEM: %s\n", pr)
	}
	return b.String()
}

// MakePlan works out what Run(t, opts) would do. It takes no lock and
// records nothing; a release branch is checked out into a throwaway
// worktree to read the config and compose file as committed there, so the
// plan is of exactly what a run would build. Problems that would make a
// run fail are in Plan.Problems rather than an error; the error is for a
// config that doesn't load at all (a *ConfigError).
func MakePlan(ctx context.Context, st *store.Store, t Target, opts Options) (*Plan, error) {
	dir, configPath, err := absPaths(t)
	if err != nil {
		return nil, err
	}
	revision, _ := detect.GitRevision(dir)
	plan := &Plan{Project: t.Name, Dir: dir, Revision: revision}

	cfg, err := config.Load(configPath, revision)
	if err != nil {
		return nil, &ConfigError{Err: err}
	}
	if active, busy, _ := Busy(st, t.Name); busy {
		plan.Busy = active.ID
	}

	var rel *release
	if cfg.Branch != "" {
		if rel, err = resolveRelease(ctx, dir, cfg.Branch, cfg.Remote, opts.Commit); err != nil {
			plan.Problems = append(plan.Problems, err.Error())
			return plan, nil
		}
		plan.Branch, plan.Remote, plan.Revision = rel.branch, rel.remote, rel.revision
	} else if opts.Commit != "" {
		plan.Problems = append(plan.Problems, "--commit needs branch: in the config, so there is a release history to take the commit from")
		return plan, nil
	}
	plan.Dirty = rel == nil && strings.HasSuffix(plan.Revision, "-dirty")
	if s, ok := releaseSync(ctx, dir, rel); ok {
		plan.Upstream, plan.Ahead, plan.Behind = s.Upstream, s.Ahead, s.Behind
		if note := syncWarning(ctx, dir, rel); note != "" {
			plan.Warnings = append(plan.Warnings, note)
		}
	}

	env := &runEnv{out: io.Discard, log: log.New(io.Discard, "", 0), run: &store.Run{}}
	snapDir := filepath.Join(st.Root(), "plans", fmt.Sprintf("%s-%d", t.Name, os.Getpid()))
	prep, err := prepare(ctx, snapDir, dir, configPath, plan.Revision, rel, env)
	if err != nil {
		return nil, err
	}
	defer prep.cleanup()
	plan.Warnings = append(plan.Warnings, env.warnings...)

	p := pipeline.New(prep.dir, pipeline.Options{
		Push: opts.Push, Deploy: opts.Deploy, Revision: plan.Revision, DeployDirty: opts.DeployDirty,
	})
	plan.Plan = p.Plan(prep.cfg)
	return plan, nil
}
