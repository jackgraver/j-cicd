// Package store is j-cicd's persistent state: the registry of projects it
// manages and the history of every run (its outcome and its full output).
// Everything lives as plain JSON and log files under one root directory,
// so it can be inspected, backed up or deleted by hand.
//
// Layout:
//
//	<root>/projects.json        registered projects
//	<root>/runs/<id>/run.json   one run's metadata
//	<root>/runs/<id>/output.log that run's combined output
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// HomeEnv overrides the state root, e.g. to keep a separate registry for
// testing or to put state on another disk.
const HomeEnv = "J_CICD_HOME"

// ErrNotFound is returned when a project or run doesn't exist.
var ErrNotFound = errors.New("not found")

// Store is a handle on the state root. It holds no open files, so any
// number of j-cicd processes (a UI plus the runs it launched) can share one
// root at once.
type Store struct {
	root string
}

// DefaultRoot returns $J_CICD_HOME, else $XDG_STATE_HOME/j-cicd, else
// ~/.local/state/j-cicd.
func DefaultRoot() (string, error) {
	if dir := os.Getenv(HomeEnv); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "j-cicd"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating state dir: %w (set %s)", err, HomeEnv)
	}
	return filepath.Join(home, ".local", "state", "j-cicd"), nil
}

// Open returns a Store rooted at root, creating the directory if needed.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "runs"), 0o755); err != nil {
		return nil, fmt.Errorf("creating state dir: %w", err)
	}
	return &Store{root: root}, nil
}

// Root is the directory the store lives in.
func (s *Store) Root() string { return s.root }

// writeJSON replaces path atomically, so a reader (another j-cicd process)
// never sees a half-written file and a crash mid-write can't corrupt it.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}
