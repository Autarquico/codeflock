package tui

import (
	"context"
	"io"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Autarquico/codeflock/internal/cli"
)

type (
	listMsg struct {
		list cli.JobList
		err  error
	}
	screenMsg struct {
		id   string
		text string
		err  error
	}
	// doneMsg is an order (send, stop, resume, attach) finished.
	doneMsg struct {
		verb string
		err  error
	}
	tickMsg struct{}
)

// Model is the TUI: the fleet list, the selection, and the detail of the
// selected Job.
type Model struct {
	ctx      context.Context
	jobs     Jobs
	interval time.Duration
	theme    Theme
	width    int
	height   int

	list      cli.JobList
	loaded    bool
	listErr   error
	cursor    int
	screen    string
	screenID  string // the Job the screen belongs to
	screenErr error

	typing bool
	input  []rune
	status string
}

// New builds the model. ctx bounds every call it makes.
func New(ctx context.Context, jobs Jobs, opts Options) *Model {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	return &Model{ctx: ctx, jobs: jobs, interval: opts.Interval, theme: NewTheme(true), width: 80, height: 24}
}

// Init asks the terminal for its background and reads the fleet.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, m.fetchList(), m.tick())
}

func (m *Model) tick() tea.Cmd {
	return tea.Tick(m.interval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *Model) fetchList() tea.Cmd {
	return func() tea.Msg {
		list, err := m.jobs.List(m.ctx, "")
		return listMsg{list, err}
	}
}

func (m *Model) fetchScreen() tea.Cmd {
	j, ok := m.selected()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		text, err := m.jobs.Screen(m.ctx, j.ID)
		return screenMsg{j.ID, text, err}
	}
}

func (m *Model) selected() (cli.JobView, bool) {
	if m.cursor < 0 || m.cursor >= len(m.list.Jobs) {
		return cli.JobView{}, false
	}
	return m.list.Jobs[m.cursor], true
}

// order runs an action on the selected Job and then reads the fleet again.
func (m *Model) order(verb string, do func(id string) error) tea.Cmd {
	j, ok := m.selected()
	if !ok {
		return nil
	}
	m.status = verb + " " + j.ID + "…"
	return func() tea.Msg { return doneMsg{verb, do(j.ID)} }
}

// Update handles one message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		m.theme = NewTheme(msg.IsDark())
	case tickMsg:
		return m, tea.Batch(m.fetchList(), m.fetchScreen(), m.tick())
	case listMsg:
		return m, m.applyList(msg)
	case screenMsg:
		if j, ok := m.selected(); ok && j.ID == msg.id {
			m.screenID, m.screenErr = msg.id, msg.err
			if msg.err == nil {
				m.screen = msg.text
			}
		}
	case doneMsg:
		if msg.err != nil {
			m.status = msg.verb + " failed: " + msg.err.Error()
		} else {
			m.status = msg.verb + " done"
		}
		return m, tea.Batch(m.fetchList(), m.fetchScreen())
	case tea.KeyPressMsg:
		return m.key(msg)
	}
	return m, nil
}

// applyList keeps the selection on the same Job when the list changes under it.
func (m *Model) applyList(msg listMsg) tea.Cmd {
	m.loaded, m.listErr = true, msg.err
	if msg.err != nil {
		return nil
	}
	var keep string
	if j, ok := m.selected(); ok {
		keep = j.ID
	}
	m.list = msg.list
	m.cursor = 0
	for i, j := range m.list.Jobs {
		if j.ID == keep {
			m.cursor = i
		}
	}
	if j, ok := m.selected(); !ok || j.ID != m.screenID {
		m.screen, m.screenID, m.screenErr = "", "", nil
		return m.fetchScreen()
	}
	return nil
}

func (m *Model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.typing {
		return m, m.typingKey(msg)
	}
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "j", "down":
		return m, m.move(1)
	case "k", "up":
		return m, m.move(-1)
	case "r":
		m.status = ""
		return m, tea.Batch(m.fetchList(), m.fetchScreen())
	case "s":
		if _, ok := m.selected(); ok {
			m.typing, m.input, m.status = true, nil, ""
		}
	case "x":
		return m, m.order("stop", func(id string) error { _, err := m.jobs.Stop(m.ctx, id); return err })
	case "u":
		return m, m.order("resume", func(id string) error { _, err := m.jobs.Resume(m.ctx, id); return err })
	case "a":
		return m, m.attach()
	}
	return m, nil
}

func (m *Model) move(delta int) tea.Cmd {
	if n := len(m.list.Jobs); n > 0 {
		m.cursor = max(0, min(n-1, m.cursor+delta))
	}
	m.screen, m.screenID, m.screenErr = "", "", nil
	return m.fetchScreen()
}

func (m *Model) typingKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.typing = false
	case "enter":
		text := string(m.input)
		m.typing = false
		if text == "" {
			return nil
		}
		return m.order("send", func(id string) error { _, err := m.jobs.Send(m.ctx, id, text); return err })
	case "backspace":
		if n := len(m.input); n > 0 {
			m.input = m.input[:n-1]
		}
	default:
		m.input = append(m.input, []rune(msg.Text)...)
	}
	return nil
}

// attach hands the terminal to the Job's session and takes it back on leaving.
func (m *Model) attach() tea.Cmd {
	j, ok := m.selected()
	if !ok {
		return nil
	}
	m.status = ""
	return tea.Exec(attachCmd{func() error { return m.jobs.Attach(m.ctx, j.ID, false) }},
		func(err error) tea.Msg { return doneMsg{"attach", err} })
}

// attachCmd is a tea.ExecCommand over a function: Attach drives the terminal
// itself, so the streams bubbletea offers are not needed.
type attachCmd struct{ run func() error }

func (c attachCmd) Run() error        { return c.run() }
func (attachCmd) SetStdin(io.Reader)  {}
func (attachCmd) SetStdout(io.Writer) {}
func (attachCmd) SetStderr(io.Writer) {}
