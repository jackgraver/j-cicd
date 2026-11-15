package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// skipDirs are never descended into when scanning a project for Dockerfiles.
var skipDirs = map[string]bool{
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
}

const dockerfileScanMaxDepth = 4

// findDockerfiles walks projectDir (bounded depth, skipping common
// vendor/build/hidden directories) and returns every Dockerfile it finds,
// relative to projectDir and sorted for deterministic output. This is how
// init discovers a project's services without being told about them.
func findDockerfiles(projectDir string) ([]string, error) {
	var found []string

	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				if depth >= dockerfileScanMaxDepth || skipDirs[e.Name()] || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				if err := walk(filepath.Join(dir, e.Name()), depth+1); err != nil {
					return err
				}
				continue
			}
			if e.Name() == "Dockerfile" || strings.HasPrefix(e.Name(), "Dockerfile.") {
				rel, err := filepath.Rel(projectDir, filepath.Join(dir, e.Name()))
				if err != nil {
					return err
				}
				found = append(found, rel)
			}
		}
		return nil
	}

	if err := walk(projectDir, 0); err != nil {
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}

func hasFile(projectDir, name string) bool {
	_, err := os.Stat(filepath.Join(projectDir, name))
	return err == nil
}

var githubRemoteRe = regexp.MustCompile(`github\.com[:/]([^/]+)/([^/]+?)(\.git)?/?$`)

// githubRepo returns (owner, repo, true) if projectDir's git "origin"
// remote points at GitHub, so init can default to a real ghcr.io path
// instead of a placeholder registry.
func githubRepo(projectDir string) (owner, repo string, ok bool) {
	out, err := exec.Command("git", "-C", projectDir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", "", false
	}
	m := githubRemoteRe.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return "", "", false
	}
	return strings.ToLower(m[1]), strings.ToLower(m[2]), true
}

// serviceName derives a short label from a Dockerfile's relative path:
// "Dockerfile" -> "" (the project's primary/only service),
// "backend/Dockerfile" -> "backend", "services/api/Dockerfile" -> "api".
func serviceName(dockerfilePath string) string {
	dir := filepath.Dir(dockerfilePath)
	if dir == "." {
		return ""
	}
	return filepath.Base(dir)
}
