// Package config defines the .cicd.yaml schema that a project uses to
// describe its own pipeline, plus loading, defaulting, and validation of
// it. It is shared by the runner and the VPS-side deploy agent, which
// validates project names the same way.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultFile is the pipeline config j-cicd looks for in a project's root
// directory when --config isn't given explicitly.
const DefaultFile = ".cicd.yaml"

// RemoteNone as remote: builds the local release branch even when it
// tracks a remote.
const RemoteNone = "none"

// Config describes the pipeline for a single project. Every project that
// wants to use j-cicd provides its own one of these — nothing here is
// specific to any particular project.
type Config struct {
	Name string `yaml:"name"`
	// Branch, when set, is the release branch: every run builds a clean
	// checkout of that branch's latest commit instead of the project
	// directory as it is on disk, so uncommitted work and whatever branch
	// happens to be checked out can never reach a registry or the VPS.
	Branch string `yaml:"branch,omitempty"`
	// Remote, with branch: set, makes a run fetch that branch from this git
	// remote and build the remote's copy rather than the local branch, so
	// a PR merged there ships without anyone pulling first. Unset, it is
	// the remote the local branch tracks (if any); RemoteNone builds the
	// local branch.
	Remote string `yaml:"remote,omitempty"`
	// Timeout is the default time limit for each individual step and
	// docker command. Unset means the runner's built-in default; an
	// explicit 0 means no limit.
	Timeout *Duration `yaml:"timeout,omitempty"`
	Steps   []Step    `yaml:"steps,omitempty"`
	Images  []Image   `yaml:"images,omitempty"`
	// Artifacts are files the steps produce that are the release (for a
	// project that ships binaries rather than images).
	Artifacts []Artifact `yaml:"artifacts,omitempty"`
	// Deploy, when set, hands the pushed images (or the artifacts) to the
	// deploy agent as the last stage of the run.
	Deploy *Deploy `yaml:"deploy,omitempty"`
}

// Step is a single shell command run as part of the pipeline (build, test,
// lint, etc). Steps run in the order they're declared and the pipeline
// stops at the first failure.
type Step struct {
	Name string `yaml:"name,omitempty"`
	Run  string `yaml:"run"`
	Dir  string `yaml:"dir,omitempty"`
	// Timeout overrides the config-level timeout for this step. An
	// explicit 0 means no limit.
	Timeout *Duration `yaml:"timeout,omitempty"`
}

// Image describes how to package one service as a container image and
// where to push it. A project can have several of these (e.g. a separate
// frontend/backend each with their own Dockerfile). Building/pushing is
// delegated to the docker CLI, so registry auth is whatever `docker login`
// already has configured on the host — j-cicd doesn't handle credentials
// itself.
type Image struct {
	Name       string   `yaml:"name,omitempty"`
	Repository string   `yaml:"repository"`
	Dockerfile string   `yaml:"dockerfile,omitempty"`
	Context    string   `yaml:"context,omitempty"`
	Tags       []string `yaml:"tags,omitempty"`
}

// Label returns a human-readable name for logging: the configured name,
// or the repository if none was given.
func (img *Image) Label() string {
	if img.Name != "" {
		return img.Name
	}
	return img.Repository
}

// Duration is a time.Duration that reads from YAML as a duration string
// ("30s", "10m", "1h30m") rather than a raw nanosecond count.
type Duration struct {
	time.Duration
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("must be a duration string like \"10m\"")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: must look like \"30s\", \"10m\" or \"1h30m\"", s)
	}
	if parsed < 0 {
		return fmt.Errorf("invalid duration %q: must not be negative", s)
	}
	d.Duration = parsed
	return nil
}

// MarshalYAML implements yaml.Marshaler, so a generated config round-trips
// as a duration string.
func (d Duration) MarshalYAML() (any, error) {
	return d.String(), nil
}

// Load reads, defaults, and validates a pipeline config from path.
//
// revision is an immutable identifier for the tree being built (in
// practice a short git SHA). When an image declares no tags of its own it
// gets "latest" plus revision, so every build leaves behind a tag that
// still refers to this exact image after the next push moves "latest".
// Pass "" when no revision is known; images then default to "latest"
// alone.
func Load(path, revision string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Unknown keys are a hard error: this config decides what gets
	// published to a registry, and a typo like "dockerFile:" would
	// otherwise be silently replaced by a default.
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: config is empty", path)
		}
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	if err := cfg.applyDefaults(revision); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// applyDefaults fills in omitted fields and rejects configs that would
// otherwise run as something other than what they say.
func (c *Config) applyDefaults(revision string) error {
	if c.Name == "" {
		return errors.New("name is required")
	}
	// git would read a leading dash as an option, and a space can't be in
	// a branch name; anything subtler is git's to reject when resolving it.
	if c.Branch != "" && (strings.HasPrefix(c.Branch, "-") || strings.ContainsAny(c.Branch, " \t\n")) {
		return fmt.Errorf("branch %q is not a valid branch name", c.Branch)
	}
	if c.Remote != "" {
		if c.Branch == "" {
			return errors.New("remote: needs branch: (the branch to fetch from it, or to build locally with remote: none)")
		}
		if strings.HasPrefix(c.Remote, "-") || strings.ContainsAny(c.Remote, " \t\n:") {
			return fmt.Errorf("remote %q is not a valid remote name", c.Remote)
		}
	}
	if len(c.Steps) == 0 && len(c.Images) == 0 && len(c.Artifacts) == 0 {
		return errors.New("must define at least one step, image or artifact")
	}

	for i := range c.Steps {
		step := &c.Steps[i]
		// A step with no command would run as `sh -c ""` and exit 0 —
		// "no test ran" must never look like "tests passed".
		if strings.TrimSpace(step.Run) == "" {
			return fmt.Errorf("steps[%d].run is required", i)
		}
		if step.Name == "" {
			step.Name = fmt.Sprintf("step %d", i+1)
		}
	}

	// refs maps a fully-qualified repository:tag to the index of the image
	// that claimed it, so two images can't push to the same ref and have
	// the last one silently win.
	refs := make(map[string]int, len(c.Images))
	for i := range c.Images {
		img := &c.Images[i]
		if img.Repository == "" {
			return fmt.Errorf("images[%d].repository is required", i)
		}
		if img.Dockerfile == "" {
			img.Dockerfile = "Dockerfile"
		}
		if img.Context == "" {
			img.Context = "."
		}
		if len(img.Tags) == 0 {
			img.Tags = []string{"latest"}
			if revision != "" {
				img.Tags = append(img.Tags, revision)
			}
		}
		for _, tag := range img.Tags {
			if tag == "" {
				return fmt.Errorf("images[%d].tags contains an empty tag", i)
			}
			ref := img.Repository + ":" + tag
			if first, dup := refs[ref]; dup {
				return fmt.Errorf("images[%d] and images[%d] both push %s: give them distinct repositories or tags", first, i, ref)
			}
			refs[ref] = i
		}
	}

	if err := c.validateArtifacts(); err != nil {
		return err
	}
	if c.Deploy != nil {
		if err := c.Deploy.applyDefaults(c); err != nil {
			return fmt.Errorf("deploy: %w", err)
		}
	}
	return nil
}

// StepTimeout returns the effective time limit for a step: its own
// timeout, else the config-level default, else fallback. A zero duration
// means no limit.
func (c *Config) StepTimeout(step *Step, fallback time.Duration) time.Duration {
	if step.Timeout != nil {
		return step.Timeout.Duration
	}
	if c.Timeout != nil {
		return c.Timeout.Duration
	}
	return fallback
}

// ImageTimeout returns the effective time limit for commands that aren't
// steps (the docker build/push stage).
func (c *Config) ImageTimeout(fallback time.Duration) time.Duration {
	if c.Timeout != nil {
		return c.Timeout.Duration
	}
	return fallback
}
