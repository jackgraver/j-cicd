package main

import (
	"os"
	"path/filepath"
	"testing"

	"j-cicd/internal/store"
)

// TestMain points every test at a throwaway state dir, so running the
// suite never touches the real project registry or run history.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "j-cicd-test-*")
	if err != nil {
		panic(err)
	}
	os.Setenv(store.HomeEnv, dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestNameFromDir(t *testing.T) {
	cases := map[string]string{
		"/home/me/simple-track": "simple-track",
		"/x/My App":             "My-App",
		"/x/.dotted":            "dotted",
		"beta app":              "beta-app",
		"/":                     "project",
	}
	for in, want := range cases {
		got := nameFromDir(in)
		if got != want {
			t.Errorf("nameFromDir(%q) = %q, want %q", in, got, want)
		}
		if err := store.ValidName(got); err != nil {
			t.Errorf("nameFromDir(%q) produced an invalid name: %v", in, err)
		}
	}
}

func writeConfig(t *testing.T, dir string) {
	t.Helper()
	cfg := "name: demo\nsteps:\n  - run: \"true\"\n"
	if err := os.WriteFile(filepath.Join(dir, ".cicd.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRunTarget(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registered := t.TempDir()
	writeConfig(t, registered)
	if _, err := st.AddProject(store.Project{Name: "demo", Dir: registered}); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(t.TempDir(), "loose proj")
	os.Mkdir(loose, 0o755)
	writeConfig(t, loose)

	run := func(args ...string) (string, string, error) {
		t.Helper()
		parsed, err := parseRunArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		target, err := resolveRunTarget(st, parsed)
		return target.Name, target.Dir, err
	}

	if name, dir, err := run("demo"); err != nil || name != "demo" || dir != registered {
		t.Errorf("by name: %q %q %v", name, dir, err)
	}
	// A registered dir records under its registered name.
	if name, _, err := run(registered); err != nil || name != "demo" {
		t.Errorf("by registered dir: %q %v", name, err)
	}
	if name, _, err := run(loose); err != nil || name != "loose-proj" {
		t.Errorf("by unregistered dir: %q %v", name, err)
	}
	if _, _, err := run("nosuchproject"); err == nil {
		t.Error("unknown name resolved")
	}
	if _, _, err := run(t.TempDir()); err == nil {
		t.Error("dir without a config resolved")
	}
	if _, _, err := run("demo", "--config", "/elsewhere.yaml"); err == nil {
		t.Error("--config with a registered project accepted")
	}
}

func TestAddListRemove(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir)
	if code := appStart([]string{"add", dir}); code != exitOK {
		t.Fatalf("add exit = %d", code)
	}
	if code := appStart([]string{"add", dir, "--name", "again"}); code == exitOK {
		t.Error("same dir registered twice")
	}
	if code := appStart([]string{"list"}); code != exitOK {
		t.Errorf("list exit = %d", code)
	}
	if code := appStart([]string{"run", "demo", "--quiet", "--no-push"}); code != exitOK {
		t.Errorf("run by name exit = %d", code)
	}
	if code := appStart([]string{"history", "demo"}); code != exitOK {
		t.Errorf("history exit = %d", code)
	}
	if code := appStart([]string{"remove", "demo"}); code != exitOK {
		t.Errorf("remove exit = %d", code)
	}
	if code := appStart([]string{"add", t.TempDir()}); code != exitConfig {
		t.Errorf("add without a config: exit = %d, want %d", code, exitConfig)
	}
}
