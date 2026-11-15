package detect

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tree creates files (and their parent directories) under a new temp dir.
func tree(t *testing.T, paths ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range paths {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return root
}

func TestFindDockerfiles(t *testing.T) {
	root := tree(t,
		"Dockerfile",
		"Dockerfile.dev",
		"backend/Dockerfile",
		"frontend/Dockerfile",
		"services/api/Dockerfile",
		"a/b/c/d/e/Dockerfile",        // deeper than the scan depth limit
		"node_modules/pkg/Dockerfile", // skipped directory
		"vendor/x/Dockerfile",         // skipped directory
		".git/Dockerfile",             // hidden directory
		"Dockerfile.md",               // documentation, not buildable
		"Dockerfile.orig",             // merge debris
		"backend/Dockerfile.swp",      // editor debris
		"frontend/Dockerfile~",        // editor backup
		"notes/Dockerfiles.txt",       // not a Dockerfile at all
	)

	got, err := findDockerfiles(root)
	if err != nil {
		t.Fatalf("findDockerfiles: %v", err)
	}
	want := []string{
		"Dockerfile",
		"Dockerfile.dev",
		"backend/Dockerfile",
		"frontend/Dockerfile",
		"services/api/Dockerfile",
	}
	assertPaths(t, got, want)
}

func TestFindDockerfilesSkipsUnreadableSubdir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any directory")
	}
	root := tree(t, "svc/Dockerfile", "secret/Dockerfile")
	secret := filepath.Join(root, "secret")
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o755) })

	// Discovery is best-effort: a root-owned cache or broken mount must
	// not make init write no config at all.
	got, err := findDockerfiles(root)
	if err != nil {
		t.Fatalf("findDockerfiles: %v", err)
	}
	assertPaths(t, got, []string{"svc/Dockerfile"})
}

func TestFindDockerfilesUnreadableRootIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any directory")
	}
	root := tree(t, "svc/Dockerfile")
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	if _, err := findDockerfiles(root); err == nil {
		t.Error("findDockerfiles succeeded on an unreadable project dir, want an error")
	}
}

func TestIsDockerfile(t *testing.T) {
	cases := map[string]bool{
		"Dockerfile":          true,
		"Dockerfile.dev":      true,
		"Dockerfile.prod":     true,
		"Dockerfile.alpine":   true,
		"Dockerfile.md":       false,
		"Dockerfile.markdown": false,
		"Dockerfile.txt":      false,
		"Dockerfile.orig":     false,
		"Dockerfile.bak":      false,
		"Dockerfile.swp":      false,
		"Dockerfile.dev.swp":  false,
		"Dockerfile.example":  false,
		"Dockerfile.template": false,
		"Dockerfile~":         false,
		".Dockerfile":         false,
		"Dockerfile.":         false,
		"Dockerfiles":         false,
		"MyDockerfile":        false,
		"dockerfile":          false,
	}
	for name, want := range cases {
		if got := isDockerfile(name); got != want {
			t.Errorf("isDockerfile(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestServiceNameIsUniquePerDockerfile(t *testing.T) {
	cases := map[string]string{
		"Dockerfile":              "",
		"Dockerfile.dev":          "dev",
		"backend/Dockerfile":      "backend",
		"backend/Dockerfile.test": "backend-test",
		"services/api/Dockerfile": "services-api",
		"other/api/Dockerfile":    "other-api",
		"My Service/Dockerfile":   "my-service",
		"apps/web_ui/Dockerfile":  "apps-web-ui",
	}
	for path, want := range cases {
		if got := serviceName(filepath.FromSlash(path)); got != want {
			t.Errorf("serviceName(%q) = %q, want %q", path, got, want)
		}
	}

	// The names are what distinguish the generated repository: values, so
	// paths that used to collapse onto one name must stay distinct.
	seen := map[string]string{}
	for _, path := range []string{
		"Dockerfile", "Dockerfile.dev",
		"backend/Dockerfile", "backend/Dockerfile.test",
		"services/api/Dockerfile", "other/api/Dockerfile",
	} {
		name := serviceName(filepath.FromSlash(path))
		if prev, dup := seen[name]; dup {
			t.Errorf("%q and %q both map to service name %q", prev, path, name)
		}
		seen[name] = path
	}
}

func TestGithubRemoteRe(t *testing.T) {
	cases := map[string][2]string{
		"git@github.com:Owner/Repo.git":       {"owner", "repo"},
		"https://github.com/owner/repo.git":   {"owner", "repo"},
		"https://github.com/owner/repo":       {"owner", "repo"},
		"https://github.com/owner/repo/":      {"owner", "repo"},
		"ssh://git@github.com/owner/repo.git": {"owner", "repo"},
	}
	for url, want := range cases {
		m := githubRemoteRe.FindStringSubmatch(url)
		if m == nil {
			t.Errorf("%q did not match", url)
			continue
		}
		owner, repo := strings.ToLower(m[1]), strings.ToLower(m[2])
		if owner != want[0] || repo != want[1] {
			t.Errorf("%q -> %s/%s, want %s/%s", url, owner, repo, want[0], want[1])
		}
	}
	for _, url := range []string{"git@gitlab.com:owner/repo.git", "https://example.com/owner/repo.git"} {
		if githubRemoteRe.MatchString(url) {
			t.Errorf("%q matched, want no match", url)
		}
	}
}

func TestGitRevision(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	root := tree(t, "file.txt")
	if _, ok := GitRevision(root); ok {
		t.Error("GitRevision succeeded outside a git repo, want ok=false")
	}

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-qm", "init")

	rev, ok := GitRevision(root)
	if !ok {
		t.Fatal("GitRevision failed in a git repo")
	}
	if strings.HasSuffix(rev, "-dirty") {
		t.Errorf("revision = %q, want no -dirty suffix on a clean tree", rev)
	}

	// j-cicd builds what's on disk, so an image built from a dirty tree
	// must not be tagged as if it came from the commit.
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	dirty, ok := GitRevision(root)
	if !ok || !strings.HasSuffix(dirty, "-dirty") {
		t.Errorf("revision = %q (ok=%v), want a -dirty suffix", dirty, ok)
	}
}

func assertPaths(t *testing.T, got, want []string) {
	t.Helper()
	gotSlash := make([]string, len(got))
	for i, p := range got {
		gotSlash[i] = filepath.ToSlash(p)
	}
	if strings.Join(gotSlash, "\n") != strings.Join(want, "\n") {
		t.Errorf("paths:\n got %v\nwant %v", gotSlash, want)
	}
}
