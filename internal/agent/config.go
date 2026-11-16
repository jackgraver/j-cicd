// Package agent is the VPS half of j-cicd: given digests the build host
// just pushed, it rolls them out through docker compose, waits for the
// services' own healthchecks, and rolls back to the last healthy release
// if they don't pass. It is driven by `j-cicd-agent deploy`, which the
// build host runs over ssh as the last stage of `j-cicd run`.
package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"

	"j-cicd/internal/config"
)

// DefaultConfigFile is where the agent looks for its config when neither
// --config nor $J_CICD_AGENT_CONFIG says otherwise.
const DefaultConfigFile = "/etc/j-cicd-agent.yaml"

// Defaults for the optional config fields.
const (
	DefaultStateDir      = "/var/lib/j-cicd-agent"
	DefaultHealthTimeout = 90 * time.Second
	DefaultTimeout       = 15 * time.Minute
)

// Config is the agent's own config. It lives on the VPS, next to the
// compose files it drives, and is the single place that maps a pushed
// repository to the compose service that runs it.
type Config struct {
	// StateDir holds one lock and one state file per project.
	StateDir string    `yaml:"state_dir,omitempty"`
	Projects []Project `yaml:"projects"`
}

// Project is one compose stack the agent may deploy to.
type Project struct {
	// Name is what the build host's deploy.project refers to.
	Name        string `yaml:"name"`
	ComposeFile string `yaml:"compose_file"`
	// EnvFile is passed to compose as --env-file. Variables the agent
	// sets for a deploy override values in it.
	EnvFile string `yaml:"env_file,omitempty"`
	// HealthTimeout bounds the wait for deployed services to report
	// healthy. Size it from the compose healthchecks: roughly
	// start_period + interval × retries, plus slack.
	HealthTimeout *config.Duration `yaml:"health_timeout,omitempty"`
	// Timeout bounds a whole deploy (pull, up, health wait). A rollback
	// gets a fresh budget of the same size.
	Timeout *config.Duration `yaml:"timeout,omitempty"`
	// PreDeploy is a shell command run (sh -c, from the compose file's
	// directory) once the new images are pulled and before any container
	// is touched — e.g. a database backup. If it fails the deploy stops
	// with nothing changed. It runs for manual rollbacks too, but not for
	// the automatic rollback after a failed deploy (it already ran).
	// J_CICD_PROJECT, J_CICD_SERVICES (space-separated) and J_CICD_RELEASE
	// (e.g. "backend=7fc0b4b12345") describe what is about to change.
	PreDeploy string    `yaml:"pre_deploy,omitempty"`
	Services  []Service `yaml:"services"`
}

// Service ties a pushed repository to the compose service that runs it.
type Service struct {
	// Name is the compose service name.
	Name string `yaml:"name"`
	// ImageVar is the variable the compose file interpolates into the
	// service's image: (e.g. image: ${BACKEND_IMAGE:-...}). The agent
	// sets it to repo@sha256:... — this is the one piece of coupling
	// that can't be inferred.
	ImageVar   string `yaml:"image_var"`
	Repository string `yaml:"repository"`
}

var envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// LoadConfig reads and validates the agent config at path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// Same rule as .cicd.yaml: this decides what runs in production, so
	// a typo must not quietly become a default.
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: config is empty", path)
		}
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir
	}
	if len(c.Projects) == 0 {
		return errors.New("projects: must list at least one project")
	}
	names := map[string]bool{}
	for i := range c.Projects {
		p := &c.Projects[i]
		if !config.ValidProjectName(p.Name) {
			return fmt.Errorf("projects[%d].name %q must contain only letters, digits, '.', '_' and '-'", i, p.Name)
		}
		if names[p.Name] {
			return fmt.Errorf("project %q is listed twice", p.Name)
		}
		names[p.Name] = true
		if err := p.validate(); err != nil {
			return fmt.Errorf("project %q: %w", p.Name, err)
		}
	}
	return nil
}

func (p *Project) validate() error {
	if p.ComposeFile == "" {
		return errors.New("compose_file is required")
	}
	if len(p.Services) == 0 {
		return errors.New("services: must list at least one service")
	}
	// Each of these must be unambiguous: a repository that mapped to two
	// services, or two services sharing a variable, would deploy the
	// wrong thing without any error.
	names, vars, repos := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, s := range p.Services {
		switch {
		case s.Name == "":
			return fmt.Errorf("services[%d].name is required", i)
		case !envVarName.MatchString(s.ImageVar):
			return fmt.Errorf("services[%d].image_var %q is not a valid variable name", i, s.ImageVar)
		case s.Repository == "":
			return fmt.Errorf("services[%d].repository is required", i)
		case names[s.Name]:
			return fmt.Errorf("service %q is listed twice", s.Name)
		case vars[s.ImageVar]:
			return fmt.Errorf("image_var %s is used by two services", s.ImageVar)
		case repos[s.Repository]:
			return fmt.Errorf("repository %s is mapped to two services", s.Repository)
		}
		names[s.Name], vars[s.ImageVar], repos[s.Repository] = true, true, true
	}
	return nil
}

// Project returns the named project, or an error naming the ones that do
// exist.
func (c *Config) Project(name string) (*Project, error) {
	var known []string
	for i := range c.Projects {
		if c.Projects[i].Name == name {
			return &c.Projects[i], nil
		}
		known = append(known, c.Projects[i].Name)
	}
	return nil, fmt.Errorf("no project %q in the agent config (have %v)", name, known)
}

func (p *Project) healthTimeout() time.Duration {
	if p.HealthTimeout != nil {
		return p.HealthTimeout.Duration
	}
	return DefaultHealthTimeout
}

func (p *Project) timeout() time.Duration {
	if p.Timeout != nil {
		return p.Timeout.Duration
	}
	return DefaultTimeout
}

func (p *Project) serviceFor(repository string) *Service {
	for i := range p.Services {
		if p.Services[i].Repository == repository {
			return &p.Services[i]
		}
	}
	return nil
}
