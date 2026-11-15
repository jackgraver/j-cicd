package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"

	"j-cicd/internal/agent"
	"j-cicd/internal/cli"
)

// version can be stamped at build time (-ldflags "-X main.version=v1.2");
// otherwise cli.Version falls back to the git revision Go recorded.
var version string

// configEnv lets the ssh command on the build host point at a
// non-default config without needing flags in a particular place.
const configEnv = "J_CICD_AGENT_CONFIG"

func printUsage() {
	fmt.Fprint(os.Stderr, `j-cicd-agent - roll out images pushed by j-cicd on this host

Usage:
  j-cicd-agent deploy <project> <repo:tag|repo@sha256:...>...
                                                       deploy, health-check, roll back on failure
  j-cicd-agent status [project]                        show what is deployed and what it rolls back to
  j-cicd-agent version                                 print the version

Config: --config, else $`+configEnv+`, else `+agent.DefaultConfigFile+`.
`)
}

// parseArgs parses flags (on either side of the positionals) for a
// subcommand that only takes --config, and returns the positionals and
// the config path.
func parseArgs(name, usage string, args []string) ([]string, string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	configPath := fs.String("config", "", "agent config (default: $"+configEnv+", else "+agent.DefaultConfigFile+")")
	positional, err := cli.ParseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "Usage: j-cicd-agent %s %s [flags]\n\nFlags:\n", name, usage)
			fs.SetOutput(os.Stderr)
			fs.PrintDefaults()
			return nil, "", flag.ErrHelp
		}
		return nil, "", usageError(fmt.Errorf("%w (run \"j-cicd-agent %s -h\" for usage)", err, name))
	}
	path := *configPath
	if path == "" {
		path = os.Getenv(configEnv)
	}
	if path == "" {
		path = agent.DefaultConfigFile
	}
	return positional, path, nil
}

func loadConfig(path string) (*agent.Config, error) {
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		return nil, &exitError{code: exitConfig, err: fmt.Errorf("load config: %w", err)}
	}
	return cfg, nil
}

func cmdDeploy(args []string) error {
	positional, configPath, err := parseArgs("deploy", "<project> <repo:tag|repo@sha256:...>...", args)
	if err != nil {
		return err
	}
	if len(positional) < 2 {
		return usageError(errors.New("deploy needs a project and at least one repo:tag or repo@sha256:... reference"))
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	// The build host's ssh session can drop mid-deploy (network blip,
	// its own timeout). Losing the terminal must not abandon a
	// half-finished rollout: ignore SIGHUP, and SIGPIPE so writing
	// progress to a closed connection doesn't kill the process before
	// it can finish or roll back. Ctrl-C / SIGTERM still cancel, and
	// cancellation itself triggers the rollback.
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := agent.New(cfg, agent.Options{})
	if err := a.Deploy(ctx, positional[0], positional[1:]); err != nil {
		return fmt.Errorf("deploy failed: %w", err)
	}
	return nil
}

func cmdStatus(args []string) error {
	positional, configPath, err := parseArgs("status", "[project]", args)
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		return usageError(fmt.Errorf("unexpected argument %q", positional[1]))
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	projects := cfg.Projects
	if len(positional) == 1 {
		p, err := cfg.Project(positional[0])
		if err != nil {
			return usageError(err)
		}
		projects = []agent.Project{*p}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PROJECT\tSERVICE\tCURRENT\tPREVIOUS\tLAST DEPLOY\tLAST RESULT")
	for _, p := range projects {
		st, err := agent.LoadState(cfg.StateDir, p.Name)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(p.Services))
		for _, s := range p.Services {
			names = append(names, s.Name)
		}
		sort.Strings(names)
		for _, name := range names {
			ss := st.Services[name]
			if ss == nil {
				fmt.Fprintf(w, "%s\t%s\t-\t-\t-\tnever deployed\n", p.Name, name)
				continue
			}
			last := "-"
			if !ss.LastDeploy.IsZero() {
				last = ss.LastDeploy.Format("2006-01-02 15:04:05Z")
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, name, release(ss.CurrentTag, ss.Current), release(ss.PreviousTag, ss.Previous), last, orDash(ss.LastResult))
		}
	}
	return w.Flush()
}

// release shows a recorded release as its tag (the commit, normally)
// plus the short digest that actually runs.
func release(tag, digest string) string {
	switch {
	case digest == "":
		return "-"
	case tag == "":
		return agent.ShortDigest(digest)
	default:
		return tag + " (" + agent.ShortDigest(digest) + ")"
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
