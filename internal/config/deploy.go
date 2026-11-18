package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DefaultAgentCommand is what the worker runs on the deploy host when
// deploy.command isn't set: the j-cicd-agent binary, found on the remote
// user's PATH.
const DefaultAgentCommand = "j-cicd-agent"

// Deploy is the optional last stage of a run: once every image has been
// pushed, j-cicd connects to the deploy host over SSH and has the
// j-cicd-agent there pull and roll out exactly the digests that were just
// pushed. The mapping from image to compose service lives in the agent's
// own config on the VPS, next to the compose file it drives, so all this
// side needs to know is where to connect.
type Deploy struct {
	// Host is the SSH destination: user@host, or an alias from
	// ~/.ssh/config (which is where ports, keys and jump hosts belong).
	Host string `yaml:"host"`
	// Command is how to invoke the agent on the host, e.g.
	// "/usr/local/bin/j-cicd-agent" or "sudo j-cicd-agent". It is run by
	// the remote shell, so it may carry arguments or env assignments.
	Command string `yaml:"command,omitempty"`
	// Project names the project in the agent's config. Defaults to the
	// pipeline's own name.
	Project string `yaml:"project,omitempty"`
	// Images restricts the deploy to these images: entries, by name.
	// Defaults to all of them.
	Images []string `yaml:"images,omitempty"`
	// Timeout bounds the whole remote deploy, including the agent's
	// health wait and any rollback. Falls back to the top-level timeout.
	Timeout *Duration `yaml:"timeout,omitempty"`
	// ComposeFile, relative to the project dir, is sent to the agent with
	// every deploy, from the same commit as the images, so the deploy
	// host runs the compose file that goes with the release (and a
	// rollback restores the one that went with the previous release).
	// Unset, the agent uses the compose file named in its own config.
	ComposeFile string `yaml:"compose_file,omitempty"`
	// Verify are checks run against the live site after the agent reports
	// a healthy deploy, to prove users are being served this release.
	Verify []Verify `yaml:"verify,omitempty"`
}

// DefaultVerifyTimeout is how long a verify check keeps retrying before
// the run fails: containers, proxies and caches take a moment to settle.
const DefaultVerifyTimeout = 2 * time.Minute

// RevisionPlaceholder in a verify url or expect_body_contains is replaced
// by the revision that was just deployed.
const RevisionPlaceholder = "{{revision}}"

// Verify is one post-deploy check: fetch URL (from the build host, the
// way users reach it) until it answers as expected, or Timeout passes.
// The usual use is a version endpoint whose body contains the commit the
// build was made from, which catches a healthy container behind a stale
// cache or proxy.
type Verify struct {
	URL string `yaml:"url"`
	// ExpectStatus is the HTTP status that counts as success. Default 200.
	ExpectStatus int `yaml:"expect_status,omitempty"`
	// ExpectBodyContains, if set, must appear in the response body.
	// {{revision}} stands for the deployed revision.
	ExpectBodyContains string `yaml:"expect_body_contains,omitempty"`
	// Timeout is how long to keep retrying. Default DefaultVerifyTimeout;
	// an explicit 0 means a single attempt.
	Timeout *Duration `yaml:"timeout,omitempty"`
}

// RetryFor returns how long the check keeps retrying.
func (v *Verify) RetryFor() time.Duration {
	if v.Timeout != nil {
		return v.Timeout.Duration
	}
	return DefaultVerifyTimeout
}

func (v *Verify) applyDefaults() error {
	u, err := url.Parse(strings.ReplaceAll(v.URL, RevisionPlaceholder, "x"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("url %q must be an http:// or https:// URL", v.URL)
	}
	if v.ExpectStatus == 0 {
		v.ExpectStatus = 200
	}
	if v.ExpectStatus < 100 || v.ExpectStatus > 599 {
		return fmt.Errorf("expect_status %d is not an HTTP status", v.ExpectStatus)
	}
	if v.Timeout != nil && v.Timeout.Duration < 0 {
		return errors.New("timeout can't be negative")
	}
	return nil
}

// projectName is deliberately narrow: the agent uses it as a file name for
// its lock and state, and it travels through a remote shell.
var projectName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidProjectName reports whether name can be used as a deploy project
// name, on either side of the SSH connection.
func ValidProjectName(name string) bool {
	return projectName.MatchString(name)
}

func (d *Deploy) applyDefaults(c *Config) error {
	if d.Host == "" {
		return errors.New("host is required")
	}
	// ssh would parse a leading dash as an option rather than a host.
	if strings.HasPrefix(d.Host, "-") || strings.ContainsAny(d.Host, " \t\n") {
		return fmt.Errorf("host %q is not a valid ssh destination", d.Host)
	}
	if strings.TrimSpace(d.Command) == "" {
		d.Command = DefaultAgentCommand
	}
	if d.Project == "" {
		d.Project = c.Name
	}
	if !ValidProjectName(d.Project) {
		return fmt.Errorf("project %q must contain only letters, digits, '.', '_' and '-' (set deploy.project if name: has other characters)", d.Project)
	}
	if len(c.Images) == 0 {
		return errors.New("there are no images: to deploy")
	}
	if d.ComposeFile != "" {
		// It's part of the release, so it has to come from the tree
		// being built, not from wherever this host keeps things.
		clean := filepath.Clean(d.ComposeFile)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("compose_file %q must be a path inside the project", d.ComposeFile)
		}
		d.ComposeFile = clean
	}

	for i := range d.Verify {
		if err := d.Verify[i].applyDefaults(); err != nil {
			return fmt.Errorf("verify[%d]: %w", i, err)
		}
	}

	if len(d.Images) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(d.Images))
	for _, name := range d.Images {
		if seen[name] {
			return fmt.Errorf("images lists %q twice", name)
		}
		seen[name] = true
		if c.imageByName(name) == nil {
			return fmt.Errorf("images lists %q, but no images: entry has that name", name)
		}
	}
	return nil
}

func (c *Config) imageByName(name string) *Image {
	for i := range c.Images {
		if c.Images[i].Name == name {
			return &c.Images[i]
		}
	}
	return nil
}

// DeployImages returns the images the deploy stage hands to the agent, in
// images: order.
func (c *Config) DeployImages() []*Image {
	if c.Deploy == nil {
		return nil
	}
	var out []*Image
	for i := range c.Images {
		img := &c.Images[i]
		if len(c.Deploy.Images) == 0 || containsString(c.Deploy.Images, img.Name) {
			out = append(out, img)
		}
	}
	return out
}

// DeployTimeout returns the effective limit for the remote deploy: the
// deploy section's own timeout, else the config-level default, else
// fallback. A zero duration means no limit.
func (c *Config) DeployTimeout(fallback time.Duration) time.Duration {
	if c.Deploy != nil && c.Deploy.Timeout != nil {
		return c.Deploy.Timeout.Duration
	}
	return c.ImageTimeout(fallback)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
