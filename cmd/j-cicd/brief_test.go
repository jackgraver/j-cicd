package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"j-cicd/internal/guide"
	"j-cicd/internal/store"
)

func TestBriefLine(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := store.Run{Project: "app", Revision: "abc123", Status: store.StatusSuccess, StartedAt: start, FinishedAt: start.Add(43 * time.Second),
		Deployed: true, Verified: []store.RunVerify{{URL: "u"}}}
	if got, want := briefLine(r), "ok app abc123 deployed verified 43s"; got != want {
		t.Errorf("briefLine = %q, want %q", got, want)
	}
	r = store.Run{Project: "app", Kind: store.KindCheck, StartedAt: start, FinishedAt: start.Add(2 * time.Second)}
	if got, want := briefLine(r), "ok app - checked 2s"; got != want {
		t.Errorf("briefLine = %q, want %q", got, want)
	}
}

func TestBriefFailureOmitsStages(t *testing.T) {
	code := 1
	r := store.Run{ID: "r1", Project: "app", Status: store.StatusFailed,
		Stages:  []store.RunStage{{Name: "build", Kind: "step", Status: "ok"}, {Name: "test", Kind: "step", Status: "failed"}},
		Failure: &store.RunFailure{Kind: "step", Stage: "test", ExitCode: &code, Message: "boom"}}
	b, err := json.Marshal(toJSONBriefFailure(r, 1))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"stages"`) || !strings.Contains(string(b), `"failure"`) || !strings.Contains(string(b), "logs r1 --stage 'step test'") {
		t.Errorf("brief failure = %s", b)
	}
}

func TestDeployRejectsConflictingFlags(t *testing.T) {
	for _, flag := range []string{"--plan", "--commit=abc", "--no-push", "--no-deploy"} {
		if err := cmdDeploy([]string{flag}); err == nil || exitCodeOf(err) != exitUsage {
			t.Errorf("deploy %s: err = %v, want usage error", flag, err)
		}
	}
}

func TestGuideShort(t *testing.T) {
	short := guide.Short()
	if !strings.Contains(short, "## Short version") || strings.Contains(short, "## Rules") {
		t.Errorf("short guide should hold only the summary:\n%s", short)
	}
	if len(short) > len(guide.Text())/3 {
		t.Errorf("short guide is %d bytes of %d: not short", len(short), len(guide.Text()))
	}
	if err := cmdGuide([]string{"--short"}); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryLastAndLimit(t *testing.T) {
	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		h, err := st.StartRun(store.Run{Project: "histproj"})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Finish(store.StatusSuccess, ""); err != nil {
			t.Fatal(err)
		}
	}
	count := func(args ...string) int {
		var runs []map[string]any
		out := captureStdout(t, func() {
			if err := cmdHistory(append([]string{"histproj", "--json"}, args...)); err != nil {
				t.Fatal(err)
			}
		})
		if err := json.Unmarshal([]byte(out), &runs); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return len(runs)
	}
	if n := count("--last"); n != 1 {
		t.Errorf("--last gave %d runs", n)
	}
	if n := count("--limit", "2"); n != 2 {
		t.Errorf("--limit 2 gave %d runs", n)
	}
	if n := count(); n != 3 {
		t.Errorf("default gave %d runs", n)
	}
}
