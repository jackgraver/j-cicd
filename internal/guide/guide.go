// Package guide holds the operating guide for AI agents that use j-cicd.
// It is embedded in the binary so it always matches the installed version.
package guide

import (
	_ "embed"
	"strings"
)

//go:embed agent.md
var text string

// Text returns the guide.
func Text() string { return text }

// shortEnd is the heading that ends the summary at the top of the guide.
const shortEnd = "\n## Rules"

// Short returns just the summary at the top of the guide, which covers most
// sessions and points to the rest by topic.
func Short() string {
	if i := strings.Index(text, shortEnd); i >= 0 {
		return text[:i+1]
	}
	return text
}
