package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"j-cicd/internal/config"
	"j-cicd/internal/detect"
	"j-cicd/internal/display"
	"j-cicd/internal/manager"
	"j-cicd/internal/store"
)

// version can be stamped at build time (-ldflags "-X main.version=v1.2");
// otherwise cli.Version falls back to the git revision Go recorded.
var version string

func printUsage() {
	fmt.Fprint(os.Stderr, `j-cicd - build, test and ship your projects from one place

Usage:
  j-cicd                       open the dashboard (when run in a terminal)
  j-cicd ui                    open the dashboard

  j-cicd add [project-dir]     register a project so it can be run by name
  j-cicd remove <name>         unregister a project (its history is kept)
  j-cicd list                  list projects and their latest result

  j-cicd run [name|dir]        run a pipeline (steps, docker build+push, then deploy)
  j-cicd history [name]        list recorded runs, newest first
  j-cicd logs <run-id|name>    print a run's output (a name means its latest run)
  j-cicd cancel <run-id|name>  stop a run in progress
  j-cicd rollback [name|dir]   revert the last deploy on the VPS (images only, not the database)

  j-cicd init [project-dir]    scaffold a starter .cicd.yaml in project-dir
  j-cicd version               print the j-cicd version

Output: at a terminal, run streams everything. Anywhere else (a pipe, a
script, an agent) it prints one line per stage and, if something fails, the
last lines of the failing command; -v streams everything, --quiet prints only
the final status. The full output is always recorded: j-cicd logs <run-id>.
--json (run, rollback, list, history) prints machine-readable results on
stdout; prose and progress always go to stderr. run --plan shows what would
happen without doing it.

Exit status: 0 success; 1 the pipeline (or rollback) failed or was cancelled;
2 usage error; 3 config could not be loaded. Check it directly: in a pipeline
like "j-cicd run x | tail", $? is tail's, not j-cicd's (use set -o pipefail).

Every run is recorded under the state dir (`+"$"+store.HomeEnv+`, else
$XDG_STATE_HOME/j-cicd, else ~/.local/state/j-cicd).
Run "j-cicd <command> -h" for flags on a given command.
`)
}

// parseCommandFlags parses args for a subcommand that takes an optional
// project dir plus flags, in any order. It returns the project dir ("" if
// none was given), and rejects anything left over rather than ignoring it.
func parseCommandFlags(fs *flag.FlagSet, args []string) (string, error) {
	positional, err := parseFlagsOnly(fs, args, "[project-dir] [flags]", 0, 1)
	if err != nil || len(positional) == 0 {
		return "", err
	}
	return positional[0], nil
}

// runArgs is the resolved invocation for `j-cicd run`.
type runArgs struct {
	projectDir string
	configPath string
	push       bool
	quiet      bool
	// deploy is false for --no-deploy: build and push, but don't ask the
	// VPS to roll out what was pushed.
	deploy bool
	// deployDirty allows deploying a build with uncommitted changes.
	deployDirty bool
	// verbose streams all command output; json prints the result as JSON
	// on stdout; plan only reports what would happen.
	verbose, json, plan bool
}

func parseRunArgs(args []string) (runArgs, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to pipeline config (default: <project-dir>/"+config.DefaultFile+")")
	noPush := fs.Bool("no-push", false, "build the image (if configured) but skip pushing it (implies --no-deploy)")
	quiet := fs.Bool("quiet", false, "print nothing but the final status (output is still recorded; see j-cicd logs)")
	verbose := fs.Bool("verbose", false, "stream every command's output (the default at a terminal; elsewhere only stage lines and a failure's last lines are shown)")
	fs.BoolVar(verbose, "v", false, "same as --verbose")
	asJSON := fs.Bool("json", false, "print the result as one JSON object on stdout (run id, status, commit, pushed image digests)")
	plan := fs.Bool("plan", false, "show what would run (commit, steps, images, deploy host) without running, building, pushing or deploying")
	noDeploy := fs.Bool("no-deploy", false, "push images but skip the deploy: stage")
	deployDirty := fs.Bool("deploy-dirty", false, "allow deploying a build of uncommitted changes")

	dir, err := parseCommandFlags(fs, args)
	if err != nil {
		return runArgs{}, err
	}

	parsed := runArgs{projectDir: dir, configPath: *configPath, push: !*noPush, quiet: *quiet, deploy: !*noDeploy, deployDirty: *deployDirty,
		verbose: *verbose, json: *asJSON, plan: *plan}
	parsed.defaultPaths()
	return parsed, nil
}

// defaultPaths fills in the project dir and config path when they weren't
// given.
func (parsed *runArgs) defaultPaths() {
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
}

func cmdRun(args []string) error {
	parsed, err := parseRunArgs(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError(err)
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	target, err := resolveRunTarget(st, parsed)
	if err != nil {
		return err
	}

	// Ctrl-C / SIGTERM cancels the run: in-flight steps are killed rather
	// than left running after j-cicd is gone, and the run is recorded as
	// cancelled. SIGTERM is also how the dashboard cancels a run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := manager.Options{Push: parsed.push, Deploy: parsed.deploy, DeployDirty: parsed.deployDirty}
	if parsed.plan {
		return cmdPlan(ctx, st, target, opts, parsed.json)
	}
	opts.Echo, opts.Progress = outputMode(parsed.quiet, parsed.verbose, parsed.json)
	run, err := manager.Run(ctx, st, target, opts)
	return reportRun(run, err, "pipeline failed", parsed.json)
}

// outputMode picks what a foreground run shows on stderr. At a terminal
// that's everything, as it happens. Anywhere else (a pipe, an agent, a
// log file) it's the stage lines plus the end of whatever failed, since
// thousands of docker layer lines bury the one that matters; -v brings
// the full stream back and --quiet drops all of it. Either way the whole
// output is recorded (j-cicd logs). --json is a script asking, so it gets
// the concise form even at a terminal.
func outputMode(quiet, verbose, asJSON bool) (echo, progress io.Writer) {
	switch {
	case quiet:
		return nil, nil
	case verbose || (!asJSON && isTerminal(os.Stderr)):
		return os.Stderr, nil
	default:
		return nil, os.Stderr
	}
}

// cmdPlan is `run --plan`: report what the run would do, touching nothing.
func cmdPlan(ctx context.Context, st *store.Store, target manager.Target, opts manager.Options, asJSON bool) error {
	plan, err := manager.MakePlan(ctx, st, target, opts)
	if err != nil {
		if configErr := (*manager.ConfigError)(nil); errors.As(err, &configErr) {
			err = &exitError{code: exitConfig, err: err}
		}
		return err
	}
	if asJSON {
		if err := printJSON(plan); err != nil {
			return err
		}
	} else {
		fmt.Print(plan.Describe())
	}
	if len(plan.Problems) > 0 {
		return fmt.Errorf("this run would fail: %s", strings.Join(plan.Problems, "; "))
	}
	return nil
}

func cmdRollback(args []string) error {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to pipeline config (default: <project-dir>/"+config.DefaultFile+")")
	quiet := fs.Bool("quiet", false, "print nothing but the final status (output is still recorded; see j-cicd logs)")
	verbose := fs.Bool("verbose", false, "stream every command's output (the default at a terminal)")
	fs.BoolVar(verbose, "v", false, "same as --verbose")
	asJSON := fs.Bool("json", false, "print the result as one JSON object on stdout")
	dir, err := parseCommandFlags(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError(err)
	}
	parsed := runArgs{projectDir: dir, configPath: *configPath}
	parsed.defaultPaths()

	st, err := openStore()
	if err != nil {
		return err
	}
	target, err := resolveRunTarget(st, parsed)
	if err != nil {
		return err
	}

	// Ctrl-C cancels it like a run; the agent then puts back what was
	// running before the rollback started.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := manager.Options{}
	opts.Echo, opts.Progress = outputMode(*quiet, *verbose, *asJSON)
	run, err := manager.Rollback(ctx, st, target, opts)
	return reportRun(run, err, "rollback failed", *asJSON)
}

// reportRun says how a recorded run ended and maps its error to an exit
// code. The prose goes to stderr; with asJSON the result is also printed
// to stdout as JSON, even when the run failed, so a script always has one
// object to parse (and the exit status to go with it).
func reportRun(run store.Run, err error, failed string, asJSON bool) error {
	if run.ID != "" && run.Status.Done() {
		line := fmt.Sprintf("run %s %s after %s", run.ID, run.Status, display.Duration(run.Duration()))
		if run.Revision != "" {
			line += ", commit " + run.Revision
		}
		log.Printf("%s (output: j-cicd logs %s)", line, run.ID)
		for _, img := range run.Images {
			ref := img.Repository + ":" + strings.Join(img.Tags, ",")
			if img.Digest != "" {
				ref = img.Repository + "@" + img.Digest
			}
			log.Printf("  pushed %s: %s", img.Name, ref)
		}
		for _, a := range run.Artifacts {
			log.Printf("  built %s: %s (sha256:%s)", a.Name, a.Path, a.SHA256[:min(12, len(a.SHA256))])
		}
		if run.Deployed {
			log.Printf("  deployed to %s", run.DeployHost)
		}
		for _, v := range run.Verified {
			log.Printf("  verified %s (%d attempt(s), %.1fs)", v.URL, v.Attempts, v.Seconds)
		}
	}

	var result error
	if err != nil {
		switch configErr := (*manager.ConfigError)(nil); {
		case errors.As(err, &configErr):
			result = &exitError{code: exitConfig, err: err}
		case errors.Is(err, manager.ErrBusy):
			result = err
		default:
			result = fmt.Errorf("%s: %w", failed, err)
		}
	}

	if asJSON {
		var out any
		if run.ID != "" {
			out = toJSONRun(run)
		} else {
			// Nothing was recorded (config didn't load, project busy):
			// still one object, saying why.
			out = map[string]string{"status": "error", "error": result.Error()}
		}
		if jsonErr := printJSON(out); jsonErr != nil && result == nil {
			result = jsonErr
		}
	}
	return result
}

// resolveRunTarget decides what `j-cicd run <x>` means. A bare word that
// names a registered project runs that project; anything that looks like a
// path (or names no project) is a project directory, as before. A
// directory that is itself registered is recorded under its registered
// name, so `cd proj && j-cicd run` and `j-cicd run proj` share a history.
func resolveRunTarget(st *store.Store, parsed runArgs) (manager.Target, error) {
	arg := parsed.projectDir
	explicitConfig := parsed.configPath != filepath.Join(parsed.projectDir, config.DefaultFile)

	if looksLikeName(arg) {
		p, err := st.Project(arg)
		switch {
		case err == nil && explicitConfig:
			return manager.Target{}, usageError(fmt.Errorf("--config can't be used with a registered project (%q); it already has one", arg))
		case err == nil:
			return manager.TargetFor(p), nil
		case !errors.Is(err, store.ErrNotFound):
			return manager.Target{}, err
		}
		if _, statErr := os.Stat(arg); statErr != nil {
			return manager.Target{}, &exitError{code: exitConfig, err: fmt.Errorf("no registered project or directory named %q (see j-cicd list)", arg)}
		}
	}

	// A directory that isn't a pipeline at all is a typo, not a run worth
	// recording in history.
	if _, err := os.Stat(parsed.configPath); err != nil {
		return manager.Target{}, &exitError{code: exitConfig, err: fmt.Errorf("load config: %w", err)}
	}
	if !explicitConfig {
		if p, ok, err := st.ProjectByDir(arg); err != nil {
			return manager.Target{}, err
		} else if ok {
			return manager.TargetFor(p), nil
		}
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return manager.Target{}, err
	}
	return manager.Target{Name: nameFromDir(abs), Dir: arg, ConfigPath: parsed.configPath}, nil
}

// looksLikeName reports whether a run argument could be a project name
// rather than a path.
func looksLikeName(arg string) bool {
	return arg != "" && !strings.ContainsAny(arg, `/\`) && store.ValidName(arg) == nil
}

// nameFromDir derives a project name from a directory, for runs of
// unregistered directories and as the default for `j-cicd add`.
func nameFromDir(dir string) string {
	name := strings.Map(func(r rune) rune {
		if r == '.' || r == '_' || r == '-' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') {
			return r
		}
		return '-'
	}, filepath.Base(dir))
	name = strings.TrimLeft(name, ".-")
	if name == "" {
		return "project"
	}
	return name
}

func openStore() (*store.Store, error) {
	root, err := store.DefaultRoot()
	if err != nil {
		return nil, err
	}
	return store.Open(root)
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
