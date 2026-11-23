package main

import (
	"errors"
	"fmt"
	"testing"

	"j-cicd/internal/store"
)

func TestRefusedDeployIsNotInternal(t *testing.T) {
	e := toJSONError(fmt.Errorf("%w: this deploy would fail: dirty", errRefused), "app")
	if e.ErrorKind != errKindRefused || len(e.Next) == 0 {
		t.Errorf("error = %+v", e)
	}
}

func TestHistoryLastConflictsWithLimit(t *testing.T) {
	err := cmdHistory([]string{"--last", "-n", "5"})
	if err == nil || exitCodeOf(err) != exitUsage {
		t.Errorf("err = %v, want usage error", err)
	}
}

func TestHistoryAndFindRunSkipChecks(t *testing.T) {
	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	start := func(kind string) store.Run {
		h, err := st.StartRun(store.Run{Project: "skipchecks", Kind: kind})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Finish(store.StatusSuccess, ""); err != nil {
			t.Fatal(err)
		}
		return h.Run
	}
	deploy := start("")
	check := start(store.KindCheck)

	got, err := findRun(st, "skipchecks")
	if err != nil || got.ID != deploy.ID {
		t.Errorf("findRun = %v, %v; want the deploy %s", got.ID, err, deploy.ID)
	}
	if got, err := findRun(st, check.ID); err != nil || got.ID != check.ID {
		t.Errorf("a check is reachable by id: %v, %v", got.ID, err)
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
}
