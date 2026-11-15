package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// RunOptions controls behavior of a single pipeline run that isn't part of
// the project's own config (e.g. whether this particular invocation should
// push).
type RunOptions struct {
	Push bool
}

// RunPipeline executes a project's steps in order, then builds (and
// optionally pushes) its image if one is configured. It stops at the first
// failure.
func RunPipeline(projectDir string, cfg *Config, opts RunOptions) error {
	log.Printf("running pipeline for %q", cfg.Name)

	for _, step := range cfg.Steps {
		dir := projectDir
		if step.Dir != "" {
			dir = filepath.Join(projectDir, step.Dir)
		}
		log.Printf("[%s] %s", step.Name, step.Run)
		if err := runShell(step.Run, dir); err != nil {
			return fmt.Errorf("step %q failed: %w", step.Name, err)
		}
	}

	for i := range cfg.Images {
		img := &cfg.Images[i]
		if err := buildImage(projectDir, img); err != nil {
			return fmt.Errorf("docker build (%s) failed: %w", img.Label(), err)
		}
		if opts.Push {
			if err := pushImage(img); err != nil {
				return fmt.Errorf("docker push (%s) failed: %w", img.Label(), err)
			}
		} else {
			log.Printf("skipping push for %s (--no-push)", img.Label())
		}
	}

	log.Printf("pipeline for %q completed successfully", cfg.Name)
	return nil
}

func runShell(command, dir string) error {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
