package main

import (
	"errors"
	"flag"
	"path/filepath"
	"testing"
)

func TestParseRunArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want runArgs
	}{
		{
			name: "no arguments",
			args: nil,
			want: runArgs{projectDir: ".", configPath: filepath.Join(".", ".cicd.yaml"), push: true, deploy: true},
		},
		{
			name: "project dir only",
			args: []string{"/proj"},
			want: runArgs{projectDir: "/proj", configPath: filepath.Join("/proj", ".cicd.yaml"), push: true, deploy: true},
		},
		{
			// The documented order, and the one that used to push anyway.
			name: "flag after project dir",
			args: []string{"/proj", "--no-push"},
			want: runArgs{projectDir: "/proj", configPath: filepath.Join("/proj", ".cicd.yaml"), push: false, deploy: true},
		},
		{
			name: "flag before project dir",
			args: []string{"--no-push", "/proj"},
			want: runArgs{projectDir: "/proj", configPath: filepath.Join("/proj", ".cicd.yaml"), push: false, deploy: true},
		},
		{
			name: "single dash flag after project dir",
			args: []string{"/proj", "-no-push"},
			want: runArgs{projectDir: "/proj", configPath: filepath.Join("/proj", ".cicd.yaml"), push: false, deploy: true},
		},
		{
			name: "config after project dir",
			args: []string{"/proj", "--config", "/elsewhere/pipeline.yaml"},
			want: runArgs{projectDir: "/proj", configPath: "/elsewhere/pipeline.yaml", push: true, deploy: true},
		},
		{
			// --config alone means "run that project", not "run the cwd
			// with another project's config".
			name: "config without project dir",
			args: []string{"--config", "/other/.cicd.yaml"},
			want: runArgs{projectDir: "/other", configPath: "/other/.cicd.yaml", push: true, deploy: true},
		},
		{
			name: "both flags after project dir",
			args: []string{"/proj", "--config", "/proj/ci.yaml", "--no-push"},
			want: runArgs{projectDir: "/proj", configPath: "/proj/ci.yaml", push: false, deploy: true},
		},
		{
			name: "flags on both sides",
			args: []string{"--no-push", "/proj", "--config=/proj/ci.yaml"},
			want: runArgs{projectDir: "/proj", configPath: "/proj/ci.yaml", push: false, deploy: true},
		},
		{
			name: "deploy-dirty",
			args: []string{"--deploy-dirty", "/proj"},
			want: runArgs{projectDir: "/proj", configPath: filepath.Join("/proj", ".cicd.yaml"), push: true, deploy: true, deployDirty: true},
		},
		{
			name: "no-deploy after project dir",
			args: []string{"/proj", "--no-deploy"},
			want: runArgs{projectDir: "/proj", configPath: filepath.Join("/proj", ".cicd.yaml"), push: true, deploy: false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRunArgs(tc.args)
			if err != nil {
				t.Fatalf("parseRunArgs(%q): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseRunArgs(%q) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseRunArgsErrors(t *testing.T) {
	cases := map[string][]string{
		"unknown flag":       {"/proj", "--nopush"},
		"extra positional":   {"/proj", "/other"},
		"missing flag value": {"/proj", "--config"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			// Silently ignoring these is exactly how --no-push used to
			// fail open, so they have to be errors.
			if _, err := parseRunArgs(args); err == nil {
				t.Errorf("parseRunArgs(%q) succeeded, want an error", args)
			}
		})
	}
}

func TestParseRunArgsHelp(t *testing.T) {
	if _, err := parseRunArgs([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("err = %v, want flag.ErrHelp", err)
	}
}

func TestParseInitArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want initArgs
	}{
		{"no arguments", nil, initArgs{projectDir: "."}},
		{"project dir only", []string{"/proj"}, initArgs{projectDir: "/proj"}},
		{"force after project dir", []string{"/proj", "--force"}, initArgs{projectDir: "/proj", force: true}},
		{"force before project dir", []string{"--force", "/proj"}, initArgs{projectDir: "/proj", force: true}},
		{"name after project dir", []string{"/proj", "--name", "demo"}, initArgs{projectDir: "/proj", name: "demo"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseInitArgs(tc.args)
			if err != nil {
				t.Fatalf("parseInitArgs(%q): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseInitArgs(%q) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestAppStartExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, exitUsage},
		{"unknown command", []string{"frobnicate"}, exitUsage},
		{"version", []string{"version"}, exitOK},
		{"help", []string{"help"}, exitOK},
		{"bad flag", []string{"run", "--nope"}, exitUsage},
		{"subcommand help", []string{"run", "-h"}, exitOK},
		// A config j-cicd can't even read is distinct from a pipeline
		// that ran and failed, so CI callers can tell them apart.
		{"missing config", []string{"run", filepath.Join(t.TempDir(), "nope")}, exitConfig},
		{"rollback bad flag", []string{"rollback", "--nope"}, exitUsage},
		{"rollback two targets", []string{"rollback", "a", "b"}, exitUsage},
		{"rollback unknown project", []string{"rollback", "no-such-project"}, exitConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appStart(tc.args); got != tc.want {
				t.Errorf("appStart(%q) = %d, want %d", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseRunArgsOutputFlags(t *testing.T) {
	got, err := parseRunArgs([]string{"app", "--json", "-v", "--plan"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.json || !got.verbose || !got.plan || got.quiet {
		t.Errorf("parsed = %+v", got)
	}
}
