package manager

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"j-cicd/internal/store"
)

// recordingRunner records each command with the directory it ran in and
// what that directory held at the time (the snapshot is gone by the time
// Run returns).
type recordingRunner struct {
	calls []recordedCall
}

type recordedCall struct {
	dir, cmd string
	files    []string
}

func (r *recordingRunner) Run(ctx context.Context, dir, name string, args ...string) error {
	c := recordedCall{dir: dir, cmd: strings.Join(append([]string{name}, args...), " ")}
	if dir != "" {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			c.files = append(c.files, e.Name())
		}
	}
	r.calls = append(r.calls, c)
	return nil
}

func (r *recordingRunner) Output(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	return nil, r.Run(ctx, dir, name, args...)
}

func (r *recordingRunner) find(t *testing.T, prefix string) recordedCall {
	t.Helper()
	for _, c := range r.calls {
		if strings.HasPrefix(c.cmd, prefix) {
			return c
		}
	}
	t.Fatalf("no command starting %q in %+v", prefix, r.calls)
	return recordedCall{}
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const releaseConfig = `name: app
branch: main
steps:
  - name: test
    run: make release-test
images:
  - name: web
    repository: registry.example.com/app
deploy:
  host: vps
`

// releaseRepo returns a repo whose main branch holds releaseConfig and
// released.txt, while the working tree is on a feature branch with an
// extra commit, an edited config and uncommitted files — none of which a
// release may pick up.
func releaseRepo(t *testing.T) (st *store.Store, target Target, mainRev string) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	write(t, filepath.Join(dir, ".cicd.yaml"), releaseConfig)
	write(t, filepath.Join(dir, "released.txt"), "on main\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "release")
	mainRev = gitT(t, dir, "rev-parse", "--short=12", "main")

	gitT(t, dir, "checkout", "-q", "-b", "feature")
	write(t, filepath.Join(dir, "feature.txt"), "committed on feature\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "feature work")
	write(t, filepath.Join(dir, "wip.txt"), "uncommitted\n")
	write(t, filepath.Join(dir, ".cicd.yaml"), strings.Replace(releaseConfig, "make release-test", "make wip-test", 1))
	return st, Target{Name: "app", Dir: dir, ConfigPath: filepath.Join(dir, ".cicd.yaml")}, mainRev
}

func TestReleaseBranchBuildsCommittedBranchNotWorkingTree(t *testing.T) {
	st, target, mainRev := releaseRepo(t)
	r := &recordingRunner{}
	run, err := Run(context.Background(), st, target, Options{Push: true, Deploy: true, Runner: r})
	if err != nil {
		t.Fatal(err)
	}

	// Main's committed config ran, not the edited working copy.
	step := r.find(t, "sh -c make release-test")
	snap := snapshotDir(st.Root(), "app")
	if step.dir != snap {
		t.Errorf("step ran in %s, want the snapshot %s", step.dir, snap)
	}
	got := strings.Join(step.files, " ")
	if !strings.Contains(got, "released.txt") || strings.Contains(got, "feature.txt") || strings.Contains(got, "wip.txt") {
		t.Errorf("snapshot held %v, want main's files only", step.files)
	}

	// Tagged and deployed as main's commit, even though the working
	// tree is dirty, and with no --deploy-dirty.
	if run.Revision != mainRev {
		t.Errorf("revision = %q, want %q", run.Revision, mainRev)
	}
	build := r.find(t, "docker build")
	if !strings.Contains(build.cmd, "registry.example.com/app:"+mainRev) || !strings.Contains(build.cmd, snap) {
		t.Errorf("build = %q", build.cmd)
	}
	if deploy := r.find(t, "ssh"); !strings.Contains(deploy.cmd, "'registry.example.com/app:"+mainRev+"'") {
		t.Errorf("deploy = %q", deploy.cmd)
	}

	// The snapshot is cleaned up, and the user's repo is left as it was.
	if _, err := os.Stat(snap); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("snapshot still exists: %v", err)
	}
	if wts := gitT(t, target.Dir, "worktree", "list"); strings.Count(wts, "\n") != 0 {
		t.Errorf("worktree left registered:\n%s", wts)
	}
	if branch := gitT(t, target.Dir, "branch", "--show-current"); branch != "feature" {
		t.Errorf("checked-out branch = %q, want feature", branch)
	}
	if _, err := os.Stat(filepath.Join(target.Dir, "wip.txt")); err != nil {
		t.Errorf("uncommitted file was disturbed: %v", err)
	}
	log, _ := os.ReadFile(st.LogPath(run.ID))
	if !strings.Contains(string(log), "building branch main at "+mainRev) {
		t.Errorf("log doesn't say what it built:\n%s", log)
	}
}

func TestReleaseBranchMissing(t *testing.T) {
	st, target, _ := releaseRepo(t)
	write(t, target.ConfigPath, strings.Replace(releaseConfig, "branch: main", "branch: prod", 1))
	run, err := Run(context.Background(), st, target, Options{Runner: &recordingRunner{}})
	if err == nil || !strings.Contains(err.Error(), `release branch "prod" doesn't exist`) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := st.GetRun(run.ID); got.Status != store.StatusFailed || !strings.Contains(got.Error, "prod") {
		t.Errorf("recorded run = %+v", got)
	}
}

func TestReleaseBranchWithoutCommittedConfig(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	write(t, filepath.Join(dir, "README"), "x\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "init")
	write(t, filepath.Join(dir, ".cicd.yaml"), releaseConfig) // never committed

	target := Target{Name: "app", Dir: dir, ConfigPath: filepath.Join(dir, ".cicd.yaml")}
	_, err = Run(context.Background(), st, target, Options{Runner: &recordingRunner{}})
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) || !strings.Contains(err.Error(), "commit it to main") {
		t.Fatalf("err = %v, want a ConfigError saying to commit the config", err)
	}
}

func TestReleaseBranchReplacesStaleSnapshot(t *testing.T) {
	// A run killed mid-build leaves its snapshot behind; the next run
	// must not trip over it.
	st, target, _ := releaseRepo(t)
	write(t, filepath.Join(snapshotDir(st.Root(), "app"), "leftover"), "x")
	r := &recordingRunner{}
	if _, err := Run(context.Background(), st, target, Options{Runner: r}); err != nil {
		t.Fatal(err)
	}
	if files := strings.Join(r.find(t, "sh -c").files, " "); strings.Contains(files, "leftover") {
		t.Errorf("stale snapshot content leaked into the build: %s", files)
	}
}

func TestReleaseBranchInSubdirectory(t *testing.T) {
	// A project in a monorepo subdirectory builds that subdirectory of
	// the snapshot.
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	dir := filepath.Join(repo, "services", "app")
	gitT(t, repo, "init", "-q", "-b", "main")
	write(t, filepath.Join(dir, ".cicd.yaml"), "name: app\nbranch: main\nsteps:\n  - run: make\n")
	write(t, filepath.Join(dir, "main.go"), "package main\n")
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-q", "-m", "init")

	r := &recordingRunner{}
	target := Target{Name: "app", Dir: dir, ConfigPath: filepath.Join(dir, ".cicd.yaml")}
	if _, err := Run(context.Background(), st, target, Options{Runner: r}); err != nil {
		t.Fatal(err)
	}
	if step := r.find(t, "sh -c make"); step.dir != filepath.Join(snapshotDir(st.Root(), "app"), "services", "app") {
		t.Errorf("step ran in %s", step.dir)
	}
}

func TestNoReleaseBranchBuildsWorkingTree(t *testing.T) {
	// Without branch:, nothing changes: the directory is built as it is.
	st, target, _ := releaseRepo(t)
	write(t, target.ConfigPath, strings.Replace(releaseConfig, "branch: main\n", "", 1))
	r := &recordingRunner{}
	if _, err := Run(context.Background(), st, target, Options{Runner: r}); err != nil {
		t.Fatal(err)
	}
	if step := r.find(t, "sh -c"); step.dir != target.Dir {
		t.Errorf("step ran in %s, want %s", step.dir, target.Dir)
	}
}
