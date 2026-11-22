package manager

import (
	"regexp"
	"strings"
)

// ansiRE matches terminal control sequences: CSI (colours, cursor moves,
// erase) and OSC (window titles, hyperlinks). They cost an agent tokens and
// say nothing about why a build failed.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// cleanLine turns one line of command output into plain text: colour and
// cursor codes are removed, and a line a progress bar redrew with carriage
// returns is reduced to what it finally showed.
func cleanLine(s string) string {
	s = strings.TrimRight(s, "\r")
	if strings.Contains(s, "\r") {
		parts := strings.Split(s, "\r")
		s = ""
		for i := len(parts) - 1; i >= 0; i-- {
			if strings.TrimSpace(ansiRE.ReplaceAllString(parts[i], "")) != "" {
				s = parts[i]
				break
			}
		}
	}
	return ansiRE.ReplaceAllString(s, "")
}
