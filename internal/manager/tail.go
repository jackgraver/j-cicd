package manager

import (
	"bytes"
	"sync"
)

// tailWriter keeps the last few lines written to it, so a failed run can
// show the end of the failing command's output without the rest.
type tailWriter struct {
	mu    sync.Mutex
	max   int
	lines []string
	part  []byte // an unfinished last line
}

func newTail(maxLines int) *tailWriter { return &tailWriter{max: maxLines} }

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	data := append(t.part, p...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		t.lines = append(t.lines, string(bytes.TrimRight(data[:i], "\r")))
		data = data[i+1:]
	}
	t.part = append([]byte(nil), data...)
	if extra := len(t.lines) - t.max; extra > 0 {
		t.lines = append([]string(nil), t.lines[extra:]...)
	}
	return len(p), nil
}

// Lines returns the retained lines, including an unfinished last one.
func (t *tailWriter) Lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := append([]string(nil), t.lines...)
	if len(t.part) > 0 {
		out = append(out, string(t.part))
		if len(out) > t.max {
			out = out[len(out)-t.max:]
		}
	}
	return out
}

// reset forgets everything written so far.
func (t *tailWriter) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines, t.part = nil, nil
}
