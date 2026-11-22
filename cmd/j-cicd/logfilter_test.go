package main

import (
	"regexp"
	"strings"
	"testing"
)

const sampleLog = `2026/01/01 00:00:00 run x of "app"
== stage step build ==
[build] make build
compiling
== stage step test ==
[test] make test
ok pkg/a
FAIL pkg/b
exit status 1
== stage building image app ==
`

func TestStageFilter(t *testing.T) {
	for _, stage := range []string{"test", "step test", "STEP TEST"} {
		out, err := logFilter{stage: stage}.apply([]byte(sampleLog))
		if err != nil {
			t.Fatalf("%q: %v", stage, err)
		}
		want := "== stage step test ==\n[test] make test\nok pkg/a\nFAIL pkg/b\nexit status 1\n"
		if string(out) != want {
			t.Errorf("--stage %q =\n%s\nwant\n%s", stage, out, want)
		}
	}
}

func TestStageFilterUnknownListsStages(t *testing.T) {
	_, err := logFilter{stage: "lint"}.apply([]byte(sampleLog))
	if err == nil || !strings.Contains(err.Error(), "step build, step test, building image app") {
		t.Errorf("err = %v, want it to list the stages the run had", err)
	}
	_, err = logFilter{stage: "x"}.apply([]byte("no markers here\n"))
	if err == nil || !strings.Contains(err.Error(), "no stage markers") {
		t.Errorf("err = %v, want a note that the log has no markers", err)
	}
}

func TestGrepWithContext(t *testing.T) {
	out, _ := logFilter{grep: regexp.MustCompile("FAIL|compiling"), context: 1}.apply([]byte(sampleLog))
	want := "[build] make build\ncompiling\n== stage step test ==\n--\nok pkg/a\nFAIL pkg/b\nexit status 1\n"
	if string(out) != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
	out, _ = logFilter{grep: regexp.MustCompile("FAIL")}.apply([]byte(sampleLog))
	if string(out) != "FAIL pkg/b\n" {
		t.Errorf("no context: %q", out)
	}
}

func TestStageThenTailThenPlain(t *testing.T) {
	log := "== stage step t ==\n\x1b[31mred\x1b[0m\nbar 1%\rbar 100%\nlast\n"
	out, _ := logFilter{plain: true, stage: "t", tail: 2}.apply([]byte(log))
	if string(out) != "bar 100%\nlast\n" {
		t.Errorf("got %q", out)
	}
}

func TestZeroFilterKeepsLogAndEmptyIsEmpty(t *testing.T) {
	if out, _ := (logFilter{}).apply([]byte(sampleLog)); string(out) != sampleLog {
		t.Errorf("zero filter changed the log:\n%s", out)
	}
	if out, err := (logFilter{plain: true}).apply(nil); err != nil || len(out) != 0 {
		t.Errorf("empty log: %q, %v", out, err)
	}
}
