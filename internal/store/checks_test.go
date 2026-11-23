package store

import "testing"

func finishRun(t *testing.T, s *Store, project, kind string, status Status) Run {
	t.Helper()
	h, err := s.StartRun(Run{Project: project, Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Finish(status, ""); err != nil {
		t.Fatal(err)
	}
	return h.Run
}

// A passing check after a failed deploy must not become the project's
// latest run.
func TestLastRunsSkipsChecks(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deploy := finishRun(t, s, "a", "", StatusFailed)
	finishRun(t, s, "a", KindCheck, StatusSuccess)
	last, err := s.LastRuns()
	if err != nil {
		t.Fatal(err)
	}
	if last["a"].ID != deploy.ID {
		t.Errorf("last run of a = %+v, want the deploy %s", last["a"], deploy.ID)
	}
}

// Checks are pruned apart from the other runs, so many checks can't
// push deploys out of history.
func TestPruneKeepsChecksApart(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deploy := finishRun(t, s, "a", "", StatusSuccess)
	for i := 0; i < DefaultKeepChecks+5; i++ {
		finishRun(t, s, "a", KindCheck, StatusSuccess)
	}
	if err := s.prune("a", 2); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs("a", 0)
	if err != nil {
		t.Fatal(err)
	}
	checks, others := 0, 0
	for _, r := range runs {
		if r.Kind == KindCheck {
			checks++
		} else {
			others++
		}
	}
	if checks != DefaultKeepChecks || others != 1 {
		t.Errorf("kept %d checks and %d others, want %d and 1 (deploy %s)", checks, others, DefaultKeepChecks, deploy.ID)
	}
}
