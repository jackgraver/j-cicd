package store

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"j-cicd/internal/config"
)

// Project is a registered project: a name to pick it by and the directory
// its pipeline runs in.
type Project struct {
	Name string `json:"name"`
	// Dir is absolute, so the project runs the same no matter where j-cicd
	// is started from.
	Dir string `json:"dir"`
	// Config is an absolute path to the pipeline config, or "" for the
	// default <Dir>/.cicd.yaml.
	Config  string    `json:"config,omitempty"`
	AddedAt time.Time `json:"added_at"`
}

// ConfigPath is the pipeline config this project runs with.
func (p Project) ConfigPath() string {
	if p.Config != "" {
		return p.Config
	}
	return filepath.Join(p.Dir, config.DefaultFile)
}

// nameRe keeps names usable as a bare CLI argument: no path separators (so
// `j-cicd run <x>` can tell a name from a directory) and no leading dot.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// ValidName reports whether name can be used as a project name.
func ValidName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid project name %q: use letters, digits, '.', '_' or '-', not starting with '.' or '-'", name)
	}
	return nil
}

type projectsFile struct {
	Projects []Project `json:"projects"`
}

func (s *Store) projectsPath() string { return filepath.Join(s.root, "projects.json") }

// Projects returns every registered project, sorted by name.
func (s *Store) Projects() ([]Project, error) {
	var f projectsFile
	if err := readJSON(s.projectsPath(), &f); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(f.Projects, func(i, j int) bool { return f.Projects[i].Name < f.Projects[j].Name })
	return f.Projects, nil
}

// Project returns the project registered as name.
func (s *Store) Project(name string) (Project, error) {
	projects, err := s.Projects()
	if err != nil {
		return Project{}, err
	}
	for _, p := range projects {
		if p.Name == name {
			return p, nil
		}
	}
	return Project{}, fmt.Errorf("project %q: %w", name, ErrNotFound)
}

// ProjectByDir returns the project registered for dir with the default
// config, if there is one. It lets `j-cicd run <dir>` record history under
// the project's registered name.
func (s *Store) ProjectByDir(dir string) (Project, bool, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Project{}, false, err
	}
	projects, err := s.Projects()
	if err != nil {
		return Project{}, false, err
	}
	for _, p := range projects {
		if p.Dir == abs && p.Config == "" {
			return p, true, nil
		}
	}
	return Project{}, false, nil
}

// AddProject registers p. Dir (and Config, if set) are made absolute. A
// name or a dir+config pair can only be registered once.
func (s *Store) AddProject(p Project) (Project, error) {
	if err := ValidName(p.Name); err != nil {
		return Project{}, err
	}
	var err error
	if p.Dir, err = filepath.Abs(p.Dir); err != nil {
		return Project{}, err
	}
	if p.Config != "" {
		if p.Config, err = filepath.Abs(p.Config); err != nil {
			return Project{}, err
		}
		// Spelling out the default config is the same as leaving it out.
		if p.Config == filepath.Join(p.Dir, config.DefaultFile) {
			p.Config = ""
		}
	}
	if p.AddedAt.IsZero() {
		p.AddedAt = time.Now().UTC()
	}

	projects, err := s.Projects()
	if err != nil {
		return Project{}, err
	}
	for _, existing := range projects {
		if existing.Name == p.Name {
			return Project{}, fmt.Errorf("a project named %q is already registered (for %s)", p.Name, existing.Dir)
		}
		if existing.Dir == p.Dir && existing.Config == p.Config {
			return Project{}, fmt.Errorf("%s is already registered as %q", p.ConfigPath(), existing.Name)
		}
	}
	projects = append(projects, p)
	if err := writeJSON(s.projectsPath(), projectsFile{Projects: projects}); err != nil {
		return Project{}, fmt.Errorf("saving projects: %w", err)
	}
	return p, nil
}

// RemoveProject unregisters name. Its run history is kept.
func (s *Store) RemoveProject(name string) error {
	projects, err := s.Projects()
	if err != nil {
		return err
	}
	kept := projects[:0]
	for _, p := range projects {
		if p.Name != name {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(projects) {
		return fmt.Errorf("project %q: %w", name, ErrNotFound)
	}
	if err := writeJSON(s.projectsPath(), projectsFile{Projects: kept}); err != nil {
		return fmt.Errorf("saving projects: %w", err)
	}
	return nil
}
