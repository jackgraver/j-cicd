package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
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
                                                       deploy images, health-check, roll back on failure
  j-cicd-agent deploy <project> <name@sha256:...>... [--artifacts-stdin] [--revision rev]
                                                       install files, restart, check, roll back on failure
  j-cicd-agent rollback <project>                      revert the last deploy (images only, not the database)
  j-cicd-agent status [project]                        show what is deployed and what it rolls back to
  j-cicd-agent version                                 print the version

Config: --config, else $`+configEnv+`, else `+agent.DefaultConfigFile+`.
`)
}

// parseArgs parses flags (on either side of the positionals) for a
// subcommand that only takes --config, and returns the positionals and
// the config path.
func parseArgs(name, usage string, args []string) ([]string, string, error) {
	return parseArgsWith(flag.NewFlagSet(name, flag.ContinueOnError), name, usage, args)
}

// parseArgsWith is parseArgs for a subcommand with flags of its own,
// already defined on fs.
func parseArgsWith(fs *flag.FlagSet, name, usage string, args []string) ([]string, string, error) {
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

// maxCompose bounds a compose file sent with a deploy. It arrives inside
// the ssh command line, which the kernel caps at 128 KiB per argument.
const maxCompose = 64 << 10

func cmdDeploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	composeB64 := fs.String("compose-b64", "", "the release's compose file, base64-encoded (sent by j-cicd run)")
	artifactsStdin := fs.Bool("artifacts-stdin", false, "read the files being deployed from stdin, as a tar (sent by j-cicd run)")
	revision := fs.String("revision", "", "the revision the files were built from (for the record and check:)")
	positional, configPath, err := parseArgsWith(fs, "deploy", "<project> <repo:tag|repo@sha256:...|name@sha256:...>...", args)
	if err != nil {
		return err
	}
	if len(positional) < 2 {
		return usageError(errors.New("deploy needs a project and at least one repo:tag, repo@sha256:... or name@sha256:... reference"))
	}
	var compose []byte
	if *composeB64 != "" {
		if compose, err = base64.StdEncoding.DecodeString(*composeB64); err != nil {
			return usageError(fmt.Errorf("--compose-b64: %w", err))
		}
		if len(compose) == 0 || len(compose) > maxCompose {
			return usageError(fmt.Errorf("--compose-b64: compose file must be 1 byte to %d KiB, got %d bytes", maxCompose>>10, len(compose)))
		}
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	ctx, stop := deployContext()
	defer stop()

	a := agent.New(cfg, agent.Options{})
	proj, err := cfg.Project(positional[0])
	if err != nil {
		return err
	}
	if proj.IsFiles() {
		if compose != nil {
			return usageError(errors.New("--compose-b64 goes with images; project " + proj.Name + " installs files"))
		}
		var in io.Reader
		if *artifactsStdin {
			in = os.Stdin
		}
		err = a.DeployFiles(ctx, proj.Name, positional[1:], *revision, in)
	} else {
		if *artifactsStdin || *revision != "" {
			return usageError(errors.New("--artifacts-stdin and --revision go with files; project " + proj.Name + " runs compose services"))
		}
		err = a.Deploy(ctx, proj.Name, positional[1:], compose)
	}
	if err != nil {
		return fmt.Errorf("deploy failed: %w", err)
	}
	return nil
}

func cmdRollback(args []string) error {
	positional, configPath, err := parseArgs("rollback", "<project>", args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usageError(errors.New("rollback needs exactly one project"))
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	ctx, stop := deployContext()
	defer stop()

	a := agent.New(cfg, agent.Options{})
	if err := a.Rollback(ctx, positional[0]); err != nil {
		return fmt.Errorf("rollback failed: %w", err)
	}
	return nil
}

// deployContext is the context for anything that changes what's running.
// The build host's ssh session can drop mid-deploy (network blip, its own
// timeout). Losing the terminal must not abandon a half-finished rollout:
// ignore SIGHUP, and SIGPIPE so writing progress to a closed connection
// doesn't kill the process before it can finish or roll back. Ctrl-C /
// SIGTERM still cancel, and cancellation itself triggers the rollback.
func deployContext() (context.Context, context.CancelFunc) {
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	fmt.Fprintln(w, "PROJECT\tSERVICE/FILE\tCURRENT\tPREVIOUS\tLAST DEPLOY\tLAST RESULT")
	for _, p := range projects {
		st, err := agent.LoadState(cfg.StateDir, p.Name)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(p.Services)+len(p.Files))
		for _, s := range p.Services {
			names = append(names, s.Name)
		}
		for _, f := range p.Files {
			names = append(names, f.Name)
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
		if c := st.Compose; c != nil {
			fmt.Fprintf(w, "%s\t(compose file)\t%s\t%s\t%s\t\n", p.Name, composeLabel(c.Current), composeLabel(c.Previous), c.CurrentSince.Format("2006-01-02 15:04:05Z"))
		}
	}
	return w.Flush()
}

// composeLabel names a release's compose file by a short content hash.
func composeLabel(hash string) string {
	if hash == "" {
		return "configured"
	}
	return "sha256:" + hash[:min(12, len(hash))]
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
