package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"j-cicd/internal/cli"
)

// Exit codes. They're distinct so a CI caller can tell a config it needs
// to fix from a pipeline that genuinely failed.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
	exitConfig  = 3
)

// exitError pairs an error with the exit code it should produce. Returning
// one lets a subcommand pick its exit status without calling os.Exit from
// deep in the call stack, which would skip every deferred cleanup on the
// way out.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func usageError(err error) error { return &exitError{code: exitUsage, err: err} }

// appStart handles application bootstrap: it dispatches args into the
// right subcommand and returns the process exit code. All of j-cicd's
// os.Exit happens at this one boundary.
func appStart(args []string) int {
	// Off a terminal the output is read by a program or an agent, which
	// gets nothing from a timestamp on every line.
	if !isTerminal(os.Stderr) {
		log.SetFlags(0)
	}
	if len(args) == 0 {
		// Bare `j-cicd` at a terminal is "show me my projects"; from a
		// script it's a mistake worth a usage error, not a UI.
		if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
			args = []string{"ui"}
		} else {
			printUsage()
			return exitUsage
		}
	}

	var err error
	switch args[0] {
	case "ui":
		err = cmdUI(args[1:])
	case "run":
		err = cmdRun(args[1:])
	case "rollback":
		err = cmdRollback(args[1:])
	case "add":
		err = cmdAdd(args[1:])
	case "remove", "rm":
		err = cmdRemove(args[1:])
	case "list", "ls":
		err = cmdList(args[1:])
	case "history":
		err = cmdHistory(args[1:])
	case "logs":
		err = cmdLogs(args[1:])
	case "cancel":
		err = cmdCancel(args[1:])
	case "init":
		err = cmdInit(args[1:])
	case "guide":
		err = cmdGuide(args[1:])
	case "version":
		fmt.Println("j-cicd", cli.Version(version))
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		printUsage()
		return exitUsage
	}

	if err != nil {
		// -h on a subcommand already printed its usage.
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
