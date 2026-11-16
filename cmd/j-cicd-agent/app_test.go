package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppStartExitCodes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent.yaml")
	body := "state_dir: " + filepath.Join(dir, "state") + `
projects:
  - name: st
    compose_file: /opt/st/compose.yml
    services:
      - {name: backend, image_var: BACKEND_IMAGE, repository: ghcr.io/o/be}
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := "ghcr.io/o/be@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, exitUsage},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"version", []string{"version"}, exitOK},
		{"deploy help", []string{"deploy", "-h"}, exitOK},
		{"deploy without refs", []string{"deploy", "st"}, exitUsage},
		{"deploy bad flag", []string{"deploy", "st", ref, "--nope"}, exitUsage},
		{"deploy missing config", []string{"deploy", "--config", filepath.Join(dir, "nope"), "st", ref}, exitConfig},
		// Rejected before docker is ever invoked.
		{"deploy tag", []string{"deploy", "--config", cfgPath, "st", "ghcr.io/o/be:latest"}, exitFailure},
		{"rollback without project", []string{"rollback", "--config", cfgPath}, exitUsage},
		{"rollback two projects", []string{"rollback", "--config", cfgPath, "st", "st"}, exitUsage},
		// Never deployed: refused before docker is ever invoked.
		{"rollback never deployed", []string{"rollback", "--config", cfgPath, "st"}, exitFailure},
		{"status", []string{"status", "--config", cfgPath}, exitOK},
		{"status flag after project", []string{"status", "st", "--config", cfgPath}, exitOK},
		{"status unknown project", []string{"status", "nope", "--config", cfgPath}, exitUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appStart(tc.args); got != tc.want {
				t.Errorf("appStart(%q) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestConfigFromEnvironment(t *testing.T) {
	t.Setenv(configEnv, "/from/env.yaml")
	_, path, err := parseArgs("status", "", nil)
	if err != nil || path != "/from/env.yaml" {
		t.Errorf("path = %q, err = %v, want the env override", path, err)
	}
	_, path, _ = parseArgs("status", "", []string{"--config", "/flag.yaml"})
	if path != "/flag.yaml" {
		t.Errorf("path = %q, want --config to win over the env", path)
	}
}
