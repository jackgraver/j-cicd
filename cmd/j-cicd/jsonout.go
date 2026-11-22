package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"j-cicd/internal/manager"
	"j-cicd/internal/store"
)

// jsonRun is a run as --json prints it: the stored record plus its
// duration, which isn't stored.
type jsonRun struct {
	store.Run
	DurationSeconds float64 `json:"duration_seconds"`
}

func toJSONRun(r store.Run) jsonRun {
	return jsonRun{Run: r, DurationSeconds: r.Duration().Round(100 * 1e6).Seconds()}
}

func toJSONRuns(runs []store.Run) []jsonRun {
	out := make([]jsonRun, len(runs))
	for i, r := range runs {
		out[i] = toJSONRun(r)
	}
	return out
}

// printJSON writes v to stdout as one line of JSON. Stdout carries only
// data and stderr only prose, so a script can parse the first without
// filtering the second.
func printJSON(v any) error {
	return json.NewEncoder(os.Stdout).Encode(v)
}

// jsonResult is what `run` and `rollback` print: the run, the exit status
// this invocation is returning, and, for a run that didn't succeed,
// commands that would help (next).
type jsonResult struct {
	jsonRun
	ExitCode int      `json:"exit_code"`
	Next     []string `json:"next,omitempty"`
}

func toJSONResult(r store.Run, exitCode int) jsonResult {
	return jsonResult{jsonRun: toJSONRun(r), ExitCode: exitCode, Next: nextSteps(r)}
}

// Error kinds of jsonError.
const (
	errKindUsage    = "usage"
	errKindConfig   = "config"
	errKindNotFound = "not_found"
	errKindBusy     = "busy"
	errKindInternal = "internal"
)

// jsonError is what a command prints under --json when no run was
// recorded, so there is always exactly one object to parse.
type jsonError struct {
	Status    string   `json:"status"` // always "error"
	ErrorKind string   `json:"error_kind"`
	Error     string   `json:"error"`
	ExitCode  int      `json:"exit_code"`
	Next      []string `json:"next,omitempty"`
}

// errNoProject is a run argument that is neither a registered project nor
// a directory.
var errNoProject = errors.New("no registered project or directory named")

// exitCodeOf is the status appStart returns for err.
func exitCodeOf(err error) int {
	if err == nil {
		return exitOK
	}
	var exit *exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	return exitFailure
}

// toJSONError classifies err for --json.
func toJSONError(err error, project string) jsonError {
	e := jsonError{Status: "error", ErrorKind: errKindInternal, Error: err.Error(), ExitCode: exitCodeOf(err)}
	var configErr *manager.ConfigError
	switch {
	case errors.Is(err, manager.ErrBusy):
		e.ErrorKind = errKindBusy
		if project != "" {
			e.Next = []string{"j-cicd logs " + project + " -f", "j-cicd cancel " + project}
		}
	case errors.Is(err, errNoProject):
		e.ErrorKind = errKindNotFound
		e.Next = []string{"j-cicd list"}
	case errors.As(err, &configErr) || e.ExitCode == exitConfig:
		e.ErrorKind = errKindConfig
	case e.ExitCode == exitUsage:
		e.ErrorKind = errKindUsage
	}
	return e
}

// jsonFail prints err as a --json error object (if asJSON) and returns it,
// for failures before a run is recorded.
func jsonFail(err error, project string, asJSON bool) error {
	if err != nil && asJSON {
		_ = printJSON(toJSONError(err, project))
	}
	return err
}

// hasJSONFlag reports whether args ask for --json, for errors raised while
// parsing the args themselves.
func hasJSONFlag(args []string) bool {
	asJSON := false
	for _, a := range args {
		flagName, ok := strings.CutPrefix(a, "--")
		if !ok {
			flagName, ok = strings.CutPrefix(a, "-")
		}
		if !ok {
			continue
		}
		name, value, hasValue := strings.Cut(flagName, "=")
		if name != "json" {
			continue
		}
		if !hasValue {
			asJSON = true
		} else if b, err := strconv.ParseBool(value); err == nil {
			// The flag package accepts any strconv.ParseBool spelling.
			asJSON = b
		}
	}
	// The last one wins, as it does for the flag package.
	return asJSON
}

// nextSteps are commands worth running after a run that didn't succeed.
func nextSteps(r store.Run) []string {
	if r.Status == store.StatusSuccess || r.ID == "" {
		return nil
	}
	var next []string
	if text := failedStageText(r); text != "" {
		next = append(next, fmt.Sprintf("j-cicd logs %s --stage %s", r.ID, shellQuote(text)))
	}
	next = append(next, fmt.Sprintf("j-cicd logs %s --tail 200", r.ID))
	if r.Deployed && r.Kind != store.KindRollback {
		next = append(next, "j-cicd rollback "+r.Project)
	}
	if r.Failure != nil && r.Failure.Kind == store.FailureConfig {
		next = append(next, "j-cicd run "+r.Project+" --plan")
	}
	return next
}

// failedStageText is how the log marks the stage that failed ("step test"),
// the unambiguous form for --stage, or "" if no stage failed. A cancelled
// failure has no stage kind of its own, so the kind comes from the stage
// record.
func failedStageText(r store.Run) string {
	f := r.Failure
	if f == nil || f.Stage == "" || f.Kind == store.FailureConfig {
		return ""
	}
	for i := len(r.Stages) - 1; i >= 0; i-- {
		if s := r.Stages[i]; s.Name == f.Stage && s.Status == "failed" {
			return manager.StageMarkerText(s.Kind, s.Name)
		}
	}
	return f.Stage
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)

// shellQuote quotes s for a POSIX shell if it needs it.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
