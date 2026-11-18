package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Status is where a run is in its life.
type Status string

const (
	StatusRunning   Status = "running"
	StatusSuccess   Status = "success"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
	// StatusInterrupted is a run whose process went away without
	// recording an outcome (killed, crashed, machine rebooted).
	StatusInterrupted Status = "interrupted"
)

// Done reports whether the run has finished, one way or another.
func (s Status) Done() bool { return s != StatusRunning }

// DefaultKeepRuns is how many finished runs per project are kept before
// the oldest are pruned. Each one carries a full build log, so history
// can't be allowed to grow without bound.
const DefaultKeepRuns = 50

// KindRollback marks a run that reverted the project's last deploy
// instead of running its pipeline.
const KindRollback = "rollback"

// Run is one recorded pipeline run.
type Run struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	// Kind is "" for a pipeline run, or KindRollback.
	Kind   string `json:"kind,omitempty"`
	Dir    string `json:"dir"`
	Config string `json:"config"`
	// Revision is the git revision the run built ("" if not a git tree).
	Revision string `json:"revision,omitempty"`
	Push     bool   `json:"push"`
	// Images are the images this run pushed, with the registry digest
	// each resolved to; Deployed is whether the deploy stage then handed
	// them to DeployHost. Both are empty for a run that didn't get that far.
	Images     []RunImage `json:"images,omitempty"`
	Deployed   bool       `json:"deployed,omitempty"`
	DeployHost string     `json:"deploy_host,omitempty"`
	Status     Status     `json:"status"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt time.Time  `json:"finished_at,omitzero"`
	// PID is the j-cicd process executing the run. It's how a run that
	// died without finishing is told apart from one still going, and what
	// a UI signals to cancel it.
	PID int `json:"pid"`
}

// RunImage is one image a run built and pushed.
type RunImage struct {
	Name       string   `json:"name"`
	Repository string   `json:"repository"`
	Tags       []string `json:"tags"`
	// Digest is the registry's content hash for the push (sha256:...),
	// "" if it couldn't be read back.
	Digest string `json:"digest,omitempty"`
}

// Duration is how long the run took, or has taken so far.
func (r Run) Duration() time.Duration {
	end := r.FinishedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(r.StartedAt)
}

func (s *Store) runDir(id string) string { return filepath.Join(s.root, "runs", id) }

// LogPath is the file holding a run's combined output.
func (s *Store) LogPath(id string) string { return filepath.Join(s.runDir(id), "output.log") }

func (s *Store) runPath(id string) string { return filepath.Join(s.runDir(id), "run.json") }

// newRunID returns an ID that sorts by start time, with a random suffix so
// two runs started in the same second don't collide.
func newRunID(now time.Time) (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:]), nil
}

// RunHandle is a run in progress, owned by the process executing it.
type RunHandle struct {
	s   *Store
	Run Run
	// Log receives the run's output. Finish closes it.
	Log *os.File
}

// StartRun records the start of a run of r.Project and opens its log. ID,
// StartedAt, Status and PID are filled in.
func (s *Store) StartRun(r Run) (*RunHandle, error) {
	now := time.Now()
	id, err := newRunID(now)
	if err != nil {
		return nil, err
	}
	r.ID, r.StartedAt, r.Status, r.PID = id, now.UTC(), StatusRunning, os.Getpid()
	r.FinishedAt = time.Time{}

	if err := os.MkdirAll(s.runDir(id), 0o755); err != nil {
		return nil, fmt.Errorf("creating run dir: %w", err)
	}
	logFile, err := os.Create(s.LogPath(id))
	if err != nil {
		return nil, fmt.Errorf("creating run log: %w", err)
	}
	if err := writeJSON(s.runPath(id), r); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("recording run: %w", err)
	}

	// Pruning is housekeeping: a failure here mustn't stop the run.
	_ = s.prune(r.Project, DefaultKeepRuns)
	return &RunHandle{s: s, Run: r, Log: logFile}, nil
}

// Finish records the run's outcome and closes its log. errMsg is the
// failure reason, if any.
func (h *RunHandle) Finish(status Status, errMsg string) error {
	h.Run.Status = status
	h.Run.Error = errMsg
	h.Run.FinishedAt = time.Now().UTC()
	closeErr := h.Log.Close()
	if err := writeJSON(h.s.runPath(h.Run.ID), h.Run); err != nil {
		return fmt.Errorf("recording run result: %w", err)
	}
	return closeErr
}

// GetRun returns the run with the given ID.
func (s *Store) GetRun(id string) (Run, error) {
	// An ID is a single path element; anything else could read outside
	// the runs dir.
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return Run{}, fmt.Errorf("run %q: %w", id, ErrNotFound)
	}
	var r Run
	if err := readJSON(s.runPath(id), &r); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Run{}, fmt.Errorf("run %q: %w", id, ErrNotFound)
		}
		return Run{}, err
	}
	return s.reconcile(r), nil
}

// Runs returns recorded runs of project ("" for every project), newest
// first. limit <= 0 means no limit.
func (s *Store) Runs(project string, limit int) ([]Run, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "runs"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	// IDs sort by start time, so newest-first is a reverse name sort.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })

	var runs []Run
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var r Run
		if err := readJSON(s.runPath(e.Name()), &r); err != nil {
			// A run dir caught between mkdir and its first write, or
			// mangled by hand: skip it rather than hide all history.
			continue
		}
		if project != "" && r.Project != project {
			continue
		}
		runs = append(runs, s.reconcile(r))
		if limit > 0 && len(runs) == limit {
			break
		}
	}
	return runs, nil
}

// LastRuns returns the most recent run of each project that has one.
func (s *Store) LastRuns() (map[string]Run, error) {
	runs, err := s.Runs("", 0)
	if err != nil {
		return nil, err
	}
	last := make(map[string]Run)
	for _, r := range runs {
		if _, seen := last[r.Project]; !seen {
			last[r.Project] = r
		}
	}
	return last, nil
}

// reconcile turns a "running" record whose process is gone into an
// interrupted one, and saves that so it only has to be worked out once.
func (s *Store) reconcile(r Run) Run {
	if r.Status != StatusRunning || r.PID == os.Getpid() || processAlive(r.PID) {
		return r
	}
	r.Status = StatusInterrupted
	r.Error = "the j-cicd process running this stopped without recording a result"
	if info, err := os.Stat(s.LogPath(r.ID)); err == nil {
		r.FinishedAt = info.ModTime().UTC()
	}
	_ = writeJSON(s.runPath(r.ID), r)
	return r
}

// prune deletes all but the newest keep finished runs of project.
func (s *Store) prune(project string, keep int) error {
	runs, err := s.Runs(project, 0)
	if err != nil {
		return err
	}
	finished := 0
	for _, r := range runs {
		if !r.Status.Done() {
			continue
		}
		finished++
		if finished > keep {
			if err := os.RemoveAll(s.runDir(r.ID)); err != nil {
				return err
			}
		}
	}
	return nil
}
