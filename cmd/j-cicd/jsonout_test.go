package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"j-cicd/internal/store"
)

// captureStdout runs fn and returns what it wrote to stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	return <-done
}

// runJSON runs j-cicd with args and decodes the single JSON object it
// prints on stdout.
func runJSON(t *testing.T, args ...string) (map[string]any, int) {
	t.Helper()
	var code int
	out := captureStdout(t, func() { code = appStart(args) })
	if strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("want exactly one line of JSON on stdout, got:\n%s", out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	return obj, code
}

func project(t *testing.T, config string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".cicd.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestJSONErrorForUnknownProject(t *testing.T) {
	obj, code := runJSON(t, "run", "no-such-project-xyz", "--json")
	if code != exitConfig || obj["status"] != "error" || obj["error_kind"] != "not_found" || obj["exit_code"] != float64(exitConfig) {
		t.Errorf("code %d, obj %v", code, obj)
	}
	if next, _ := obj["next"].([]any); len(next) != 1 || next[0] != "j-cicd list" {
		t.Errorf("next = %v", obj["next"])
	}
}

func TestJSONErrorForUsageMistake(t *testing.T) {
	obj, code := runJSON(t, "run", "--json", "--nope")
	if code != exitUsage || obj["error_kind"] != "usage" || obj["exit_code"] != float64(exitUsage) {
		t.Errorf("code %d, obj %v", code, obj)
	}
}

func TestJSONFailedRunNamesStageExitCodeAndNextSteps(t *testing.T) {
	dir := project(t, "name: jsonfail\nsteps:\n  - name: test\n    run: echo boom; exit 4\n")
	obj, code := runJSON(t, "run", dir, "--json", "--quiet")
	if code != exitFailure || obj["status"] != "failed" || obj["exit_code"] != float64(exitFailure) {
		t.Fatalf("code %d, obj %v", code, obj)
	}
	f, _ := obj["failure"].(map[string]any)
	if f["kind"] != "step" || f["stage"] != "test" || f["exit_code"] != float64(4) {
		t.Errorf("failure = %v", f)
	}
	if tail, _ := f["tail"].([]any); len(tail) != 1 || tail[0] != "boom" {
		t.Errorf("tail = %v", f["tail"])
	}
	id, _ := obj["id"].(string)
	var next []string
	for _, n := range obj["next"].([]any) {
		next = append(next, n.(string))
	}
	if want := "j-cicd logs " + id + " --stage 'step test'"; !slices.Contains(next, want) {
		t.Errorf("next = %v, want it to include %q", next, want)
	}
}

func TestJSONSuccessHasNoFailureOrNext(t *testing.T) {
	dir := project(t, "name: jsonok\nsteps:\n  - name: hi\n    run: echo hi\n")
	obj, code := runJSON(t, "run", dir, "--json", "--quiet")
	if code != exitOK || obj["status"] != "success" || obj["exit_code"] != float64(0) {
		t.Fatalf("code %d, obj %v", code, obj)
	}
	for _, k := range []string{"failure", "next"} {
		if _, ok := obj[k]; ok {
			t.Errorf("a successful run has %q: %v", k, obj[k])
		}
	}
	if stages, _ := obj["stages"].([]any); len(stages) != 1 {
		t.Errorf("stages = %v", obj["stages"])
	}
}

func TestNextStepsAfterVerifyFailureOffersRollback(t *testing.T) {
	r := store.Run{ID: "r1", Project: "app", Status: store.StatusFailed, Deployed: true,
		Failure: &store.RunFailure{Kind: "verify", Stage: "deploy@vps"}}
	if got := nextSteps(r); !slices.Contains(got, "j-cicd rollback app") {
		t.Errorf("next = %v, want a rollback hint for a deployed run", got)
	}
	if got := nextSteps(store.Run{ID: "r2", Status: store.StatusSuccess}); got != nil {
		t.Errorf("next for success = %v", got)
	}
}

func TestShellQuoteAndHasJSONFlag(t *testing.T) {
	for in, want := range map[string]string{"test": "test", "step test": "'step test'", "it's": `'it'\''s'`} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
	for args, want := range map[string]bool{
		"run --json": true, "run -json": true, "run --json=true": true, "run --json=1": true,
		"run --json=TRUE": true, "run --json=t": true, "run --json=false": false,
		"run --json=0": false, "run x": false,
		// The last one wins, as with the flag package.
		"run --json=false --json --nope": true, "run --json --json=false": false, "run --jsonx": false, "run json": false,
	} {
		if got := hasJSONFlag(strings.Fields(args)); got != want {
			t.Errorf("hasJSONFlag(%q) = %v, want %v", args, got, want)
		}
	}
}

// Every command in `next` has to work: a hint that points at a stage that
// isn't in the log is worse than none. Covers stages that print no progress
// line of their own (artifacts, deploy-check) as well as an ordinary step.
func TestNextStageHintsWork(t *testing.T) {
	cases := map[string]string{
		"artifacts": "name: art\nsteps:\n  - name: hi\n    run: echo hi\nartifacts:\n  - name: bin\n    path: dist/bin\n",
		// Not a git repo, so the deploy is refused before anything runs.
		"preflight": "name: dep\nsteps:\n  - name: hi\n    run: echo hi\nimages:\n  - repository: example.com/x\ndeploy:\n  host: nowhere\n",
		"test":      "name: stp\nsteps:\n  - name: hi\n    run: echo hi\n  - name: test\n    run: exit 1\n",
	}
	for stage, config := range cases {
		t.Run(stage, func(t *testing.T) {
			obj, code := runJSON(t, "run", project(t, config), "--json", "--quiet")
			if code != exitFailure {
				t.Fatalf("code %d, obj %v", code, obj)
			}
			f, _ := obj["failure"].(map[string]any)
			if f["stage"] != stage {
				t.Fatalf("failure = %v, want stage %s", f, stage)
			}
			// The tail must not hold a passed stage's output ("hi").
			tail, _ := f["tail"].([]any)
			for _, l := range tail {
				if l == "hi" {
					t.Errorf("tail has an earlier stage's output: %v", f["tail"])
				}
			}
			id := obj["id"].(string)
			// Run the hint itself: its --stage argument, quotes removed.
			m := regexp.MustCompile(`--stage (.*)$`).FindStringSubmatch(obj["next"].([]any)[0].(string))
			if m == nil {
				t.Fatalf("next[0] = %v, want a logs --stage hint", obj["next"])
			}
			arg := strings.Trim(m[1], "'")
			var logsCode int
			out := captureStdout(t, func() { logsCode = appStart([]string{"logs", id, "--stage", arg}) })
			if logsCode != exitOK || !strings.Contains(out, "== stage "+arg+" ==") {
				t.Errorf("logs --stage %q: exit %d, output:\n%s", arg, logsCode, out)
			}
		})
	}
}

func TestNextStepUsesTheUnambiguousStageForm(t *testing.T) {
	// A cancelled run's failure has no stage kind; the stage record has it.
	r := store.Run{ID: "r", Project: "app", Status: store.StatusCancelled,
		Failure: &store.RunFailure{Kind: store.FailureCancelled, Stage: "test"},
		Stages:  []store.RunStage{{Name: "setup", Kind: "step", Status: "ok"}, {Name: "test", Kind: "step", Status: "failed"}}}
	if got := nextSteps(r); len(got) == 0 || got[0] != "j-cicd logs r --stage 'step test'" {
		t.Errorf("next = %v", got)
	}
	// A stage named for its kind is just the kind.
	r.Failure.Stage, r.Stages = "artifacts", []store.RunStage{{Name: "artifacts", Kind: "artifacts", Status: "failed"}}
	if got := nextSteps(r); got[0] != "j-cicd logs r --stage artifacts" {
		t.Errorf("next = %v", got)
	}
}
