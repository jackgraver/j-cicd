package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// defaultConfigFile is the pipeline config j-cicd looks for in a project's
// root directory when --config isn't given explicitly.
const defaultConfigFile = ".cicd.yaml"

// Config describes the pipeline for a single project. Every project that
// wants to use j-cicd provides its own one of these — nothing here is
// specific to any particular project.
type Config struct {
	Name  string       `yaml:"name"`
	Steps []Step       `yaml:"steps"`
	Image *ImageConfig `yaml:"image,omitempty"`
}

// Step is a single shell command run as part of the pipeline (build, test,
// lint, etc). Steps run in the order they're declared and the pipeline
// stops at the first failure.
type Step struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
	Dir  string `yaml:"dir,omitempty"`
}

// ImageConfig describes how to package the project as a container image and
// where to push it. Building/pushing is delegated to the docker CLI, so
// registry auth is whatever `docker login` already has configured on the
// host — j-cicd doesn't handle credentials itself.
type ImageConfig struct {
	Repository string   `yaml:"repository"`
	Dockerfile string   `yaml:"dockerfile,omitempty"`
	Context    string   `yaml:"context,omitempty"`
	Tags       []string `yaml:"tags,omitempty"`
}

// LoadConfig reads and validates a pipeline config from path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	if cfg.Name == "" {
		return nil, fmt.Errorf("%s: name is required", path)
	}
	if len(cfg.Steps) == 0 && cfg.Image == nil {
		return nil, fmt.Errorf("%s: must define at least one step or an image block", path)
	}

	if cfg.Image != nil {
		if cfg.Image.Repository == "" {
			return nil, fmt.Errorf("%s: image.repository is required", path)
		}
		if cfg.Image.Dockerfile == "" {
			cfg.Image.Dockerfile = "Dockerfile"
		}
		if cfg.Image.Context == "" {
			cfg.Image.Context = "."
		}
		if len(cfg.Image.Tags) == 0 {
			cfg.Image.Tags = []string{"latest"}
		}
	}

	return &cfg, nil
}
