package detect

import (
	"os"
	"strings"
	"testing"

	"j-cicd/internal/config"
)

func TestWriteStarterConfigGivesEveryServiceItsOwnRef(t *testing.T) {
	root := tree(t,
		"go.mod",
		"Dockerfile",
		"Dockerfile.dev",
		"backend/Dockerfile",
		"backend/Dockerfile.test",
		"services/api/Dockerfile",
		"other/api/Dockerfile",
	)

	res, err := WriteStarterConfig(root, "sample", false)
	if err != nil {
		t.Fatalf("WriteStarterConfig: %v", err)
	}
	if len(res.Dockerfiles) != 6 {
		t.Fatalf("found %v, want 6 Dockerfiles", res.Dockerfiles)
	}

	// The generated config has to survive the loader, which rejects two
	// images that would push the same repository:tag.
	cfg, err := config.Load(res.Path, "abc123")
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if len(cfg.Images) != 6 {
		t.Fatalf("got %d images, want 6", len(cfg.Images))
	}

	seen := map[string]string{}
	for _, img := range cfg.Images {
		if prev, dup := seen[img.Repository]; dup {
			t.Errorf("%s and %s share repository %s", prev, img.Dockerfile, img.Repository)
		}
		seen[img.Repository] = img.Dockerfile
	}

	// Steps come from the detected project type (go.mod here).
	if len(cfg.Steps) == 0 || !strings.Contains(cfg.Steps[0].Run, "go build") {
		t.Errorf("steps = %+v, want go build/test steps", cfg.Steps)
	}
}

func TestWriteStarterConfigLeavesTagsToTheRun(t *testing.T) {
	root := tree(t, "Dockerfile")

	res, err := WriteStarterConfig(root, "sample", false)
	if err != nil {
		t.Fatalf("WriteStarterConfig: %v", err)
	}
	raw, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	if strings.Contains(string(raw), "tags:") {
		t.Errorf("generated config pins tags, want them left to the run:\n%s", raw)
	}

	// Omitting them is what lets a run add the commit alongside "latest".
	cfg, err := config.Load(res.Path, "abc123")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := strings.Join(cfg.Images[0].Tags, ","); got != "latest,abc123" {
		t.Errorf("tags = %q, want latest,abc123", got)
	}
}

func TestWriteStarterConfigPlaceholderWithoutGitHubRemote(t *testing.T) {
	root := tree(t, "Dockerfile")

	res, err := WriteStarterConfig(root, "sample", false)
	if err != nil {
		t.Fatalf("WriteStarterConfig: %v", err)
	}
	if res.GitHubRemote {
		t.Error("GitHubRemote = true in a non-repo temp dir")
	}
	cfg, err := config.Load(res.Path, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.HasPrefix(cfg.Images[0].Repository, "registry.example.com/") {
		t.Errorf("repository = %q, want the editable placeholder", cfg.Images[0].Repository)
	}
}

func TestWriteStarterConfigRefusesToClobberWithoutForce(t *testing.T) {
	root := tree(t, "Dockerfile")

	if _, err := WriteStarterConfig(root, "sample", false); err != nil {
		t.Fatalf("first write: %v", err)
	}
	_, err := WriteStarterConfig(root, "sample", false)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want an already-exists error", err)
	}
	if _, err := WriteStarterConfig(root, "renamed", true); err != nil {
		t.Fatalf("--force write: %v", err)
	}
	cfg, err := config.Load(root+"/"+config.DefaultFile, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Name != "renamed" {
		t.Errorf("name = %q, want renamed", cfg.Name)
	}
}
