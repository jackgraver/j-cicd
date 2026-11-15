package detect

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"j-cicd/internal/config"
)

// StarterResult reports what WriteStarterConfig found and wrote, so the
// command layer can decide what to tell the user instead of this layer
// printing on its own.
type StarterResult struct {
	// Path is the config file that was written.
	Path string
	// Dockerfiles are the Dockerfiles found, relative to the project dir.
	Dockerfiles []string
	// GitHubRemote reports whether a GitHub origin was detected; without
	// one the generated repository: values are placeholders.
	GitHubRemote bool
}

// WriteStarterConfig scaffolds a starter .cicd.yaml in projectDir. It
// inspects the project (git remote, Dockerfiles, go.mod/package.json)
// so it can fill in real values instead of generic placeholders wherever
// possible — a project with Dockerfiles for several services gets one
// images entry per service, not just the first one it finds.
func WriteStarterConfig(projectDir, name string, force bool) (*StarterResult, error) {
	owner, repo, hasGitHub := githubRepo(projectDir)

	if name == "" {
		if hasGitHub {
			name = repo
		} else {
			abs, err := filepath.Abs(projectDir)
			if err != nil {
				return nil, err
			}
			name = filepath.Base(abs)
		}
	}

	path := filepath.Join(projectDir, config.DefaultFile)
	if _, err := os.Stat(path); err == nil && !force {
		return nil, fmt.Errorf("%s already exists (use --force to overwrite)", path)
	}

	cfg := config.Config{Name: name}

	switch {
	case hasFile(projectDir, "go.mod"):
		cfg.Steps = []config.Step{
			{Name: "build", Run: "go build ./..."},
			{Name: "test", Run: "go test ./..."},
		}
	case hasFile(projectDir, "package.json"):
		cfg.Steps = []config.Step{
			{Name: "install", Run: "npm ci"},
			{Name: "test", Run: "npm test"},
		}
	default:
		cfg.Steps = []config.Step{
			{Name: "build", Run: `echo "replace with your build command"`},
			{Name: "test", Run: `echo "replace with your test command"`},
		}
	}

	dockerfiles, err := findDockerfiles(projectDir)
	if err != nil {
		return nil, fmt.Errorf("scanning for Dockerfiles: %w", err)
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

		buildContext := "."
		if dir := filepath.Dir(df); dir != "." {
			buildContext = dir
		}

		// Tags are deliberately left out: omitting them lets each run tag
		// the image "latest" plus the commit it was built from, which is
		// what gives a deploy something immutable to pin and roll back to.
		cfg.Images = append(cfg.Images, config.Image{
			Name:       svc,
			Repository: repository,
			Dockerfile: df,
			Context:    buildContext,
		})
	}

	out, err := yaml.Marshal(&cfg)
	if err != nil {
		return nil, fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(path, out, 0644); err != nil {
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}

	return &StarterResult{
		Path:         path,
		Dockerfiles:  dockerfiles,
		GitHubRemote: hasGitHub,
	}, nil
}
