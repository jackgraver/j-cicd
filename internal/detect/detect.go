// Package detect inspects a project directory so `j-cicd init` can
// scaffold a config with real values instead of placeholders, and so a
// run can label its images with the commit it built.
package detect

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// skipDirs are never descended into when scanning a project for
// Dockerfiles.
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
//
// Discovery is best-effort: an unreadable subdirectory (a root-owned
// cache, a broken mount) is skipped rather than failing the whole scan.
func findDockerfiles(projectDir string) ([]string, error) {
	var found []string

	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if depth == 0 {
				// projectDir itself being unreadable is a real error.
				return err
			}
			return nil
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
			if !isDockerfile(e.Name()) {
				continue
			}
			rel, err := filepath.Rel(projectDir, filepath.Join(dir, e.Name()))
			if err != nil {
				return err
			}
			found = append(found, rel)
		}
		return nil
	}

	if err := walk(projectDir, 0); err != nil {
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}

// nonDockerfileExts are final extensions that mean a "Dockerfile.*" file
// is documentation, a template, or editor debris rather than something
// `docker build -f` can use. Generating an images: entry for one of these
// turns a working init into a pipeline that fails on the next run.
var nonDockerfileExts = map[string]bool{
	"bak":      true,
	"diff":     true,
	"example":  true,
	"lock":     true,
	"log":      true,
	"markdown": true,
	"md":       true,
	"old":      true,
	"orig":     true,
	"patch":    true,
	"rej":      true,
	"sample":   true,
	"save":     true,
	"swo":      true,
	"swp":      true,
	"template": true,
	"tmp":      true,
	"txt":      true,
}

// isDockerfile reports whether name looks like a Dockerfile that docker
// could actually build: "Dockerfile" itself, or "Dockerfile.<variant>"
// where the variant isn't a known non-Dockerfile extension.
func isDockerfile(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") {
		return false
	}
	if name == "Dockerfile" {
		return true
	}
	variant, ok := strings.CutPrefix(name, "Dockerfile.")
	if !ok || variant == "" {
		return false
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	return !nonDockerfileExts[ext]
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

// GitHubRepo is githubRepo for other packages: the owner and repository
// (lowercased) of projectDir's GitHub "origin" remote.
func GitHubRepo(projectDir string) (owner, repo string, ok bool) {
	return githubRepo(projectDir)
}

// GitRevision returns a short identifier for what's checked out in
// projectDir, suitable for use as an image tag, and whether it could be
// determined at all (false when projectDir isn't a git repository or has
// no commits).
//
// A tree with uncommitted changes gets a "-dirty" suffix: j-cicd builds
// whatever is on disk, so an image tagged with a bare commit SHA that
// didn't come from that commit would be actively misleading.
func GitRevision(projectDir string) (string, bool) {
	out, err := exec.Command("git", "-C", projectDir, "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		return "", false
	}
	rev := strings.TrimSpace(string(out))
	if rev == "" {
		return "", false
	}
	if status, err := exec.Command("git", "-C", projectDir, "status", "--porcelain").Output(); err == nil {
		if len(strings.TrimSpace(string(status))) > 0 {
			rev += "-dirty"
		}
	}
	return rev, true
}

var nonLabelRe = regexp.MustCompile(`[^a-z0-9]+`)

// serviceName derives a label from a Dockerfile's path relative to the
// project root, including any Dockerfile.<variant> suffix:
//
//	Dockerfile               -> ""              (the project's only service)
//	Dockerfile.dev           -> "dev"
//	backend/Dockerfile       -> "backend"
//	backend/Dockerfile.test  -> "backend-test"
//	services/api/Dockerfile  -> "services-api"
//
// The whole path has to go in, because this label is what distinguishes
// the generated repository: values from each other. Deriving it from the
// parent directory alone made services/api and other/api — and Dockerfile
// and Dockerfile.dev — collapse onto a single registry ref, so several
// images would push to the same tag and the last one would win.
func serviceName(dockerfilePath string) string {
	var parts []string
	if dir := filepath.Dir(dockerfilePath); dir != "." {
		parts = append(parts, strings.Split(filepath.ToSlash(dir), "/")...)
	}
	if variant := strings.TrimPrefix(filepath.Base(dockerfilePath), "Dockerfile"); variant != "" {
		parts = append(parts, variant)
	}
	return sanitizeLabel(strings.Join(parts, "-"))
}

// sanitizeLabel reduces s to what's legal in a docker repository path
// component: lowercase alphanumerics separated by single dashes.
func sanitizeLabel(s string) string {
	return strings.Trim(nonLabelRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}
