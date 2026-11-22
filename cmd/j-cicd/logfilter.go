package main

import (
	"fmt"
	"regexp"
	"strings"

	"j-cicd/internal/manager"
)

// logFilter selects and cleans what `j-cicd logs` prints. The zero value
// prints the log untouched.
type logFilter struct {
	// plain removes colour codes and collapses progress-bar redraws.
	plain bool
	// stage keeps only that stage's output (see manager.StageMarker lines).
	stage string
	// grep keeps only lines matching it, plus context lines around each.
	grep    *regexp.Regexp
	context int
	// tail keeps only the last n lines of whatever is left (0 for all).
	tail int
}

// apply returns data as f selects and cleans it.
func (f logFilter) apply(data []byte) ([]byte, error) {
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(data) == 0 {
		lines = nil
	}
	if f.plain {
		for i, l := range lines {
			lines[i] = manager.CleanLine(l)
		}
	}
	if f.stage != "" {
		var err error
		if lines, err = stageLines(lines, f.stage); err != nil {
			return nil, err
		}
	}
	if f.grep != nil {
		lines = grepLines(lines, f.grep, f.context)
	}
	if f.tail > 0 && len(lines) > f.tail {
		lines = lines[len(lines)-f.tail:]
	}
	if len(lines) == 0 {
		return nil, nil
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

// markerText is the stage text of a marker line, if lines[i] is one.
func markerText(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, manager.StageMarker)
	if !ok {
		return "", false
	}
	rest, ok = strings.CutSuffix(rest, " ==")
	return rest, ok
}

// stageLines returns the lines of the stages named want: from each matching
// marker to the next one. A marker reads "<kind> <name>", or just "<kind>"
// for a stage with no name of its own (see manager.StageMarkerText); want
// matches the whole of it ("step test", "build app", "artifacts") or the
// name alone ("test"), exactly
// and case-insensitively. Several stages can match (an image is built and
// pushed under one name); all are kept, markers included, so the result
// still says where each came from.
func stageLines(lines []string, want string) ([]string, error) {
	var out, seen []string
	keep := false
	for _, l := range lines {
		if text, ok := markerText(l); ok {
			seen = append(seen, text)
			name := text
			if _, after, ok := strings.Cut(text, " "); ok {
				name = after
			}
			keep = strings.EqualFold(text, want) || strings.EqualFold(name, want)
		}
		if keep {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		if len(seen) == 0 {
			return nil, fmt.Errorf("--stage %q: this run's log has no stage markers (it predates them)", want)
		}
		return nil, fmt.Errorf("--stage %q: no such stage in this run; it had: %s", want, strings.Join(seen, ", "))
	}
	return out, nil
}

// grepLines keeps the lines matching re and n lines around each, with "--"
// between groups that aren't adjacent (as grep -C does).
func grepLines(lines []string, re *regexp.Regexp, n int) []string {
	show := make([]bool, len(lines))
	for i, l := range lines {
		if re.MatchString(l) {
			for j := max(0, i-n); j <= min(len(lines)-1, i+n); j++ {
				show[j] = true
			}
		}
	}
	var out []string
	last := -1
	for i, l := range lines {
		if !show[i] {
			continue
		}
		if n > 0 && last >= 0 && i != last+1 {
			out = append(out, "--")
		}
		out = append(out, l)
		last = i
	}
	return out
}
