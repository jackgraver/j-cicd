package main

import (
	"fmt"

	"j-cicd/internal/cli"
	"j-cicd/internal/guide"
)

// cmdGuide prints the embedded guide for agents operating j-cicd. It goes to
// stdout because it's data a caller reads, and needs no terminal.
func cmdGuide(args []string) error {
	if len(args) > 0 {
		return usageError(fmt.Errorf("guide takes no arguments, got %q", args[0]))
	}
	fmt.Printf("<!-- j-cicd %s -->\n%s", cli.Version(version), guide.Text())
	return nil
}
