package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"j-cicd/internal/cli"
	"j-cicd/internal/config"
	"j-cicd/internal/detect"
	"j-cicd/internal/pipeline"
)

// version can be stamped at build time (-ldflags "-X main.version=v1.2");
// otherwise cli.Version falls back to the git revision Go recorded.
var version string

func printUsage() {
	fmt.Fprint(os.Stderr, `j-cicd - run a project's CI/CD pipeline from a config file it provides

Usage:
  j-cicd run [project-dir]     run the pipeline (steps, docker build+push, then deploy)
  j-cicd init [project-dir]    scaffold a starter .cicd.yaml in project-dir
  j-cicd version               print the j-cicd version

Run "j-cicd <command> -h" for flags on a given command.
`)
}

// parseCommandFlags parses args for a subcommand that takes an optional
// project dir plus flags, in any order (see cli.ParseInterleaved). It
// returns the project dir ("" if none was given), and rejects anything
// left over rather than ignoring it.
func parseCommandFlags(fs *flag.FlagSet, args []string) (string, error) {
	positional, err := cli.ParseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "Usage: j-cicd %s [project-dir] [flags]\n\nFlags:\n", fs.Name())
			fs.SetOutput(os.Stderr)
			fs.PrintDefaults()
			return "", flag.ErrHelp
		}
		return "", fmt.Errorf("%w (run \"j-cicd %s -h\" for usage)", err, fs.Name())
	}

	if len(positional) > 1 {
		return "", fmt.Errorf("unexpected argument %q (run \"j-cicd %s -h\" for usage)", positional[1], fs.Name())
	}
	if len(positional) == 1 {
		return positional[0], nil
	}
	return "", nil
}

// runArgs is the resolved invocation for `j-cicd run`.
type runArgs struct {
	projectDir string
	configPath string
	push       bool
	// deploy is false for --no-deploy: build and push, but don't ask the
	// VPS to roll out what was pushed.
	deploy bool
	// deployDirty allows deploying a build with uncommitted changes.
	deployDirty bool
}

func parseRunArgs(args []string) (runArgs, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to pipeline config (default: <project-dir>/"+config.DefaultFile+")")
	noPush := fs.Bool("no-push", false, "build the image (if configured) but skip pushing it (implies --no-deploy)")
	noDeploy := fs.Bool("no-deploy", false, "push images but skip the deploy: stage")
	deployDirty := fs.Bool("deploy-dirty", false, "allow deploying a build of uncommitted changes")

	dir, err := parseCommandFlags(fs, args)
	if err != nil {
		return runArgs{}, err
	}

	parsed := runArgs{projectDir: dir, configPath: *configPath, push: !*noPush, deploy: !*noDeploy, deployDirty: *deployDirty}
	if parsed.projectDir == "" {
		// A --config with no project dir means "run that project": its
		// steps, step dirs, dockerfiles and build contexts all resolve
		// relative to the project the config lives in, not to wherever
		// j-cicd happens to be invoked from.
		if parsed.configPath != "" {
			parsed.projectDir = filepath.Dir(parsed.configPath)
		} else {
			parsed.projectDir = "."
		}
	}
	if parsed.configPath == "" {
		parsed.configPath = filepath.Join(parsed.projectDir, config.DefaultFile)
	}
	return parsed, nil
}

func cmdRun(args []string) error {
	parsed, err := parseRunArgs(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError(err)
	}

	// The revision tags each image with the commit it was built from, so
	// a deploy has something immutable to pin even though "latest" moves.
	revision, ok := detect.GitRevision(parsed.projectDir)
	if !ok {
		log.Printf("no git revision for %s — images will only be tagged \"latest\"", parsed.projectDir)
	}

	cfg, err := config.Load(parsed.configPath, revision)
	if err != nil {
		return &exitError{code: exitConfig, err: fmt.Errorf("load config: %w", err)}
	}

	// Ctrl-C / SIGTERM cancels the run: in-flight steps are killed rather
	// than left running after j-cicd is gone.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p := pipeline.New(parsed.projectDir, pipeline.Options{
		Push:        parsed.push,
		Deploy:      parsed.deploy,
		Revision:    revision,
		DeployDirty: parsed.deployDirty,
	})
	if err := p.Run(ctx, cfg); err != nil {
		return fmt.Errorf("pipeline failed: %w", err)
	}
	return nil
}

// initArgs is the resolved invocation for `j-cicd init`.
type initArgs struct {
	projectDir string
	name       string
	force      bool
}

func parseInitArgs(args []string) (initArgs, error) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	name := fs.String("name", "", "project name to write into the config (default: directory name)")
	force := fs.Bool("force", false, "overwrite an existing config file")

	dir, err := parseCommandFlags(fs, args)
	if err != nil {
		return initArgs{}, err
	}
	if dir == "" {
		dir = "."
	}
	return initArgs{projectDir: dir, name: *name, force: *force}, nil
}

func cmdInit(args []string) error {
	parsed, err := parseInitArgs(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError(err)
	}

	res, err := detect.WriteStarterConfig(parsed.projectDir, parsed.name, parsed.force)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}

	// Reporting happens here rather than inside detect, so everything
	// j-cicd says goes through one logger on one stream.
	log.Printf("wrote %s", res.Path)
	if len(res.Dockerfiles) == 0 {
		log.Printf("no Dockerfile found — add an images: entry by hand once you have one")
	} else {
		log.Printf("found %d Dockerfile(s): %v", len(res.Dockerfiles), res.Dockerfiles)
	}
	if !res.GitHubRemote {
		log.Printf("no GitHub remote detected — repository: in the generated config is a placeholder, edit it before running with push")
	}
	return nil
}
