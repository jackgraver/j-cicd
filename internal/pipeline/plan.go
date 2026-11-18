package pipeline

import (
	"path/filepath"
	"strings"

	"j-cicd/internal/config"
)

// Plan is what a run with these options would do, worked out without
// running anything: no command executes, nothing is built, pushed or
// deployed.
type Plan struct {
	Steps  []PlanStep  `json:"steps"`
	Images []PlanImage `json:"images"`
	// Artifacts are the files the steps must produce.
	Artifacts []PlanArtifact `json:"artifacts,omitempty"`
	// Deploy is nil when the run wouldn't deploy (no deploy: section,
	// --no-push or --no-deploy); Skipped then says why.
	Deploy  *PlanDeploy `json:"deploy,omitempty"`
	Skipped []string    `json:"skipped,omitempty"`
	// Problems are things that would make the run fail or be refused.
	Problems []string `json:"problems,omitempty"`
}

// PlanStep is one shell step.
type PlanStep struct {
	Name string `json:"name"`
	Run  string `json:"run"`
	Dir  string `json:"dir,omitempty"`
}

// PlanImage is one image build.
type PlanImage struct {
	Name       string   `json:"name"`
	Repository string   `json:"repository"`
	Tags       []string `json:"tags"`
	Dockerfile string   `json:"dockerfile"`
	Push       bool     `json:"push"`
}

// PlanArtifact is one file the steps must leave behind.
type PlanArtifact struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// PlanDeploy is the deploy stage.
type PlanDeploy struct {
	// Host is the ssh destination, or "local" for a local deploy.
	Host    string   `json:"host"`
	Project string   `json:"project"`
	Command string   `json:"command"`
	Images  []string `json:"images"`
	// Artifacts are the files sent instead of images, as name=path; their
	// hashes are known only once the steps have built them.
	Artifacts []string `json:"artifacts,omitempty"`
	// Compose is the compose file sent along, if any.
	Compose string `json:"compose_file,omitempty"`
	// Verify are the URLs checked after the deploy.
	Verify []string `json:"verify,omitempty"`
}

// Plan describes what Run(cfg) would do with the pipeline's options.
func (p *Pipeline) Plan(cfg *config.Config) Plan {
	var plan Plan
	for _, s := range cfg.Steps {
		plan.Steps = append(plan.Steps, PlanStep{Name: s.Name, Run: s.Run, Dir: s.Dir})
	}
	for _, img := range cfg.Images {
		plan.Images = append(plan.Images, PlanImage{
			Name: img.Label(), Repository: img.Repository, Tags: img.Tags,
			Dockerfile: filepath.ToSlash(img.Dockerfile), Push: p.push,
		})
	}
	for _, a := range cfg.Artifacts {
		plan.Artifacts = append(plan.Artifacts, PlanArtifact{Name: a.Name, Path: filepath.ToSlash(a.Path)})
	}

	switch {
	case cfg.Deploy == nil:
	case !p.push:
		plan.Skipped = append(plan.Skipped, "deploy (--no-push: a build-only run publishes nothing)")
	case !p.deploy:
		plan.Skipped = append(plan.Skipped, "deploy (--no-deploy)")
	default:
		refs, err := p.deployRefs(cfg)
		if err != nil {
			plan.Problems = append(plan.Problems, "cannot deploy this build: "+err.Error())
			break
		}
		var verify []string
		for _, v := range cfg.Deploy.Verify {
			verify = append(verify, expandRevision(v.URL, p.revision))
		}
		var arts []string
		for _, a := range cfg.DeployArtifacts() {
			arts = append(arts, a.Name+"="+filepath.ToSlash(a.Path))
		}
		plan.Deploy = &PlanDeploy{Verify: verify,
			Host: cfg.Deploy.Target(), Project: cfg.Deploy.Project, Command: strings.TrimSpace(cfg.Deploy.Command),
			Images: refs, Artifacts: arts, Compose: cfg.Deploy.ComposeFile,
		}
	}
	return plan
}
