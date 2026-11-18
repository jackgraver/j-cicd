package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Artifact is a file the steps produce that is the release itself, for
// projects that ship a binary rather than a container image (a CLI, a
// systemd service). After the steps, each one must exist; its sha256 is
// what identifies the release, the way a registry digest does for an
// image. A deploy sends the files to the agent, which installs them
// atomically, restarts what runs them and rolls back on failure.
type Artifact struct {
	// Name is what the agent's config knows the file by (files: name).
	Name string `yaml:"name"`
	// Path is where the steps leave it, relative to the project dir.
	Path string `yaml:"path"`
}

// artifactName is narrow for the same reason as projectName: it names a
// file in the agent's store and travels through a remote shell.
var artifactName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidArtifactName reports whether name can name an artifact, on either
// side of the deploy.
func ValidArtifactName(name string) bool {
	return artifactName.MatchString(name)
}

func (c *Config) validateArtifacts() error {
	seen := map[string]bool{}
	for i := range c.Artifacts {
		a := &c.Artifacts[i]
		if !ValidArtifactName(a.Name) {
			return fmt.Errorf("artifacts[%d].name %q must be letters, digits, '.', '_' and '-'", i, a.Name)
		}
		if seen[a.Name] {
			return fmt.Errorf("artifact %q is listed twice", a.Name)
		}
		seen[a.Name] = true
		if a.Path == "" {
			return fmt.Errorf("artifacts[%d].path is required", i)
		}
		clean, ok := insideProject(a.Path)
		if !ok {
			return fmt.Errorf("artifacts[%d].path %q must be a path inside the project", i, a.Path)
		}
		a.Path = clean
	}
	return nil
}

// insideProject cleans a project-relative path, reporting false for one
// that is absolute or climbs out of the project.
func insideProject(p string) (string, bool) {
	clean := filepath.Clean(p)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	return clean, true
}

func (c *Config) artifactByName(name string) *Artifact {
	for i := range c.Artifacts {
		if c.Artifacts[i].Name == name {
			return &c.Artifacts[i]
		}
	}
	return nil
}

// DeployArtifacts returns the artifacts the deploy stage sends, in
// artifacts: order. It is empty for a deploy of images.
func (c *Config) DeployArtifacts() []*Artifact {
	if c.Deploy == nil || !c.Deploy.sendsArtifacts {
		return nil
	}
	var out []*Artifact
	for i := range c.Artifacts {
		a := &c.Artifacts[i]
		if len(c.Deploy.Artifacts) == 0 || containsString(c.Deploy.Artifacts, a.Name) {
			out = append(out, a)
		}
	}
	return out
}
