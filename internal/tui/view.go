package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Autarquico/codeflock/internal/cli"
)

// View draws the header, the fleet list, the detail of the selected Job and
// the footer.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	return v
}

func (m *Model) render() string {
	th := m.theme
	rule := th.Rule.Render(strings.Repeat("─", max(m.width, 1)))

	listRows := m.listRows()
	detailRows := m.detailRows()

	// header + rule + list + rule + detail + footer
	body := max(m.height-4, 2)
	listH := min(len(listRows), max(body/2, 1))
	detailH := max(body-listH, 1)

	lines := []string{m.header(), rule}
	lines = append(lines, window(listRows, listH, m.cursorRow())...)
	lines = append(lines, rule)
	lines = append(lines, tail(detailRows, detailH)...)
	lines = append(lines, m.footer())
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, m.width, "…")
	}
	return strings.Join(lines, "\n")
}

func (m *Model) header() string {
	th := m.theme
	waiting := 0
	for _, j := range m.list.Jobs {
		if j.State == "waiting" {
			waiting++
		}
	}
	h := th.Title.Render("CODE FLOCK") + th.Faint.Render(fmt.Sprintf("  %d jobs", len(m.list.Jobs)))
	if waiting > 0 {
		h += "  " + th.Warning.Render(fmt.Sprintf("%d waiting", waiting))
	}
	if n := len(m.list.Unreachable); n > 0 {
		h += "  " + th.Danger.Render(fmt.Sprintf("%d unreachable", n))
	}
	return h
}

// cursorRow is the index of the selected Job among the list rows.
func (m *Model) cursorRow() int { return m.cursor }

func (m *Model) listRows() []string {
	th := m.theme
	switch {
	case !m.loaded:
		return []string{th.Faint.Render("reading the fleet…")}
	case m.listErr != nil && len(m.list.Jobs) == 0:
		return []string{th.Danger.Render("cannot list: " + clean(m.listErr.Error()))}
	}
	jobs := m.list.Jobs
	w := func(get func(cli.JobView) string) int {
		n := 0
		for _, j := range jobs {
			n = max(n, ansi.StringWidth(clean(get(j))))
		}
		return n
	}
	wPeer := w(func(j cli.JobView) string { return j.Peer })
	wRepo := w(func(j cli.JobView) string { return j.Repo })
	wBranch := w(func(j cli.JobView) string { return j.Branch })
	wHarness := w(func(j cli.JobView) string { return j.Harness })

	var rows []string
	if m.listErr != nil {
		rows = append(rows, th.Danger.Render("cannot list (stale, read-only): "+clean(m.listErr.Error())))
	}
	for i, j := range jobs {
		marker := "  "
		if i == m.cursor && m.listErr == nil {
			marker = th.Accent.Render("▸ ")
		}
		state := clean(j.State)
		if j.Problem != "" {
			state += "?"
		}
		cells := []string{
			m.stateStyle(j.State).Render(pad(state, 9)),
			th.Text.Render(pad(clean(j.Peer), wPeer)),
			th.Text.Render(pad(dash(clean(j.Repo)), wRepo)),
			th.Faint.Render(pad(dash(clean(j.Branch)), wBranch)),
			th.Faint.Render(pad(dash(clean(j.Harness)), wHarness)),
			th.Faint.Render(clean(j.ID)),
		}
		rows = append(rows, marker+strings.Join(cells, "  "))
	}
	if len(jobs) == 0 {
		rows = append(rows, th.Faint.Render("no Jobs"))
	}
	for _, u := range m.list.Unreachable {
		rows = append(rows, "  "+th.Danger.Render("unreachable")+"  "+th.Text.Render(clean(u.Peer))+th.Faint.Render("  "+clean(u.Problem)))
	}
	return rows
}

func (m *Model) stateStyle(state string) lipgloss.Style {
	th := m.theme
	switch state {
	case "waiting":
		return th.Warning.Bold(true)
	case "running":
		return th.Success
	case "lost", "failed":
		return th.Danger
	}
	return th.Faint
}

func (m *Model) detailRows() []string {
	th := m.theme
	j, ok := m.selected()
	if !ok {
		return []string{th.Faint.Render("select a Job to see its screen")}
	}
	head := th.Title.Render(clean(j.ID)) + th.Faint.Render(fmt.Sprintf("  %s on %s · gen %d", clean(j.State), clean(j.Peer), j.Gen))
	rows := []string{head}
	if j.Problem != "" {
		rows = append(rows, th.Warning.Render("problem: "+clean(j.Problem)))
	}
	if m.screenErr != nil {
		rows = append(rows, th.Danger.Render("no screen: "+clean(m.screenErr.Error())))
	}
	for l := range strings.SplitSeq(strings.TrimRight(cleanScreen(m.screen), "\n"), "\n") {
		rows = append(rows, th.Text.Render(l))
	}
	return rows
}

func (m *Model) footer() string {
	th := m.theme
	if m.typing {
		return th.Accent.Render("send› ") + th.Text.Render(string(m.input)) + th.Faint.Render("▏  enter sends · esc cancels")
	}
	keys := [][2]string{{"j/k", "move"}, {"s", "send"}, {"a", "attach"}, {"x", "stop"}, {"u", "resume"}, {"r", "refresh"}, {"q", "quit"}}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = th.Accent.Render(k[0]) + " " + th.Faint.Render(k[1])
	}
	f := strings.Join(parts, th.Faint.Render(" · "))
	if m.status != "" {
		f = th.Text.Render(clean(m.status)) + th.Faint.Render("  |  ") + f
	}
	return f
}

// window is height rows of rows that include the one at cursor.
func window(rows []string, height, cursor int) []string {
	if len(rows) <= height {
		return rows
	}
	start := min(max(cursor-height+1, 0), len(rows)-height)
	return rows[start : start+height]
}

// tail is the last height rows: a terminal's news is at its bottom. The first
// row (the Job's title) always stays.
func tail(rows []string, height int) []string {
	if len(rows) <= height {
		return rows
	}
	return append([]string{rows[0]}, rows[len(rows)-height+1:]...)
}

func pad(s string, w int) string {
	if gap := w - ansi.StringWidth(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// clean is s without escape sequences or control characters: what a peer or an
// agent wrote must not drive our terminal.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, ansi.Strip(s))
}

// cleanScreen is clean for a screen: line breaks stay, tabs become spaces.
func cleanScreen(s string) string {
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\t", "    "), "\n")
	for i, l := range lines {
		lines[i] = clean(l)
	}
	return strings.Join(lines, "\n")
}
