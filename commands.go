package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

const version = "0.1.0"

func printUsage() {
	fmt.Fprint(os.Stderr, `j-cicd - run a project's CI/CD pipeline from a config file it provides

Usage:
  j-cicd run [project-dir]     run the pipeline (build/test steps, then docker build+push)
  j-cicd init [project-dir]    scaffold a starter .cicd.yaml in project-dir
  j-cicd version               print the j-cicd version

Run "j-cicd <command> -h" for flags on a given command.
`)
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "", "path to pipeline config (default: <project-dir>/"+defaultConfigFile+")")
	noPush := fs.Bool("no-push", false, "build the image (if configured) but skip pushing it")
	fs.Parse(args)

	projectDir := "."
	if fs.NArg() > 0 {
		projectDir = fs.Arg(0)
	}

	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = filepath.Join(projectDir, defaultConfigFile)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	if err := RunPipeline(projectDir, cfg, RunOptions{Push: !*noPush}); err != nil {
		log.Fatalf("pipeline failed: %v", err)
	}
}

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	name := fs.String("name", "", "project name to write into the config (default: directory name)")
	force := fs.Bool("force", false, "overwrite an existing config file")
	fs.Parse(args)

	projectDir := "."
	if fs.NArg() > 0 {
		projectDir = fs.Arg(0)
	}

	if err := WriteStarterConfig(projectDir, *name, *force); err != nil {
		log.Fatalf("init: %v", err)
	}
}
