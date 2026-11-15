package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"j-cicd/internal/store"
)

// fakeRunner stands in for real processes: it succeeds, fails on a
// matching command, or blocks until cancelled.
type fakeRunner struct {
	failOn string
	block  bool
}

func (f *fakeRunner) Run(ctx context.Context, dir, name string, args ...string) error {
	cmd := strings.Join(append([]string{name}, args...), " ")
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.failOn != "" && strings.Contains(cmd, f.failOn) {
		return errors.New("exit status 1")
	}
	return nil
}

// Output records the call like Run; these tests never need command output.
func (f *fakeRunner) Output(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	return nil, f.Run(ctx, dir, name, args...)
}

func setup(t *testing.T, config string) (*store.Store, Target) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".cicd.yaml")
	if config != "" {
		if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return st, Target{Name: "app", Dir: dir, ConfigPath: path}
}

const twoSteps = `name: app
steps:
  - name: build
    run: make build
  - name: test
    run: make test
`

func TestRunRecordsSuccess(t *testing.T) {
	st, target := setup(t, twoSteps)
	run, err := Run(context.Background(), st, target, Options{Push: true, Runner: &fakeRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusSuccess || got.Project != "app" || !got.Push || got.FinishedAt.IsZero() {
		t.Errorf("recorded run = %+v", got)
	}
	log, _ := os.ReadFile(st.LogPath(run.ID))
	for _, want := range []string{"[build] make build", "[test] make test", "run succeeded"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
}

func TestRunRecordsFailure(t *testing.T) {
	st, target := setup(t, twoSteps)
	run, err := Run(context.Background(), st, target, Options{Runner: &fakeRunner{failOn: "make test"}})
	if err == nil {
		t.Fatal("want an error")
	}
	got, _ := st.GetRun(run.ID)
	if got.Status != store.StatusFailed || !strings.Contains(got.Error, `step "test"`) {
		t.Errorf("recorded run = %+v", got)
	}
}

func TestRunRecordsBadConfig(t *testing.T) {
	// A broken config is still a run worth seeing in history.
	st, target := setup(t, "name: app\nstepz: []\n")
	run, err := Run(context.Background(), st, target, Options{Runner: &fakeRunner{}})
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("err = %v, want a ConfigError", err)
	}
	if got, _ := st.GetRun(run.ID); got.Status != store.StatusFailed {
		t.Errorf("recorded run = %+v", got)
	}
}

func TestRunRecordsCancellation(t *testing.T) {
	st, target := setup(t, twoSteps)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan store.Run)
	go func() {
		run, _ := Run(ctx, st, target, Options{Runner: &fakeRunner{block: true}})
		done <- run
	}()
	// Wait for the run to be recorded as in progress before cancelling.
	for {
		if _, busy, _ := Busy(st, "app"); busy {
			break
		}
	}
	cancel()
	run := <-done
	if got, _ := st.GetRun(run.ID); got.Status != store.StatusCancelled {
		t.Errorf("recorded run = %+v, want cancelled", got)
	}
}

func TestRunRefusesConcurrentRunOfSameProject(t *testing.T) {
	st, target := setup(t, twoSteps)
	unlock, err := st.LockProject("app")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := Run(context.Background(), st, target, Options{Runner: &fakeRunner{}}); !errors.Is(err, ErrBusy) {
		t.Errorf("err = %v, want ErrBusy", err)
	}
	if runs, _ := st.Runs("app", 0); len(runs) != 0 {
		t.Errorf("a refused run was recorded: %+v", runs)
	}
}

func TestRunEchoesOutput(t *testing.T) {
	st, target := setup(t, twoSteps)
	var echo strings.Builder
	if _, err := Run(context.Background(), st, target, Options{Runner: &fakeRunner{}, Echo: &echo}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(echo.String(), "[build] make build") {
		t.Errorf("echo = %q", echo.String())
	}
}
