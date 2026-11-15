package main

import (
	"fmt"
	"os"
)

// appStart handles application bootstrap: it dispatches os.Args into the
// right subcommand.
func appStart() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "init":
		cmdInit(os.Args[2:])
	case "version":
		fmt.Println("j-cicd", version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}
