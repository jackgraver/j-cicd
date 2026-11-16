package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"j-cicd/internal/cli"
)

// Exit codes, matching j-cicd's so the build host reads them the same way.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
	exitConfig  = 3
)

// exitError pairs an error with the exit code it should produce, so
// subcommands never call os.Exit themselves and deferred cleanup (the
// deploy lock) always runs.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func usageError(err error) error { return &exitError{code: exitUsage, err: err} }

// appStart dispatches args to a subcommand and returns the exit code. It
// is the only place the agent decides how the process exits.
func appStart(args []string) int {
	if len(args) == 0 {
		printUsage()
		return exitUsage
	}

	var err error
	switch args[0] {
	case "deploy":
		err = cmdDeploy(args[1:])
	case "rollback":
		err = cmdRollback(args[1:])
	case "status":
		err = cmdStatus(args[1:])
	case "version":
		fmt.Println("j-cicd-agent", cli.Version(version))
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		printUsage()
		return exitUsage
	}

	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		log.Printf("%v", err)
		var exit *exitError
		if errors.As(err, &exit) {
			return exit.code
		}
		return exitFailure
	}
	return exitOK
}
