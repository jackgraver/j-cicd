package pipeline

import (
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Stage kinds, as recorded in a run and reported to agents.
const (
	KindPreflight = "preflight"
	KindDocker    = "docker-check"
	KindStep      = "step"
	KindBuild     = "build"
	KindPush      = "push"
	KindArtifacts = "artifacts"
	KindDeploy    = "deploy"
	KindVerify    = "verify"
	KindRollback  = "rollback"
)

// Stage statuses.
const (
	StageRunning = "running"
	StageOK      = "ok"
	StageFailed  = "failed"
)

// StageResult is how one stage of the last Run went.
type StageResult struct {
	Name, Kind, Status string
	Started            time.Time
	Duration           time.Duration
	// ExitCode is the failing command's exit status, or -1 if the stage
	// didn't fail on one (it passed, timed out, or failed some other way).
	ExitCode int
	TimedOut bool
}

// StageError is a failure attributed to one stage. Its text is the same
// message the run always had; the fields are what a program can read
// without parsing it.
type StageError struct {
	Kind, Stage string
	// ExitCode is the command's exit status, or -1 if it didn't exit
	// (not found, killed, timed out) or this wasn't a command.
	ExitCode int
	TimedOut bool
	msg      string
	err      error
}

func (e *StageError) Error() string { return e.msg }
func (e *StageError) Unwrap() error { return e.err }

// stageErr wraps err as a failure of the given stage with message msg.
func stageErr(kind, stage string, err error, format string, args ...any) *StageError {
	e := &StageError{Kind: kind, Stage: stage, ExitCode: -1, msg: fmt.Sprintf(format, args...), err: err}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() >= 0 {
		e.ExitCode = exit.ExitCode()
	}
	var to *timeoutError
	e.TimedOut = errors.As(err, &to)
	return e
}

// timeoutError is a command that outlived its deadline.
type timeoutError struct{ after time.Duration }

func (e *timeoutError) Error() string { return fmt.Sprintf("timed out after %s", e.after) }

// Stages returns the stages the last Run started, in order.
func (p *Pipeline) Stages() []StageResult { return p.stages }

// stage starts a new stage, closing the previous one as passed. Whatever
// fails next is attributed to it until another begins.
func (p *Pipeline) stage(kind, name string) {
	p.closeStage(nil)
	p.stages = append(p.stages, StageResult{Name: name, Kind: kind, Status: StageRunning, Started: time.Now(), ExitCode: -1})
	if p.onStage != nil {
		p.onStage(kind, name)
	}
}

// currentStage is the name of the stage most recently begun.
func (p *Pipeline) currentStage() string {
	if len(p.stages) == 0 {
		return ""
	}
	return p.stages[len(p.stages)-1].Name
}

// closeStage ends the running stage: failed if err is non-nil, else passed.
func (p *Pipeline) closeStage(err error) {
	if len(p.stages) == 0 {
		return
	}
	s := &p.stages[len(p.stages)-1]
	if s.Status != StageRunning {
		return
	}
	s.Duration = time.Since(s.Started)
	if err == nil {
		s.Status = StageOK
		return
	}
	s.Status = StageFailed
	var se *StageError
	if errors.As(err, &se) {
		s.ExitCode, s.TimedOut = se.ExitCode, se.TimedOut
	}
}
