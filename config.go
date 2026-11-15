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
	Name   string        `yaml:"name"`
	Steps  []Step        `yaml:"steps"`
	Images []ImageConfig `yaml:"images,omitempty"`
}

// Step is a single shell command run as part of the pipeline (build, test,
// lint, etc). Steps run in the order they're declared and the pipeline
// stops at the first failure.
type Step struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
	Dir  string `yaml:"dir,omitempty"`
}

// ImageConfig describes how to package one service as a container image and
// where to push it. A project can have several of these (e.g. a separate
// frontend/backend each with their own Dockerfile). Building/pushing is
// delegated to the docker CLI, so registry auth is whatever `docker login`
// already has configured on the host — j-cicd doesn't handle credentials
// itself.
type ImageConfig struct {
	Name       string   `yaml:"name,omitempty"`
	Repository string   `yaml:"repository"`
	Dockerfile string   `yaml:"dockerfile,omitempty"`
	Context    string   `yaml:"context,omitempty"`
	Tags       []string `yaml:"tags,omitempty"`
}

// Label returns a human-readable name for logging: the configured name,
// or the repository if none was given.
func (img *ImageConfig) Label() string {
	if img.Name != "" {
		return img.Name
	}
	return img.Repository
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
	if len(cfg.Steps) == 0 && len(cfg.Images) == 0 {
		return nil, fmt.Errorf("%s: must define at least one step or image", path)
	}

	for i := range cfg.Images {
		img := &cfg.Images[i]
		if img.Repository == "" {
			return nil, fmt.Errorf("%s: images[%d].repository is required", path, i)
		}
		if img.Dockerfile == "" {
			img.Dockerfile = "Dockerfile"
		}
		if img.Context == "" {
			img.Context = "."
		}
		if len(img.Tags) == 0 {
			img.Tags = []string{"latest"}
		}
	}

	return &cfg, nil
}
