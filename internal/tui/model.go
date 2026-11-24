package tui

import (
	"context"
	"io"
	"slices"
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
		seq  int
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

	// One list fetch is in flight at a time: a tick or an order that asks for
	// another while one runs only marks that a fresh one is due when it returns.
	listing   bool
	relist    bool
	screening int // screen fetches in flight
	screenSeq int // the last one asked for: older answers are dropped

	typing bool
	target string // the Job the draft is for: the cursor may move to another while it is typed
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
	if m.listing {
		m.relist = true
		return nil
	}
	m.listing = true
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
	m.screenSeq++
	m.screening++
	seq := m.screenSeq
	return func() tea.Msg {
		text, err := m.jobs.Screen(m.ctx, j.ID)
		return screenMsg{seq, j.ID, text, err}
	}
}

// selected is the Job under the cursor. While the list is in error the rows
// shown are stale, so nothing is selected and no order can reach them.
func (m *Model) selected() (cli.JobView, bool) {
	if m.listErr != nil || m.cursor < 0 || m.cursor >= len(m.list.Jobs) {
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
		var screen tea.Cmd
		if m.screening == 0 {
			screen = m.fetchScreen()
		}
		return m, tea.Batch(m.fetchList(), screen, m.tick())
	case listMsg:
		m.listing = false
		cmds := []tea.Cmd{m.applyList(msg)}
		if m.relist {
			m.relist = false
			cmds = append(cmds, m.fetchList())
		}
		return m, tea.Batch(cmds...)
	case screenMsg:
		m.screening--
		if j, ok := m.selected(); ok && j.ID == msg.id && msg.seq == m.screenSeq {
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
		if j, ok := m.selected(); ok {
			m.typing, m.target, m.input, m.status = true, j.ID, nil, ""
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
		target := m.target
		if m.listErr != nil || !slices.ContainsFunc(m.list.Jobs, func(j cli.JobView) bool { return j.ID == target }) {
			m.status = "send: " + target + " is not in the list now, nothing was sent"
			return nil
		}
		m.status = "send " + target + "…"
		return func() tea.Msg {
			_, err := m.jobs.Send(m.ctx, target, text)
			return doneMsg{"send", err}
		}
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
