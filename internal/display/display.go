// Package display formats run history for people: the CLI's tables and
// the interactive UI both use it, so a run reads the same in either.
package display

import (
	"fmt"
	"strings"
	"time"

	"j-cicd/internal/store"
)

// Symbol is a one-character marker for a run status.
func Symbol(s store.Status) string {
	switch s {
	case store.StatusSuccess:
		return "✓"
	case store.StatusFailed:
		return "✗"
	case store.StatusRunning:
		return "●"
	case store.StatusCancelled:
		return "⊘"
	case store.StatusInterrupted:
		return "!"
	}
	return "?"
}

// Ago renders how long before now t was, coarsely ("12s ago", "3h ago").
func Ago(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Local().Format("2006-01-02")
}

// Duration renders a run's length to the second ("4s", "2m05s", "1h02m").
func Duration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// Mode describes what a run did: rolled back, or built with or without
// pushing its images.
func Mode(r store.Run) string {
	switch r.Kind {
	case store.KindRollback:
		return "rollback"
	case store.KindCheck:
		return "check"
	}
	if r.Push {
		return "push"
	}
	return "no-push"
}

// Revision renders a run's git revision, or "-" if it had none.
func Revision(r store.Run) string {
	if r.Revision == "" {
		return "-"
	}
	return r.Revision
}

// FirstLine trims s to its first line, for a one-row summary of an error.
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
