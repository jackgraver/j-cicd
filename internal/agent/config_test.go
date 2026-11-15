package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const validConfig = `
projects:
  - name: simple-track
    compose_file: /opt/st/docker-compose.vps.yml
    services:
      - name: backend
        image_var: BACKEND_IMAGE
        repository: ghcr.io/o/st-backend
`

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	p, err := cfg.Project("simple-track")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateDir != DefaultStateDir || p.healthTimeout() != DefaultHealthTimeout || p.timeout() != DefaultTimeout {
		t.Errorf("defaults not applied: state_dir=%q health=%v timeout=%v", cfg.StateDir, p.healthTimeout(), p.timeout())
	}
}

func TestLoadConfigRejects(t *testing.T) {
	svc := func(name, v, repo string) string {
		return "      - {name: " + name + ", image_var: " + v + ", repository: " + repo + "}\n"
	}
	head := "projects:\n  - name: st\n    compose_file: /c.yml\n    services:\n"
	cases := map[string]struct{ body, want string }{
		"empty":           {"", "empty"},
		"no projects":     {"state_dir: /x\n", "at least one project"},
		"typo":            {strings.Replace(validConfig, "compose_file", "compose", 1), "compose"},
		"bad name":        {strings.Replace(validConfig, "simple-track", "../etc", 1), "must contain only"},
		"no compose file": {"projects:\n  - name: st\n    services:\n" + svc("be", "BE", "r"), "compose_file is required"},
		"no services":     {"projects:\n  - name: st\n    compose_file: /c.yml\n", "at least one service"},
		"bad var":         {head + svc("be", "BE-IMAGE", "r"), "not a valid variable name"},
		"dup repository":  {head + svc("be", "A", "r") + svc("fe", "B", "r"), "mapped to two services"},
		"dup var":         {head + svc("be", "A", "r1") + svc("fe", "A", "r2"), "used by two services"},
		"dup service":     {head + svc("be", "A", "r1") + svc("be", "B", "r2"), "listed twice"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
