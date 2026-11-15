package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const starterConfigTemplate = `name: %s
steps:
  - name: build
    run: echo "replace with your build command"
  - name: test
    run: echo "replace with your test command"
image:
  repository: registry.example.com/%s
  dockerfile: Dockerfile
  context: .
  tags:
    - latest
`

// WriteStarterConfig scaffolds a starter .cicd.yaml in projectDir so a new
// project can be onboarded without hand-writing the format from scratch.
func WriteStarterConfig(projectDir, name string, force bool) error {
	if name == "" {
		abs, err := filepath.Abs(projectDir)
		if err != nil {
			return err
		}
		name = filepath.Base(abs)
	}

	path := filepath.Join(projectDir, defaultConfigFile)
	if _, err := os.Stat(path); err == nil && !force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", path)
	}

	content := fmt.Sprintf(starterConfigTemplate, name, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	fmt.Printf("wrote %s\n", path)
	return nil
}
