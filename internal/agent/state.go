package agent

import (
	"crypto/sha256"
	"encoding/hex"
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
	// Compose tracks the compose file releases were deployed with, when
	// the build host sends one (deploy.compose_file in .cicd.yaml). Nil
	// means every deploy so far used the file in the agent's config.
	Compose *ComposeState `json:"compose,omitempty"`
}

// ComposeState is the compose file that goes with Current, and the one
// before it, by the sha256 of their contents ("" meaning the file named
// in the agent config). The files themselves are kept in
// <state_dir>/<project>.compose/<sha256>.yml.
type ComposeState struct {
	Current  string `json:"current,omitempty"`
	Previous string `json:"previous,omitempty"`
	// CurrentSince is when Current was deployed; equal to the services'
	// CurrentSince when the same deploy changed both.
	CurrentSince time.Time `json:"current_since,omitzero"`
}

// composeCurrent is the hash of the compose file in use ("" for the
// configured one).
func (s *State) composeCurrent() string {
	if s.Compose == nil {
		return ""
	}
	return s.Compose.Current
}

func composeDir(stateDir, project string) string {
	return filepath.Join(stateDir, project+".compose")
}

func composePath(stateDir, project, hash string) string {
	return filepath.Join(composeDir(stateDir, project), hash+".yml")
}

// storeCompose saves a compose file sent with a deploy under its content
// hash and returns the hash. Storing the same file twice is a no-op.
func storeCompose(stateDir, project string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	dir := composeDir(stateDir, project)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := composePath(stateDir, project, hash)
	if _, err := os.Stat(path); err == nil {
		return hash, nil
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return hash, os.Rename(tmp.Name(), path)
}

// pruneCompose deletes stored compose files other than the current and
// previous ones: nothing can roll back further than that.
func (s *State) pruneCompose(stateDir, project string) error {
	entries, err := os.ReadDir(composeDir(stateDir, project))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	if s.Compose != nil {
		keep[s.Compose.Current+".yml"] = true
		keep[s.Compose.Previous+".yml"] = true
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			if err := os.Remove(filepath.Join(composeDir(stateDir, project), e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
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
