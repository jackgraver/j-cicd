// Package cli holds argument-parsing helpers shared by the j-cicd and
// j-cicd-agent binaries. It does no I/O of its own: printing usage and
// choosing exit codes stay in each binary's package main.
package cli

import (
	"flag"
	"io"
)

// ParseInterleaved parses args against fs, allowing flags on either side
// of (and between) positional arguments, and returns the positionals in
// order. flag.ErrHelp is returned as-is so the caller can print usage.
//
// Go's flag package stops parsing at the first non-flag argument, so a
// single Parse of the raw argv would leave every flag after a positional
// unconsumed and silently discarded — `j-cicd run . --no-push` would
// push. Parsing repeatedly, peeling off one positional each time it
// stops, makes flag position irrelevant.
func ParseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional, rest = append(positional, rest[0]), rest[1:]
	}
}
