package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// WriteStarterConfig scaffolds a starter .cicd.yaml in projectDir. It
// inspects the project (git remote, Dockerfiles, go.mod/package.json)
// so it can fill in real values instead of generic placeholders wherever
// possible — a project with Dockerfiles for several services gets one
// images entry per service, not just the first one it finds.
func WriteStarterConfig(projectDir, name string, force bool) error {
	owner, repo, hasGitHub := githubRepo(projectDir)

	if name == "" {
		if hasGitHub {
			name = repo
		} else {
			abs, err := filepath.Abs(projectDir)
			if err != nil {
				return err
			}
			name = filepath.Base(abs)
		}
	}

	path := filepath.Join(projectDir, defaultConfigFile)
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", path)
	}

	cfg := Config{Name: name}

	switch {
	case hasFile(projectDir, "go.mod"):
		cfg.Steps = []Step{
			{Name: "build", Run: "go build ./..."},
			{Name: "test", Run: "go test ./..."},
		}
	case hasFile(projectDir, "package.json"):
		cfg.Steps = []Step{
			{Name: "install", Run: "npm ci"},
			{Name: "test", Run: "npm test"},
		}
	default:
		cfg.Steps = []Step{
			{Name: "build", Run: `echo "replace with your build command"`},
			{Name: "test", Run: `echo "replace with your test command"`},
		}
	}

	dockerfiles, err := findDockerfiles(projectDir)
	if err != nil {
		return fmt.Errorf("scanning for Dockerfiles: %w", err)
	}

	for _, df := range dockerfiles {
		svc := serviceName(df)
		imgName := name
		if svc != "" {
			imgName = name + "-" + svc
		}

		repository := fmt.Sprintf("registry.example.com/%s", imgName)
		if hasGitHub {
			repository = fmt.Sprintf("ghcr.io/%s/%s", owner, imgName)
		}

		context := "."
		if dir := filepath.Dir(df); dir != "." {
			context = dir
		}

		cfg.Images = append(cfg.Images, ImageConfig{
			Name:       svc,
			Repository: repository,
			Dockerfile: df,
			Context:    context,
			Tags:       []string{"latest"},
		})
	}

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(path, out, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	fmt.Printf("wrote %s\n", path)
	if len(dockerfiles) == 0 {
		fmt.Println("no Dockerfile found — add an images: entry by hand once you have one")
	} else {
		fmt.Printf("found %d Dockerfile(s): %v\n", len(dockerfiles), dockerfiles)
	}
	if !hasGitHub {
		fmt.Println("no GitHub remote detected — repository: in the generated config is a placeholder, edit it before running with push")
	}

	return nil
}
