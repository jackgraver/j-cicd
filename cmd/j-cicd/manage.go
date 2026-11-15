package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"j-cicd/internal/config"
	"j-cicd/internal/display"
	"j-cicd/internal/manager"
	"j-cicd/internal/store"
	"j-cicd/internal/tui"
)

// parseFlagsOnly parses a subcommand's flags and returns its positional
// arguments, which must number between minArgs and maxArgs.
//
// Go's flag package stops parsing at the first non-flag argument, so a
// single Parse of the raw argv would leave every flag after a positional
// unconsumed and silently discarded — `j-cicd run . --no-push` would push.
// Parsing repeatedly, peeling off one positional each time it stops, makes
// flags work on either side of the positionals.
func parseFlagsOnly(fs *flag.FlagSet, args []string, usage string, minArgs, maxArgs int) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprintf(os.Stderr, "Usage: j-cicd %s %s\n", fs.Name(), usage)
				fs.SetOutput(os.Stderr)
				fs.PrintDefaults()
				return nil, flag.ErrHelp
			}
			return nil, usageError(fmt.Errorf("%w (run \"j-cicd %s -h\" for usage)", err, fs.Name()))
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positional, rest = append(positional, rest[0]), rest[1:]
	}
	if len(positional) < minArgs || len(positional) > maxArgs {
		return nil, usageError(fmt.Errorf("usage: j-cicd %s %s", fs.Name(), usage))
	}
	return positional, nil
}

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	name := fs.String("name", "", "name to register the project under (default: the config's name)")
	configPath := fs.String("config", "", "pipeline config, if not <project-dir>/"+config.DefaultFile)
	pos, err := parseFlagsOnly(fs, args, "[project-dir] [--name n] [--config path]", 0, 1)
	if err != nil {
		return err
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = filepath.Join(dir, config.DefaultFile)
	}

	// Registering something that can't run would only fail later, on the
	// first run; better to say so now.
	cfg, err := config.Load(cfgPath, "")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && *configPath == "" {
			return &exitError{code: exitConfig, err: fmt.Errorf("%w\n(create one with: j-cicd init %s)", err, dir)}
		}
		return &exitError{code: exitConfig, err: err}
	}

	projectName := *name
	if projectName == "" {
		projectName = nameFromDir(cfg.Name)
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	p, err := st.AddProject(store.Project{Name: projectName, Dir: dir, Config: *configPath})
	if err != nil {
		return err
	}
	log.Printf("registered %q (%s)", p.Name, p.Dir)
	log.Printf("run it from anywhere with: j-cicd run %s", p.Name)
	return nil
}

func cmdRemove(args []string) error {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	pos, err := parseFlagsOnly(fs, args, "<name>", 1, 1)
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	if err := st.RemoveProject(pos[0]); err != nil {
		return err
	}
	log.Printf("unregistered %q (its run history is kept; files on disk are untouched)", pos[0])
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	if _, err := parseFlagsOnly(fs, args, "", 0, 0); err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	projects, err := st.Projects()
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		log.Printf("no projects registered yet — add one with: j-cicd add <project-dir>")
		return nil
	}
	last, err := st.LastRuns()
	if err != nil {
		return err
	}

	now := time.Now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tLAST RUN\tSTATUS\tDIR")
	for _, p := range projects {
		when, status := "never", "-"
		if r, ok := last[p.Name]; ok {
			when = display.Ago(r.StartedAt, now)
			status = display.Symbol(r.Status) + " " + string(r.Status)
		}
		dir := p.Dir
		if p.Config != "" {
			dir += " (config: " + p.Config + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, when, status, dir)
	}
	return tw.Flush()
}

func cmdHistory(args []string) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	limit := fs.Int("n", 20, "show at most this many runs (0 for all)")
	pos, err := parseFlagsOnly(fs, args, "[name] [-n count]", 0, 1)
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	project := ""
	if len(pos) == 1 {
		project = pos[0]
	}
	runs, err := st.Runs(project, *limit)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		log.Printf("no runs recorded yet")
		return nil
	}

	now := time.Now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tPROJECT\tSTATUS\tSTARTED\tTOOK\tREVISION\tMODE\tERROR")
	for _, r := range runs {
		fmt.Fprintf(tw, "%s\t%s\t%s %s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.Project, display.Symbol(r.Status), r.Status,
			display.Ago(r.StartedAt, now), display.Duration(r.Duration()),
			display.Revision(r), display.Mode(r), display.FirstLine(r.Error))
	}
	return tw.Flush()
}

// findRun resolves a run ID, or a project name meaning its latest run.
func findRun(st *store.Store, ref string) (store.Run, error) {
	r, err := st.GetRun(ref)
	if !errors.Is(err, store.ErrNotFound) {
		return r, err
	}
	runs, err := st.Runs(ref, 1)
	if err != nil {
		return store.Run{}, err
	}
	if len(runs) == 0 {
		return store.Run{}, fmt.Errorf("no run or project with recorded runs named %q (see j-cicd history)", ref)
	}
	return runs[0], nil
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "keep printing output until the run finishes")
	pos, err := parseFlagsOnly(fs, args, "<run-id|name> [-f]", 1, 1)
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	r, err := findRun(st, pos[0])
	if err != nil {
		return err
	}
	f, err := os.Open(st.LogPath(r.ID))
	if err != nil {
		return err
	}
	defer f.Close()

	for {
		if _, err := io.Copy(os.Stdout, f); err != nil {
			return err
		}
		if !*follow || r.Status.Done() {
			break
		}
		time.Sleep(500 * time.Millisecond)
		if r, err = st.GetRun(r.ID); err != nil {
			return err
		}
	}
	// One last copy: the run may have written its final lines between the
	// previous copy and being seen as done.
	if _, err := io.Copy(os.Stdout, f); err != nil {
		return err
	}
	if *follow {
		log.Printf("run %s %s", r.ID, r.Status)
	}
	return nil
}

func cmdCancel(args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	pos, err := parseFlagsOnly(fs, args, "<run-id|name>", 1, 1)
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	r, err := findRun(st, pos[0])
	if err != nil {
		return err
	}
	if err := manager.Cancel(r); err != nil {
		return err
	}
	log.Printf("asked run %s of %s to stop", r.ID, r.Project)
	return nil
}

func cmdUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	if _, err := parseFlagsOnly(fs, args, "", 0, 0); err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the j-cicd binary to launch runs with: %w", err)
	}
	return tui.Run(tui.Options{
		Store:   st,
		Version: version,
		// Each run is its own `j-cicd run` process, detached from the
		// dashboard, so quitting the dashboard leaves builds running.
		Launch: func(project string, push bool) error {
			args := []string{"run", "--quiet"}
			if !push {
				args = append(args, "--no-push")
			}
			return manager.Launch(st, exe, append(args, project)...)
		},
	})
}

// isTerminal reports whether f is an interactive terminal.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
