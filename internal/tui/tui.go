// Package tui is j-cicd's interactive dashboard: every registered project
// with its latest result, the run history of the selected one, and each
// run's log, followed live while it's running.
//
// The dashboard never executes a pipeline itself. Starting a run launches
// a separate background j-cicd process (see Options.Launch) that records
// into the store, and the dashboard simply re-reads the store. So closing
// the dashboard never kills a build, a run started from the CLI shows up
// here too, and there's exactly one code path that runs pipelines.
package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"j-cicd/internal/display"
	"j-cicd/internal/manager"
	"j-cicd/internal/store"
)

// Options configure the dashboard.
type Options struct {
	Store *store.Store
	// Launch starts a background run of project. It returns once the run
	// is launched, not when it finishes.
	Launch  func(project string, push bool) error
	Version string
}

// Run shows the dashboard until the user quits.
func Run(opts Options) error {
	if opts.Store == nil || opts.Launch == nil {
		return errors.New("tui: Store and Launch are required")
	}
	_, err := tea.NewProgram(newModel(opts), tea.WithAltScreen()).Run()
	return err
}

// refreshInterval is how often the store is re-read. It's what makes a
// running build's status and log move on screen.
const refreshInterval = 700 * time.Millisecond

// maxLogBytes caps how much of a log is loaded: the tail is what matters,
// and a verbose docker build can run to many megabytes.
const maxLogBytes = 2 << 20

type pane int

const (
	paneProjects pane = iota
	paneRuns
)

type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// confirmation is an action waiting on a y/n, for the things that are
// hard to take back: publishing images, killing a build.
type confirmation struct {
	prompt string
	action func() tea.Cmd
}

type model struct {
	opts Options
	st   *store.Store

	projects []store.Project
	last     map[string]store.Run // latest run per project
	runs     []store.Run          // history of the selected project

	projCursor, runCursor int
	focus                 pane

	log     *logView
	confirm *confirmation
	// launchedAt is when the dashboard last started a run: until that run
	// shows up in history, the selection jumps to it as soon as it does.
	launchedAt time.Time

	flash    string
	flashErr bool
	loadErr  error

	width, height int
	now           time.Time
}

func newModel(opts Options) *model {
	m := &model{opts: opts, st: opts.Store, now: time.Now()}
	m.reload()
	return m
}

func (m *model) Init() tea.Cmd { return tick() }

func (m *model) selectedProject() (store.Project, bool) {
	if m.projCursor < 0 || m.projCursor >= len(m.projects) {
		return store.Project{}, false
	}
	return m.projects[m.projCursor], true
}

func (m *model) selectedRun() (store.Run, bool) {
	if m.runCursor < 0 || m.runCursor >= len(m.runs) {
		return store.Run{}, false
	}
	return m.runs[m.runCursor], true
}

// reload re-reads projects and history, keeping the selection on the same
// project and run where they still exist.
func (m *model) reload() {
	m.now = time.Now()
	prevProject, _ := m.selectedProject()
	prevRun, _ := m.selectedRun()

	projects, err := m.st.Projects()
	if err != nil {
		m.loadErr = err
		return
	}
	all, err := m.st.Runs("", 0)
	if err != nil {
		m.loadErr = err
		return
	}
	m.loadErr = nil
	m.projects = projects

	m.last = make(map[string]store.Run)
	for _, r := range all {
		if _, seen := m.last[r.Project]; !seen {
			m.last[r.Project] = r
		}
	}

	m.projCursor = clamp(m.projCursor, len(m.projects))
	for i, p := range m.projects {
		if p.Name == prevProject.Name {
			m.projCursor = i
		}
	}

	m.runs = m.runs[:0]
	if p, ok := m.selectedProject(); ok {
		for _, r := range all {
			if r.Project == p.Name {
				m.runs = append(m.runs, r)
			}
		}
	}
	if p, _ := m.selectedProject(); p.Name != prevProject.Name {
		m.runCursor = 0
	} else {
		m.runCursor = clamp(m.runCursor, len(m.runs))
		for i, r := range m.runs {
			if r.ID == prevRun.ID {
				m.runCursor = i
			}
		}
	}
	if !m.launchedAt.IsZero() && len(m.runs) > 0 && !m.runs[0].StartedAt.Before(m.launchedAt) {
		m.runCursor, m.launchedAt = 0, time.Time{}
	}

	if m.log != nil {
		m.log.reload(m.st)
	}
}

func clamp(i, n int) int {
	if i >= n {
		i = n - 1
	}
	return max(i, 0)
}

func (m *model) setFlash(msg string, isErr bool) {
	m.flash, m.flashErr = msg, isErr
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		m.reload()
		return m, tick()
	case tea.KeyMsg:
		return m, m.handleKey(msg.String())
	}
	return m, nil
}

func (m *model) handleKey(key string) tea.Cmd {
	if key == "ctrl+c" {
		return tea.Quit
	}
	if m.confirm != nil {
		c := m.confirm
		m.confirm = nil
		if key == "y" || key == "Y" {
			return c.action()
		}
		m.setFlash("cancelled", false)
		return nil
	}
	if m.log != nil {
		return m.handleLogKey(key)
	}

	m.flash = ""
	switch key {
	case "q":
		return tea.Quit
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "home", "g":
		m.move(-1 << 30)
	case "end", "G":
		m.move(1 << 30)
	case "tab", "shift+tab":
		m.focus = 1 - m.focus
	case "left", "h", "esc":
		m.focus = paneProjects
	case "right", "l":
		if len(m.runs) > 0 {
			m.focus = paneRuns
		}
	case "enter":
		if m.focus == paneProjects {
			if len(m.runs) > 0 {
				m.focus = paneRuns
			}
			return nil
		}
		if r, ok := m.selectedRun(); ok {
			m.openLog(r)
		}
	case "o":
		// The latest log of the selected project, from either pane.
		if len(m.runs) > 0 {
			m.openLog(m.runs[0])
		}
	case "r":
		m.startRun(true)
	case "b":
		m.startRun(false)
	case "x":
		if p, ok := m.selectedProject(); ok {
			if r, ok := m.last[p.Name]; ok && !r.Status.Done() {
				m.askCancel(r)
			} else {
				m.setFlash(p.Name+" has no run in progress", true)
			}
		}
	}
	return nil
}

func (m *model) move(delta int) {
	if m.focus == paneProjects {
		before := m.projCursor
		m.projCursor = clamp(m.projCursor+delta, len(m.projects))
		if m.projCursor != before {
			m.launchedAt = time.Time{}
			m.reload()
		}
		return
	}
	m.runCursor = clamp(m.runCursor+delta, len(m.runs))
}

func (m *model) startRun(push bool) {
	p, ok := m.selectedProject()
	if !ok {
		return
	}
	if r, ok := m.last[p.Name]; ok && !r.Status.Done() {
		m.setFlash(fmt.Sprintf("%s is already running (%s)", p.Name, r.ID), true)
		return
	}
	launch := func() tea.Cmd {
		// Run IDs have one-second resolution; compare at that grain.
		m.launchedAt = time.Now().Truncate(time.Second)
		if err := m.opts.Launch(p.Name, push); err != nil {
			m.launchedAt = time.Time{}
			m.setFlash("launch failed: "+err.Error(), true)
			return nil
		}
		what := "build+push"
		if !push {
			what = "build (no push)"
		}
		m.setFlash(fmt.Sprintf("started %s of %s", what, p.Name), false)
		m.focus = paneRuns
		return nil
	}
	if !push {
		launch()
		return
	}
	// Pushing publishes images a VPS may deploy, so it takes a second key.
	m.confirm = &confirmation{
		prompt: fmt.Sprintf("Build and PUSH %s? [y/N]", p.Name),
		action: launch,
	}
}

func (m *model) askCancel(r store.Run) {
	m.confirm = &confirmation{
		prompt: fmt.Sprintf("Cancel run %s of %s? [y/N]", r.ID, r.Project),
		action: func() tea.Cmd {
			if err := manager.Cancel(r); err != nil {
				m.setFlash("cancel failed: "+err.Error(), true)
			} else {
				m.setFlash("cancelling "+r.ID, false)
			}
			return nil
		},
	}
}

func (m *model) openLog(r store.Run) {
	m.log = &logView{run: r, follow: true}
	m.log.reload(m.st)
}

func (m *model) handleLogKey(key string) tea.Cmd {
	lv := m.log
	page := max(1, m.logHeight()-1)
	switch key {
	case "q", "esc", "backspace", "left", "h":
		m.log = nil
	case "up", "k":
		lv.scroll(-1, m.logHeight())
	case "down", "j":
		lv.scroll(1, m.logHeight())
	case "pgup", "ctrl+u", "b":
		lv.scroll(-page, m.logHeight())
	case "pgdown", "ctrl+d", " ", "f":
		lv.scroll(page, m.logHeight())
	case "home", "g":
		lv.scroll(-1<<30, m.logHeight())
	case "end", "G":
		lv.follow = true
	case "F":
		lv.follow = !lv.follow
	case "x":
		if !lv.run.Status.Done() {
			m.askCancel(lv.run)
		}
	}
	return nil
}

// ---- view ----

var (
	styleTitle    = lipgloss.NewStyle().Bold(true)
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleSelected = lipgloss.NewStyle().Reverse(true)
	styleErr      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleOK       = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn     = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	styleBorder   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8"))
	styleFocused  = styleBorder.BorderForeground(lipgloss.Color("6"))
)

func statusStyle(s store.Status) lipgloss.Style {
	switch s {
	case store.StatusSuccess:
		return styleOK
	case store.StatusFailed:
		return styleErr
	case store.StatusRunning:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	case store.StatusInterrupted:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	}
	return styleDim
}

func (m *model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	var body string
	if m.log != nil {
		body = m.viewLog()
	} else {
		body = m.viewDashboard()
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.viewHeader(), body, m.viewFooter())
}

func (m *model) viewHeader() string {
	running := 0
	for _, r := range m.last {
		if !r.Status.Done() {
			running++
		}
	}
	right := fmt.Sprintf("%d projects", len(m.projects))
	if running > 0 {
		right += styleWarn.Render(fmt.Sprintf("  ● %d running", running))
	}
	left := styleTitle.Render("j-cicd") + styleDim.Render(" "+m.opts.Version)
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func (m *model) viewFooter() string {
	var line string
	switch {
	case m.confirm != nil:
		line = styleWarn.Render(m.confirm.prompt)
	case m.loadErr != nil:
		line = styleErr.Render("reading state: " + m.loadErr.Error())
	case m.flash != "" && m.flashErr:
		line = styleErr.Render(m.flash)
	case m.flash != "":
		line = styleOK.Render(m.flash)
	}
	var keys string
	if m.log != nil {
		keys = "↑↓/pgup/pgdn scroll · g/G top/bottom · F follow · x cancel run · esc back"
	} else {
		keys = "↑↓ select · tab pane · enter open · o latest log · r run+push · b build only · x cancel · q quit"
	}
	return truncate(line, m.width) + "\n" + styleDim.Render(truncate(keys, m.width))
}

// bodyHeight is the rows between header and footer, borders included.
func (m *model) bodyHeight() int { return max(3, m.height-3) }

func (m *model) viewDashboard() string {
	h := m.bodyHeight()
	inner := h - 2
	leftW := min(max(24, m.width/3), 40)
	rightW := max(10, m.width-leftW-4)

	left := m.viewProjects(leftW, inner)
	right := m.viewRuns(rightW, inner)

	ls, rs := styleBorder, styleBorder
	if m.focus == paneProjects {
		ls = styleFocused
	} else {
		rs = styleFocused
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		ls.Width(leftW).Height(inner).Render(left),
		rs.Width(rightW).Height(inner).Render(right),
	)
}

func (m *model) viewProjects(w, h int) string {
	lines := []string{styleTitle.Render("Projects")}
	if len(m.projects) == 0 {
		lines = append(lines, "", styleDim.Render(truncate("No projects yet.", w)),
			styleDim.Render(truncate("Add one with:", w)),
			truncate("  j-cicd add <dir>", w))
		return strings.Join(lines, "\n")
	}
	rows := h - 1
	start := scrollStart(m.projCursor, len(m.projects), rows)
	for i := start; i < len(m.projects) && i < start+rows; i++ {
		p := m.projects[i]
		sym, when := styleDim.Render("–"), "never run"
		if r, ok := m.last[p.Name]; ok {
			sym = statusStyle(r.Status).Render(display.Symbol(r.Status))
			when = display.Ago(r.StartedAt, m.now)
			if !r.Status.Done() {
				when = "running " + display.Duration(r.Duration())
			}
		}
		text := padBetween(p.Name, when, w-2)
		if i == m.projCursor {
			text = styleSelected.Render(text)
		}
		lines = append(lines, sym+" "+text)
	}
	return strings.Join(lines, "\n")
}

func (m *model) viewRuns(w, h int) string {
	p, ok := m.selectedProject()
	if !ok {
		return ""
	}
	lines := []string{styleTitle.Render(truncate(p.Name, w)) + styleDim.Render(truncate("  "+p.Dir, w-lipgloss.Width(p.Name)))}
	if len(m.runs) == 0 {
		lines = append(lines, "", styleDim.Render("No runs yet — press r to run+push, b to build only."))
		return strings.Join(lines, "\n")
	}

	// The selected run's error gets the bottom rows, since it's the first
	// thing to want after "it failed".
	detail := m.runDetail(w)
	rows := max(1, h-1-len(detail))
	start := scrollStart(m.runCursor, len(m.runs), rows)
	for i := start; i < len(m.runs) && i < start+rows; i++ {
		r := m.runs[i]
		text := fmt.Sprintf("%-22s %-9s %7s  %-14s %s",
			r.ID, display.Ago(r.StartedAt, m.now), display.Duration(r.Duration()),
			display.Revision(r), display.Mode(r))
		text = truncate(text, w-2)
		if i == m.runCursor && m.focus == paneRuns {
			text = styleSelected.Render(text)
		}
		lines = append(lines, statusStyle(r.Status).Render(display.Symbol(r.Status))+" "+text)
	}
	for len(lines) < h-len(detail) {
		lines = append(lines, "")
	}
	return strings.Join(append(lines, detail...), "\n")
}

func (m *model) runDetail(w int) []string {
	// Outside the runs pane, describe the latest run.
	r, ok := m.selectedRun()
	if m.focus != paneRuns && len(m.runs) > 0 {
		r, ok = m.runs[0], true
	}
	if !ok {
		return nil
	}
	head := statusStyle(r.Status).Render(fmt.Sprintf("%s %s", display.Symbol(r.Status), r.Status)) +
		styleDim.Render(truncate(fmt.Sprintf("  %s · %s", r.ID, r.StartedAt.Local().Format("Mon Jan 2 15:04:05")), w))
	out := []string{"", head}
	// Errors say what to do about them (e.g. Docker's "Fix: ..."), so
	// they get a few lines rather than being cut at the pane's edge.
	for _, line := range wrap(display.FirstLine(r.Error), w, maxErrorLines) {
		out = append(out, styleErr.Render(line))
	}
	return out
}

const maxErrorLines = 4

// wrap breaks s into lines of at most w columns at spaces, keeping at
// most n lines and marking anything cut with "…".
func wrap(s string, w, n int) []string {
	if s == "" || w <= 0 || n <= 0 {
		return nil
	}
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case lipgloss.Width(line)+1+lipgloss.Width(word) <= w:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	lines = append(lines, line)
	if len(lines) > n {
		lines = lines[:n]
		lines[n-1] += " …"
	}
	for i := range lines {
		lines[i] = truncate(lines[i], w)
	}
	return lines
}

// scrollStart returns the first visible row so cursor stays on screen.
func scrollStart(cursor, n, rows int) int {
	if rows <= 0 || n <= rows {
		return 0
	}
	return min(max(0, cursor-rows/2), n-rows)
}

func (m *model) logHeight() int { return max(1, m.bodyHeight()-3) }

func (m *model) viewLog() string {
	lv := m.log
	w := max(10, m.width-4)
	h := m.logHeight()
	lv.clampTo(h)

	r := lv.run
	title := statusStyle(r.Status).Render(display.Symbol(r.Status)+" "+string(r.Status)) + " " +
		styleTitle.Render(r.Project) + styleDim.Render(fmt.Sprintf("  %s · %s · %s", r.ID, display.Duration(r.Duration()), display.Mode(r)))
	if lv.follow {
		title += styleDim.Render(" · following")
	}

	var lines []string
	switch {
	case lv.err != nil:
		lines = append(lines, styleErr.Render(lv.err.Error()))
	case len(lv.lines) == 0:
		lines = append(lines, styleDim.Render("(no output yet)"))
	default:
		end := min(lv.offset+h, len(lv.lines))
		for _, l := range lv.lines[lv.offset:end] {
			lines = append(lines, truncate(l, w))
		}
	}
	return styleFocused.Width(w).Height(h + 1).Render(truncate(title, w) + "\n" + strings.Join(lines, "\n"))
}

// truncate cuts plain text to w columns.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r)) > w-1 {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// padBetween lays out left and right in w columns with space between,
// dropping right if there isn't room for both.
func padBetween(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return truncate(left, w)
	}
	return left + strings.Repeat(" ", gap) + right
}
