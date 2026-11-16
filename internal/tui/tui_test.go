package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"j-cicd/internal/store"
)

type launch struct {
	project string
	push    bool
}

func newTestModel(t *testing.T, projects ...string) (*model, *[]launch) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range projects {
		if _, err := st.AddProject(store.Project{Name: name, Dir: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
	}
	var launches []launch
	m := newModel(Options{Store: st, Version: "test", Launch: func(p string, push bool) error {
		launches = append(launches, launch{p, push})
		return nil
	}})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m, &launches
}

func press(m *model, keys ...string) {
	for _, k := range keys {
		m.handleKey(k)
	}
}

func TestBuildOnlyLaunchesImmediately(t *testing.T) {
	m, launches := newTestModel(t, "alpha", "beta")
	press(m, "down", "b")
	if len(*launches) != 1 || (*launches)[0] != (launch{"beta", false}) {
		t.Errorf("launches = %+v, want one no-push run of beta", *launches)
	}
}

func TestPushNeedsConfirmation(t *testing.T) {
	m, launches := newTestModel(t, "alpha")
	press(m, "r")
	if len(*launches) != 0 {
		t.Fatal("pushed without confirmation")
	}
	if !strings.Contains(m.View(), "PUSH alpha?") {
		t.Errorf("no confirmation prompt shown:\n%s", m.View())
	}
	press(m, "n")
	if len(*launches) != 0 {
		t.Fatal("pushed after declining")
	}
	press(m, "r", "y")
	if len(*launches) != 1 || (*launches)[0] != (launch{"alpha", true}) {
		t.Errorf("launches = %+v, want one push run of alpha", *launches)
	}
}

func TestWontLaunchWhileRunning(t *testing.T) {
	m, launches := newTestModel(t, "alpha")
	h, err := m.st.StartRun(store.Run{Project: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Finish(store.StatusSuccess, "")
	m.reload()
	press(m, "b")
	if len(*launches) != 0 {
		t.Errorf("launched a second run of a running project")
	}
}

func TestEmptyStateExplainsHowToAdd(t *testing.T) {
	m, _ := newTestModel(t)
	if v := m.View(); !strings.Contains(v, "j-cicd add") {
		t.Errorf("empty dashboard doesn't say how to add a project:\n%s", v)
	}
}

func TestOpenLatestLogAndFollow(t *testing.T) {
	m, _ := newTestModel(t, "alpha")
	h, err := m.st.StartRun(store.Run{Project: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		h.Log.WriteString(strings.Repeat("x", i%7) + "line\n")
	}
	h.Log.WriteString("the last line\n")
	h.Finish(store.StatusFailed, "boom")
	m.reload()

	press(m, "o")
	if m.log == nil {
		t.Fatal("o didn't open the log")
	}
	if v := m.View(); !strings.Contains(v, "the last line") {
		t.Errorf("following log doesn't show its end:\n%s", v)
	}
	press(m, "g")
	if v := m.View(); strings.Contains(v, "the last line") || m.log.follow {
		t.Errorf("scrolling to the top should leave the end and stop following")
	}
	press(m, "G")
	if !m.log.follow {
		t.Error("G should resume following")
	}
	press(m, "esc")
	if m.log != nil {
		t.Error("esc didn't close the log")
	}
}

func TestCleanLine(t *testing.T) {
	cases := map[string]string{
		"plain":                      "plain",
		"10%\r50%\r100%":             "100%",
		"done\r":                     "done",
		"a\tb":                       "a    b",
		"\x1b[31mred\x1b[0m text":    "red text",
		"bell\x07 and \x00null":      "bell and null",
		"\x1b[2K\rprogress complete": "progress complete",
	}
	for in, want := range cases {
		if got := cleanLine(in); got != want {
			t.Errorf("cleanLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadLogTailTrimsToLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	var b strings.Builder
	for range 1000 {
		b.WriteString("0123456789\n")
	}
	b.WriteString("end\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := readLogTail(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lines[0], "trimmed") || lines[len(lines)-1] != "end" {
		t.Errorf("lines = %q", lines)
	}
	for _, l := range lines[1 : len(lines)-1] {
		if l != "0123456789" {
			t.Errorf("partial line kept after trimming: %q", l)
		}
	}
}

func TestWrap(t *testing.T) {
	got := wrap("Docker isn't usable: permission denied. Fix: reboot", 20, 4)
	want := []string{"Docker isn't usable:", "permission denied.", "Fix: reboot"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("wrap = %q, want %q", got, want)
	}
	if got := wrap("a b c d e f", 1, 2); len(got) != 2 {
		t.Errorf("wrap kept %d lines, want 2: %q", len(got), got)
	}
	if got := wrap("", 10, 3); got != nil {
		t.Errorf("wrap of empty = %q", got)
	}
}
