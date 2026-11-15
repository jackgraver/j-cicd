package tui

import (
	"io"
	"os"
	"strings"

	"j-cicd/internal/store"
)

// logView is one run's log, re-read on every refresh while it grows.
type logView struct {
	run    store.Run
	lines  []string
	offset int
	// follow pins the view to the end of the log, like `tail -f`. It's on
	// by default and turns off as soon as the user scrolls up.
	follow bool
	err    error
}

func (lv *logView) reload(st *store.Store) {
	if r, err := st.GetRun(lv.run.ID); err == nil {
		lv.run = r
	}
	lines, err := readLogTail(st.LogPath(lv.run.ID), maxLogBytes)
	lv.lines, lv.err = lines, err
}

func (lv *logView) scroll(delta, height int) {
	lv.offset += delta
	lv.follow = false
	lv.clampTo(height)
	if lv.offset >= len(lv.lines)-height {
		lv.follow = true
	}
}

func (lv *logView) clampTo(height int) {
	bottom := max(0, len(lv.lines)-height)
	if lv.follow {
		lv.offset = bottom
	}
	lv.offset = min(max(lv.offset, 0), bottom)
}

// readLogTail reads up to the last limit bytes of path as display lines.
func readLogTail(path string, limit int64) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	truncated := info.Size() > limit
	if truncated {
		if _, err := f.Seek(-limit, io.SeekEnd); err != nil {
			return nil, err
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, err
	}

	text := string(data)
	if truncated {
		// Drop the partial first line the seek landed in.
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = cleanLine(l)
	}
	if truncated {
		lines = append([]string{"… (earlier output trimmed — full log: " + path + ")"}, lines...)
	}
	return lines, nil
}

// cleanLine makes one line of raw command output safe to draw: progress
// output that redraws itself with \r keeps only its final state, tabs
// become spaces, and escape sequences are dropped so a command's colours
// can't scramble the dashboard.
func cleanLine(l string) string {
	if i := strings.LastIndexByte(strings.TrimRight(l, "\r"), '\r'); i >= 0 {
		l = l[i+1:]
	}
	l = strings.ReplaceAll(l, "\t", "    ")
	var b strings.Builder
	b.Grow(len(l))
	inEscape := false
	for _, r := range l {
		switch {
		case inEscape:
			// CSI sequences end on a letter.
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				inEscape = false
			}
		case r == 0x1b:
			inEscape = true
		case r < 0x20 || r == 0x7f:
			// other control characters
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
