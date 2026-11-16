package manager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"j-cicd/internal/store"
)

const deployConfig = `name: app
images:
  - name: web
    repository: registry.example.com/app
deploy:
  host: deploy@vps
  command: /opt/bin/j-cicd-agent
  project: app-prod
`

func TestRollbackAsksTheAgentAndIsRecorded(t *testing.T) {
	st, target := setup(t, deployConfig)
	r := &recordingRunner{}
	run, err := Rollback(context.Background(), st, target, Options{Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || r.calls[0].cmd != "ssh -o BatchMode=yes -- deploy@vps /opt/bin/j-cicd-agent rollback 'app-prod'" {
		t.Errorf("commands = %+v", r.calls)
	}
	got, _ := st.GetRun(run.ID)
	if got.Kind != store.KindRollback || got.Status != store.StatusSuccess || got.Push {
		t.Errorf("recorded run = %+v", got)
	}
}

func TestRollbackFailureIsRecorded(t *testing.T) {
	st, target := setup(t, deployConfig)
	run, err := Rollback(context.Background(), st, target, Options{Runner: &fakeRunner{failOn: "rollback"}})
	if err == nil || !strings.Contains(err.Error(), "rollback on deploy@vps failed") {
		t.Fatalf("err = %v", err)
	}
	if got, _ := st.GetRun(run.ID); got.Status != store.StatusFailed || got.Kind != store.KindRollback {
		t.Errorf("recorded run = %+v", got)
	}
}

func TestRollbackWithoutDeploySection(t *testing.T) {
	st, target := setup(t, twoSteps)
	r := &recordingRunner{}
	_, err := Rollback(context.Background(), st, target, Options{Runner: r})
	if err == nil || !strings.Contains(err.Error(), "no deploy: section") {
		t.Fatalf("err = %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("ran commands anyway: %+v", r.calls)
	}
}

func TestRollbackWaitsForNoRun(t *testing.T) {
	// A rollback mid-build would race the build's own deploy.
	st, target := setup(t, deployConfig)
	unlock, err := st.LockProject("app")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := Rollback(context.Background(), st, target, Options{Runner: &recordingRunner{}}); !errors.Is(err, ErrBusy) {
		t.Errorf("err = %v, want ErrBusy", err)
	}
}
