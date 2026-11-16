package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Deploy outcomes recorded in ServiceState.LastResult.
const (
	resultHealthy         = "healthy"
	resultRolledBack      = "failed, rolled back"
	resultRollbackFailed  = "failed, rollback failed"
	resultNoRollback      = "failed, no previous release to roll back to"
	resultUntouched       = "failed to pull, nothing changed"
	resultPreDeployFailed = "pre_deploy failed, nothing changed"
)

// State is what the agent remembers about one project between deploys:
// what is running, and what to roll back to. It answers both without
// interrogating docker, and survives restarts.
type State struct {
	Services map[string]*ServiceState `json:"services"`
}

// ServiceState tracks one compose service.
type ServiceState struct {
	Repository string `json:"repository"`
	// Current is the last release that passed its health check, as
	// repo@sha256:...: what should be running, and the rollback target
	// for the next deploy.
	Current string `json:"current,omitempty"`
	// CurrentTag is the tag Current was deployed as (normally the
	// commit it was built from), for humans; the digest is what's used.
	CurrentTag string `json:"current_tag,omitempty"`
	// CurrentSince is when Current was deployed. The services sharing
	// the latest one are what the most recent deploy changed, and so
	// what `rollback` reverts.
	CurrentSince time.Time `json:"current_since,omitzero"`
	// Previous is the release Current replaced.
	Previous    string `json:"previous,omitempty"`
	PreviousTag string `json:"previous_tag,omitempty"`
	// LastAttempt is the most recent release tried, healthy or not.
	LastAttempt string    `json:"last_attempt,omitempty"`
	LastDeploy  time.Time `json:"last_deploy,omitzero"`
	LastResult  string    `json:"last_result,omitempty"`
}

// since is when Current was deployed. State written before CurrentSince
// existed falls back to the last deploy attempt, which is the same thing
// unless a later deploy of this service failed.
func (ss *ServiceState) since() time.Time {
	if !ss.CurrentSince.IsZero() {
		return ss.CurrentSince
	}
	return ss.LastDeploy
}

func statePath(stateDir, project string) string {
	return filepath.Join(stateDir, project+".json")
}

// LoadState reads a project's state. A project that has never been
// deployed has empty state, not an error.
func LoadState(stateDir, project string) (*State, error) {
	st := &State{Services: map[string]*ServiceState{}}
	data, err := os.ReadFile(statePath(stateDir, project))
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("corrupt state file %s: %w", statePath(stateDir, project), err)
	}
	if st.Services == nil {
		st.Services = map[string]*ServiceState{}
	}
	return st, nil
}

// service returns the state for a service, creating it if needed.
func (s *State) service(svc *Service) *ServiceState {
	ss := s.Services[svc.Name]
	if ss == nil {
		ss = &ServiceState{}
		s.Services[svc.Name] = ss
	}
	ss.Repository = svc.Repository
	return ss
}

// save writes the state atomically (temp file + rename), so a crash
// mid-write can't leave the rollback target unreadable.
func (s *State) save(stateDir, project string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, project+".json.tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), statePath(stateDir, project))
}
