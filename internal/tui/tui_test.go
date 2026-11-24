package tui

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Autarquico/codeflock/internal/cli"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fakeJobs is the fleet of a test: it answers from memory and records orders.
type fakeJobs struct {
	mu      sync.Mutex
	list    cli.JobList
	listErr error
	screens map[string]string
	calls   []string
	failOn  string
}

func (f *fakeJobs) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if f.failOn == call {
		return errors.New("peer said no")
	}
	return nil
}

func (f *fakeJobs) List(context.Context, string) (cli.JobList, error) { return f.list, f.listErr }
func (f *fakeJobs) Screen(_ context.Context, id string) (string, error) {
	return f.screens[id], f.record("screen " + id)
}

func (f *fakeJobs) Send(_ context.Context, id, text string) (cli.JobView, error) {
	return cli.JobView{}, f.record("send " + id + " " + text)
}

func (f *fakeJobs) Stop(_ context.Context, id string) (cli.JobView, error) {
	return cli.JobView{}, f.record("stop " + id)
}

func (f *fakeJobs) Resume(_ context.Context, id string) (cli.JobView, error) {
	return cli.JobView{}, f.record("resume " + id)
}

func (f *fakeJobs) Attach(_ context.Context, id string, readOnly bool) error {
	if readOnly {
		return errors.New("attach must not be read-only from the TUI")
	}
	return f.record("attach " + id)
}

func fleet() *fakeJobs {
	return &fakeJobs{
		list: cli.JobList{
			Jobs: []cli.JobView{
				{ID: "j-01", Peer: "mac", Repo: "codefabric", Branch: "cflk/j-01", Harness: "claude", State: "waiting", Gen: 1},
				{ID: "j-02", Peer: "mac", Repo: "codefabric", Branch: "cflk/j-02", Harness: "codex", State: "running", Gen: 1},
				{ID: "j-03", Peer: "pi", Repo: "site", Branch: "cflk/j-03", Harness: "claude", State: "stopped", Gen: 2, Problem: "peer slow"},
			},
			Unreachable: []cli.UnreachablePeer{{Peer: "nas", Problem: "ssh: timeout"}},
		},
		screens: map[string]string{
			"j-01": "Do you want to run the tests?\n❯ 1. Yes\n  2. No\n",
			"j-02": "working…\n",
		},
	}
}

// drive runs a command and feeds what it produces back to the model, as the
// program would, until nothing is left (ticks are not followed).
func drive(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			drive(m, c)
		}
		return
	}
	_, next := m.Update(msg)
	drive(m, next)
}

func opened(t *testing.T, f *fakeJobs, w, h int) *Model {
	t.Helper()
	m := New(context.Background(), f, Options{})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	drive(m, m.fetchList())
	return m
}

func press(m *Model, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "backspace":
			msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		default:
			msg = tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
		}
		_, cmd := m.Update(msg)
		drive(m, cmd)
	}
}

func screenOf(m *Model) string { return ansi.Strip(m.View().Content) }

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // a fixed golden under testdata
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs:\n--- want\n%s\n--- got\n%s", name, want, got)
	}
}

func TestFleet_ListsJobsWithWaitingAndUnreachablePeers(t *testing.T) {
	m := opened(t, fleet(), 80, 24)
	golden(t, "fleet-80x24", screenOf(m))
}

func TestFleet_ShowsTheScreenOfTheSelectedJob(t *testing.T) {
	m := opened(t, fleet(), 80, 24)
	drive(m, m.fetchScreen())
	got := screenOf(m)
	if !strings.Contains(got, "Do you want to run the tests?") {
		t.Fatalf("screen of j-01 missing:\n%s", got)
	}
	press(m, "j")
	got = screenOf(m)
	if !strings.Contains(got, "working…") || strings.Contains(got, "run the tests") {
		t.Fatalf("screen did not follow the selection:\n%s", got)
	}
}

func TestFleet_NarrowTerminalTruncatesAndShortOneScrolls(t *testing.T) {
	m := opened(t, fleet(), 40, 12)
	drive(m, m.fetchScreen())
	golden(t, "fleet-40x12", screenOf(m))
}

func TestFleet_Empty(t *testing.T) {
	m := opened(t, &fakeJobs{}, 80, 24)
	golden(t, "empty-80x24", screenOf(m))
}

func TestFleet_ListError(t *testing.T) {
	m := opened(t, &fakeJobs{listErr: errors.New("no peers file")}, 80, 24)
	if got := screenOf(m); !strings.Contains(got, "cannot list: no peers file") {
		t.Fatalf("error not shown:\n%s", got)
	}
}

func TestSend_TypesAndSendsToTheSelectedJob(t *testing.T) {
	f := fleet()
	m := opened(t, f, 80, 24)
	press(m, "s", "y", "e", "s", "x", "backspace")
	if got := screenOf(m); !strings.Contains(got, "send› yes") {
		t.Fatalf("input line missing:\n%s", got)
	}
	press(m, "enter")
	if !contains(f.calls, "send j-01 yes") {
		t.Fatalf("calls = %v", f.calls)
	}
	if got := screenOf(m); !strings.Contains(got, "send done") {
		t.Fatalf("no feedback:\n%s", got)
	}
}

func TestSend_EscCancelsAndEmptyTextSendsNothing(t *testing.T) {
	f := fleet()
	m := opened(t, f, 80, 24)
	press(m, "s", "h", "i", "esc", "s", "enter")
	for _, c := range f.calls {
		if strings.HasPrefix(c, "send") {
			t.Fatalf("sent %q", c)
		}
	}
}

func TestOrders_StopResumeAttachActOnTheSelectedJob(t *testing.T) {
	f := fleet()
	m := opened(t, f, 80, 24)
	press(m, "j", "x", "u")
	for _, want := range []string{"stop j-02", "resume j-02"} {
		if !contains(f.calls, want) {
			t.Errorf("missing %q in %v", want, f.calls)
		}
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if cmd == nil {
		t.Fatal("attach returned no command")
	}
}

func TestOrders_FailureIsShownNotFatal(t *testing.T) {
	f := fleet()
	f.failOn = "stop j-01"
	m := opened(t, f, 80, 24)
	press(m, "x")
	if got := screenOf(m); !strings.Contains(got, "stop failed: peer said no") {
		t.Fatalf("failure not shown:\n%s", got)
	}
}

func TestAttach_RunsTheUseCaseForTheSelectedJob(t *testing.T) {
	f := fleet()
	m := opened(t, f, 80, 24)
	if err := (attachCmd{func() error { return m.jobs.Attach(m.ctx, "j-01", false) }}).Run(); err != nil {
		t.Fatal(err)
	}
	if !contains(f.calls, "attach j-01") {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestRefresh_KeepsTheSelectionOnTheSameJobWhenTheListChanges(t *testing.T) {
	f := fleet()
	m := opened(t, f, 80, 24)
	press(m, "j")
	f.list.Jobs = append([]cli.JobView{{ID: "j-00", Peer: "mac", State: "running"}}, f.list.Jobs...)
	press(m, "r")
	if j, _ := m.selected(); j.ID != "j-02" {
		t.Fatalf("selection moved to %s", j.ID)
	}
}

func TestSanitizing_PeerTextCannotDriveTheTerminal(t *testing.T) {
	f := fleet()
	f.screens["j-01"] = "\x1b[2Jcleared\x1b]0;title\x07\n"
	f.list.Jobs[0].Repo = "re\x1b[31mpo"
	m := opened(t, f, 80, 24)
	drive(m, m.fetchScreen())
	if raw := m.View().Content; strings.Contains(raw, "\x1b[2J") || strings.Contains(raw, "\x1b]0") || strings.Contains(ansi.Strip(raw), "[31m") {
		t.Fatalf("escape sequence reached the view: %q", raw)
	}
}

func TestInterval_DefaultsWhenZero(t *testing.T) {
	if m := New(context.Background(), &fakeJobs{}, Options{}); m.interval != DefaultInterval {
		t.Fatalf("interval = %v", m.interval)
	}
}

func contains(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}
