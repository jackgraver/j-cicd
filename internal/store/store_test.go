package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestProjectsRoundTrip(t *testing.T) {
	st := openTemp(t)
	dir := t.TempDir()

	if ps, err := st.Projects(); err != nil || len(ps) != 0 {
		t.Fatalf("fresh store: projects = %v, %v; want none", ps, err)
	}
	if _, err := st.AddProject(Project{Name: "zeta", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddProject(Project{Name: "alpha", Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}

	ps, err := st.Projects()
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].Name != "alpha" || ps[1].Name != "zeta" {
		t.Fatalf("projects = %+v, want alpha then zeta", ps)
	}
	if p, ok, err := st.ProjectByDir(dir); err != nil || !ok || p.Name != "zeta" {
		t.Errorf("ProjectByDir = %+v, %v, %v; want zeta", p, ok, err)
	}

	if err := st.RemoveProject("zeta"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Project("zeta"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removed project still found: %v", err)
	}
	if err := st.RemoveProject("zeta"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing twice: err = %v, want ErrNotFound", err)
	}
}

func TestAddProjectRejectsDuplicates(t *testing.T) {
	st := openTemp(t)
	dir := t.TempDir()
	if _, err := st.AddProject(Project{Name: "app", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddProject(Project{Name: "app", Dir: t.TempDir()}); err == nil {
		t.Error("same name registered twice")
	}
	// Spelling out the default config is the same registration.
	if _, err := st.AddProject(Project{Name: "app2", Dir: dir, Config: filepath.Join(dir, ".cicd.yaml")}); err == nil {
		t.Error("same dir+config registered twice under different names")
	}
	// A different config in the same dir is a different pipeline.
	if _, err := st.AddProject(Project{Name: "app-staging", Dir: dir, Config: filepath.Join(dir, "staging.yaml")}); err != nil {
		t.Errorf("second config in the same dir rejected: %v", err)
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"app", "my-app", "app_2", "a.b", "9lives"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) = %v, want ok", ok, err)
		}
	}
	// Names have to be distinguishable from paths on the command line.
	for _, bad := range []string{"", ".", "..", ".hidden", "-flag", "a/b", "has space", `a\b`} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) succeeded, want an error", bad)
		}
	}
}

func TestRunLifecycle(t *testing.T) {
	st := openTemp(t)
	h, err := st.StartRun(Run{Project: "app", Dir: "/x", Push: true})
	if err != nil {
		t.Fatal(err)
	}
	if h.Run.ID == "" || h.Run.Status != StatusRunning || h.Run.PID != os.Getpid() {
		t.Fatalf("started run = %+v", h.Run)
	}
	if _, err := h.Log.WriteString("hello\n"); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetRun(h.Run.ID)
	if err != nil || got.Status != StatusRunning {
		t.Fatalf("in-progress run read back as %+v, %v", got, err)
	}

	if err := h.Finish(StatusFailed, "step \"test\" failed"); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetRun(h.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || got.Error == "" || got.FinishedAt.IsZero() || !got.Push {
		t.Errorf("finished run = %+v", got)
	}
	if data, _ := os.ReadFile(st.LogPath(h.Run.ID)); string(data) != "hello\n" {
		t.Errorf("log = %q", data)
	}
}

func TestGetRunRejectsPaths(t *testing.T) {
	st := openTemp(t)
	for _, id := range []string{"", ".", "..", "../projects.json", "a/b"} {
		if _, err := st.GetRun(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetRun(%q) err = %v, want ErrNotFound", id, err)
		}
	}
}

// writeRun records a finished run directly, with a chosen start time, so
// ordering tests don't depend on the clock.
func writeRun(t *testing.T, st *Store, r Run) {
	t.Helper()
	if err := os.MkdirAll(st.runDir(r.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(st.runPath(r.ID), r); err != nil {
		t.Fatal(err)
	}
}

func TestRunsNewestFirstAndFiltered(t *testing.T) {
	st := openTemp(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, project := range []string{"a", "b", "a", "b", "a"} {
		start := base.Add(time.Duration(i) * time.Minute)
		id, _ := newRunID(start)
		writeRun(t, st, Run{ID: id, Project: project, Status: StatusSuccess, StartedAt: start})
	}

	all, err := st.Runs("", 0)
	if err != nil || len(all) != 5 {
		t.Fatalf("Runs = %d runs, %v", len(all), err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].StartedAt.After(all[i-1].StartedAt) {
			t.Fatalf("runs not newest-first: %v before %v", all[i-1].StartedAt, all[i].StartedAt)
		}
	}

	as, _ := st.Runs("a", 2)
	if len(as) != 2 || as[0].Project != "a" || !as[0].StartedAt.Equal(base.Add(4*time.Minute)) {
		t.Errorf("Runs(a, 2) = %+v", as)
	}

	last, _ := st.LastRuns()
	if !last["b"].StartedAt.Equal(base.Add(3 * time.Minute)) {
		t.Errorf("LastRuns[b] = %+v", last["b"])
	}
}

func TestDeadRunIsInterrupted(t *testing.T) {
	st := openTemp(t)
	// A PID far beyond any real pid_max: no such process.
	writeRun(t, st, Run{ID: "20260101-000000-aaaaaa", Project: "a", Status: StatusRunning, PID: 1 << 30, StartedAt: time.Now()})

	r, err := st.GetRun("20260101-000000-aaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusInterrupted {
		t.Fatalf("status = %s, want interrupted", r.Status)
	}
	// And it's saved, not just reported.
	var onDisk Run
	if err := readJSON(st.runPath(r.ID), &onDisk); err != nil || onDisk.Status != StatusInterrupted {
		t.Errorf("on disk: %+v, %v", onDisk, err)
	}
}

func TestPruneKeepsNewestFinishedRuns(t *testing.T) {
	st := openTemp(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		start := base.Add(time.Duration(i) * time.Minute)
		id, _ := newRunID(start)
		writeRun(t, st, Run{ID: id, Project: "a", Status: StatusSuccess, StartedAt: start})
	}
	other, _ := newRunID(base)
	writeRun(t, st, Run{ID: other, Project: "b", Status: StatusSuccess, StartedAt: base})

	if err := st.prune("a", 2); err != nil {
		t.Fatal(err)
	}
	as, _ := st.Runs("a", 0)
	if len(as) != 2 || !as[1].StartedAt.Equal(base.Add(3*time.Minute)) {
		t.Errorf("after prune: %+v", as)
	}
	if bs, _ := st.Runs("b", 0); len(bs) != 1 {
		t.Errorf("prune of a touched b: %+v", bs)
	}
}

func TestLockProjectIsExclusive(t *testing.T) {
	st := openTemp(t)
	unlock, err := st.LockProject("app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.LockProject("app"); !errors.Is(err, ErrLocked) {
		t.Errorf("second lock: err = %v, want ErrLocked", err)
	}
	other, err := st.LockProject("other")
	if err != nil {
		t.Errorf("unrelated project blocked: %v", err)
	} else {
		other()
	}
	unlock()
	again, err := st.LockProject("app")
	if err != nil {
		t.Errorf("lock not released: %v", err)
	} else {
		again()
	}
}
