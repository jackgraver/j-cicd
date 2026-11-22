// Package guide holds the operating guide for AI agents that use j-cicd.
// It is embedded in the binary so it always matches the installed version.
package guide

import _ "embed"

//go:embed agent.md
var text string

// Text returns the guide.
func Text() string { return text }
