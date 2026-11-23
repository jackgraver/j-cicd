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
	// errKindRefused is a run j-cicd would not start (a dirty tree, no
	// per-commit tag): fix what the message names, don't report a bug.
	errKindRefused = "refused"
)

// errRefused wraps the reason a plan says a run would be refused.
var errRefused = errors.New("refused")

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
	case errors.Is(err, errRefused):
		e.ErrorKind = errKindRefused
		if project != "" {
			e.Next = []string{"j-cicd run " + project + " --plan"}
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
func hasJSONFlag(args []string) bool { return hasBoolFlag(args, "json") }

// hasBoolFlag reports whether args set the boolean flag name.
func hasBoolFlag(args []string, want string) bool {
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
		if name != want {
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

// briefLine is --brief's whole output for a run that succeeded:
//
//	ok simple-track 7b2eed0ec256 deployed verified 43s
//
// The words after the revision say what the run did.
func briefLine(r store.Run) string {
	rev := r.Revision
	if rev == "" {
		rev = "-"
	}
	words := []string{"ok", r.Project, rev}
	switch {
	case r.Kind == store.KindRollback:
		words = append(words, "rolled-back")
	case r.Kind == store.KindCheck:
		words = append(words, "checked")
	case r.Deployed:
		words = append(words, "deployed")
	case len(r.Images) > 0:
		words = append(words, "pushed")
	case len(r.Artifacts) > 0:
		words = append(words, "hashed")
	default:
		words = append(words, "built")
	}
	if len(r.Verified) > 0 {
		words = append(words, "verified")
	}
	words = append(words, fmt.Sprintf("%.0fs", r.Duration().Seconds()))
	return strings.Join(words, " ")
}

// jsonBriefFailure is --brief's output for a run that didn't succeed: only
// what is needed to act on the failure, not the stages that passed.
type jsonBriefFailure struct {
	Status   string            `json:"status"`
	ID       string            `json:"id"`
	Project  string            `json:"project"`
	Revision string            `json:"revision,omitempty"`
	Deployed bool              `json:"deployed,omitempty"`
	Failure  *store.RunFailure `json:"failure,omitempty"`
	ExitCode int               `json:"exit_code"`
	Next     []string          `json:"next,omitempty"`
}

func toJSONBriefFailure(r store.Run, exitCode int) jsonBriefFailure {
	return jsonBriefFailure{Status: string(r.Status), ID: r.ID, Project: r.Project, Revision: r.Revision,
		Deployed: r.Deployed, Failure: r.Failure, ExitCode: exitCode, Next: nextSteps(r)}
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
