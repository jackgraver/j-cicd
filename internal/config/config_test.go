package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write puts body in a temp file and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), DefaultFile)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func loadOK(t *testing.T, body, revision string) *Config {
	t.Helper()
	cfg, err := Load(write(t, body), revision)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func loadErr(t *testing.T, body string) string {
	t.Helper()
	_, err := Load(write(t, body), "")
	if err == nil {
		t.Fatalf("Load succeeded, want an error:\n%s", body)
	}
	return err.Error()
}

func TestLoadAppliesImageDefaults(t *testing.T) {
	cfg := loadOK(t, `
name: demo
images:
  - repository: ghcr.io/o/demo
`, "abc123456789")

	img := cfg.Images[0]
	if img.Dockerfile != "Dockerfile" || img.Context != "." {
		t.Errorf("dockerfile/context = %q/%q, want Dockerfile/.", img.Dockerfile, img.Context)
	}
	// Every build must leave behind a tag that still points at this image
	// after the next push moves "latest".
	want := []string{"latest", "abc123456789"}
	if strings.Join(img.Tags, ",") != strings.Join(want, ",") {
		t.Errorf("tags = %v, want %v", img.Tags, want)
	}
}

func TestLoadWithoutRevisionTagsLatestOnly(t *testing.T) {
	cfg := loadOK(t, "name: demo\nimages:\n  - repository: ghcr.io/o/demo\n", "")
	if len(cfg.Images[0].Tags) != 1 || cfg.Images[0].Tags[0] != "latest" {
		t.Errorf("tags = %v, want [latest]", cfg.Images[0].Tags)
	}
}

func TestLoadKeepsExplicitTags(t *testing.T) {
	cfg := loadOK(t, "name: demo\nimages:\n  - repository: ghcr.io/o/demo\n    tags: [v1]\n", "abc123")
	if len(cfg.Images[0].Tags) != 1 || cfg.Images[0].Tags[0] != "v1" {
		t.Errorf("tags = %v, want [v1]", cfg.Images[0].Tags)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	// A typo like "dockerFile" would otherwise be dropped silently and
	// the build would use the default Dockerfile instead.
	got := loadErr(t, `
name: demo
images:
  - repository: ghcr.io/o/demo
    dockerFile: backend/Dockerfile
`)
	if !strings.Contains(got, "dockerFile") {
		t.Errorf("error = %q, want it to name the unknown field", got)
	}
}

func TestLoadRejectsStepWithoutRun(t *testing.T) {
	// `sh -c ""` exits 0: "no test ran" must never look like "tests passed".
	got := loadErr(t, "name: demo\nsteps:\n  - name: test\n")
	if !strings.Contains(got, "steps[0].run is required") {
		t.Errorf("error = %q, want it to name the step with no run", got)
	}
}

func TestLoadRejectsDuplicateImageRefs(t *testing.T) {
	// Two images pushing one ref means the last build silently wins and
	// a deploy pulling that tag gets whichever it happened to be.
	got := loadErr(t, `
name: demo
images:
  - repository: ghcr.io/o/demo
    dockerfile: Dockerfile
    tags: [latest]
  - repository: ghcr.io/o/demo
    dockerfile: Dockerfile.dev
    tags: [latest, dev]
`)
	if !strings.Contains(got, "both push ghcr.io/o/demo:latest") {
		t.Errorf("error = %q, want it to name the colliding ref", got)
	}
}

func TestLoadAllowsSameRepositoryWithDistinctTags(t *testing.T) {
	loadOK(t, `
name: demo
images:
  - repository: ghcr.io/o/demo
    tags: [backend]
  - repository: ghcr.io/o/demo
    dockerfile: Dockerfile.dev
    tags: [frontend]
`, "")
}

func TestLoadValidationErrors(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"empty file", "", "config is empty"},
		{"no name", "steps:\n  - run: make\n", "name is required"},
		{"nothing to do", "name: demo\n", "at least one step or image"},
		{"image without repository", "name: demo\nimages:\n  - name: backend\n", "images[0].repository is required"},
		{"empty tag", "name: demo\nimages:\n  - repository: r\n    tags: [\"\"]\n", "empty tag"},
		{"bad duration", "name: demo\ntimeout: soon\nsteps:\n  - run: make\n", "invalid duration"},
		{"negative duration", "name: demo\ntimeout: -5m\nsteps:\n  - run: make\n", "must not be negative"},
		{"malformed yaml", "name: [demo\n", "parsing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := loadErr(t, tc.body); !strings.Contains(got, tc.want) {
				t.Errorf("error = %q, want it to contain %q", got, tc.want)
			}
		})
	}
}

func TestLoadDefaultsStepNames(t *testing.T) {
	cfg := loadOK(t, "name: demo\nsteps:\n  - run: make build\n  - name: test\n    run: make test\n", "")
	if cfg.Steps[0].Name != "step 1" {
		t.Errorf("steps[0].name = %q, want \"step 1\"", cfg.Steps[0].Name)
	}
	if cfg.Steps[1].Name != "test" {
		t.Errorf("steps[1].name = %q, want \"test\"", cfg.Steps[1].Name)
	}
}

func TestStepTimeoutPrecedence(t *testing.T) {
	cfg := loadOK(t, `
name: demo
timeout: 10m
steps:
  - run: quick
  - run: slow
    timeout: 1h
  - run: unbounded
    timeout: 0s
`, "")

	const fallback = 42 * time.Minute
	cases := []struct {
		step int
		want time.Duration
	}{
		{0, 10 * time.Minute}, // inherits the config default
		{1, time.Hour},        // its own timeout wins
		{2, 0},                // explicit 0 means no limit
	}
	for _, tc := range cases {
		if got := cfg.StepTimeout(&cfg.Steps[tc.step], fallback); got != tc.want {
			t.Errorf("steps[%d] timeout = %v, want %v", tc.step, got, tc.want)
		}
	}
	if got := cfg.ImageTimeout(fallback); got != 10*time.Minute {
		t.Errorf("image timeout = %v, want 10m", got)
	}
}

func TestTimeoutFallsBackWhenUnset(t *testing.T) {
	cfg := loadOK(t, "name: demo\nsteps:\n  - run: make\n", "")
	const fallback = 7 * time.Minute
	if got := cfg.StepTimeout(&cfg.Steps[0], fallback); got != fallback {
		t.Errorf("step timeout = %v, want the fallback %v", got, fallback)
	}
	if got := cfg.ImageTimeout(fallback); got != fallback {
		t.Errorf("image timeout = %v, want the fallback %v", got, fallback)
	}
}

func TestImageLabel(t *testing.T) {
	named := Image{Name: "backend", Repository: "ghcr.io/o/demo"}
	if got := named.Label(); got != "backend" {
		t.Errorf("Label() = %q, want backend", got)
	}
	unnamed := Image{Repository: "ghcr.io/o/demo"}
	if got := unnamed.Label(); got != "ghcr.io/o/demo" {
		t.Errorf("Label() = %q, want the repository", got)
	}
}

func TestLoadDeployDefaults(t *testing.T) {
	cfg := loadOK(t, `
name: simple-track
images:
  - name: backend
    repository: ghcr.io/o/be
deploy:
  host: deploy@vps
`, "")
	d := cfg.Deploy
	if d.Command != DefaultAgentCommand || d.Project != "simple-track" {
		t.Errorf("command/project = %q/%q, want %q/simple-track", d.Command, d.Project, DefaultAgentCommand)
	}
	if got := cfg.DeployImages(); len(got) != 1 || got[0].Name != "backend" {
		t.Errorf("DeployImages = %v, want [backend]", got)
	}
}

func TestLoadDeployTimeoutFallsBack(t *testing.T) {
	cfg := loadOK(t, "name: d\ntimeout: 5m\nimages: [{repository: r}]\ndeploy: {host: h}\n", "")
	if got := cfg.DeployTimeout(time.Hour); got != 5*time.Minute {
		t.Errorf("DeployTimeout = %v, want the top-level 5m", got)
	}
	cfg = loadOK(t, "name: d\ntimeout: 5m\nimages: [{repository: r}]\ndeploy: {host: h, timeout: 2m}\n", "")
	if got := cfg.DeployTimeout(time.Hour); got != 2*time.Minute {
		t.Errorf("DeployTimeout = %v, want deploy's own 2m", got)
	}
}

func TestLoadRejectsBadDeploy(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"missing host": {
			"name: d\nimages: [{repository: r}]\ndeploy: {command: x}\n", "host is required",
		},
		"host looks like an ssh option": {
			"name: d\nimages: [{repository: r}]\ndeploy: {host: -oProxyCommand=x}\n", "not a valid ssh destination",
		},
		"nothing to deploy": {
			"name: d\nsteps: [{run: make}]\ndeploy: {host: h}\n", "no images",
		},
		// The name doubles as the agent's lock/state file name.
		"unsafe project name": {
			"name: my project\nimages: [{repository: r}]\ndeploy: {host: h}\n", "project \"my project\"",
		},
		"unknown image": {
			"name: d\nimages: [{name: be, repository: r}]\ndeploy: {host: h, images: [fe]}\n", "no images: entry has that name",
		},
		"duplicate image": {
			"name: d\nimages: [{name: be, repository: r}]\ndeploy: {host: h, images: [be, be]}\n", "twice",
		},
		"typo": {
			"name: d\nimages: [{repository: r}]\ndeploy: {hostname: h}\n", "hostname",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := loadErr(t, tc.body); !strings.Contains(got, tc.want) {
				t.Errorf("error = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}
