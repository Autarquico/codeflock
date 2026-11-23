//go:build linux || darwin

package runner_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/peer/runner"
)

func notifyFixture(t *testing.T, session string) (marker, sessionFile string) {
	t.Helper()
	dir := t.TempDir()
	marker, sessionFile = filepath.Join(dir, "waiting"), filepath.Join(dir, runner.SessionFile)
	if session != "" {
		if err := os.WriteFile(sessionFile, []byte(session+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return marker, sessionFile
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestHandleNotify(t *testing.T) {
	const job, other = "0196a7b2-0000-7000-8000-000000000001", "0196a7b2-0000-7000-8000-000000000002"
	tests := []struct {
		name, session, payload string
		wantMarker, wantErr    bool
	}{
		{"the turn of the job's session", job, `{"type":"agent-turn-complete","thread-id":"` + job + `"}`, true, false},
		{"an internal thread", job, `{"type":"agent-turn-complete","thread-id":"` + other + `"}`, false, false},
		{"another kind of notification", job, `{"type":"approval-requested","thread-id":"` + job + `"}`, false, false},
		{"a session not known yet", "", `{"type":"agent-turn-complete","thread-id":"` + job + `"}`, false, false},
		{"no thread", job, `{"type":"agent-turn-complete"}`, false, false},
		{"not json", job, `nope`, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marker, sessionFile := notifyFixture(t, tt.session)
			err := runner.HandleNotify(marker, sessionFile, tt.payload)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if got := exists(marker); got != tt.wantMarker {
				t.Errorf("marker = %v, want %v", got, tt.wantMarker)
			}
		})
	}
}

// A turn that ends before the launch knows its session is not lost: the launch
// marks it waiting once it records the session, whichever comes first.
func TestATurnThatEndsBeforeTheSessionIsKnownMarksTheJobWaitingOnceItIs(t *testing.T) {
	const job, other = "0196a7b2-0000-7000-8000-000000000001", "0196a7b2-0000-7000-8000-000000000002"
	turn := func(thread string) string { return `{"type":"agent-turn-complete","thread-id":"` + thread + `"}` }

	t.Run("the turn of the session came first", func(t *testing.T) {
		marker, sessionFile := notifyFixture(t, "")
		for _, thread := range []string{other, job} {
			if err := runner.HandleNotify(marker, sessionFile, turn(thread)); err != nil {
				t.Fatal(err)
			}
		}
		if exists(marker) {
			t.Fatal("marked waiting before the session was known")
		}
		if err := os.WriteFile(sessionFile, []byte(job), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runner.MarkTurnsEndedBefore(marker, sessionFile, job); err != nil {
			t.Fatal(err)
		}
		if !exists(marker) {
			t.Error("the turn that ended before the session was known was lost")
		}
	})
	t.Run("only other threads ended", func(t *testing.T) {
		marker, sessionFile := notifyFixture(t, "")
		if err := runner.HandleNotify(marker, sessionFile, turn(other)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sessionFile, []byte(job), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runner.MarkTurnsEndedBefore(marker, sessionFile, job); err != nil {
			t.Fatal(err)
		}
		if exists(marker) {
			t.Error("another thread's turn marked the Job waiting")
		}
	})
}

// The hooks of a Claude launch are plain shell commands: Stop sets the marker and
// UserPromptSubmit removes it, whatever the path holds.
func TestClaudeSettingsHooksSetAndClearTheMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "it's a dir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker, settings := filepath.Join(dir, "waiting"), filepath.Join(dir, runner.ClaudeSettingsFile)
	if err := runner.WriteClaudeSettings(settings, marker); err != nil {
		t.Fatal(err)
	}
	hooks := runner.ReadHooks(t, settings)
	for _, step := range []struct {
		event      string
		wantMarker bool
	}{{"Stop", true}, {"UserPromptSubmit", false}} {
		event, wantMarker := step.event, step.wantMarker
		cmds := hooks[event]
		if len(cmds) != 1 {
			t.Fatalf("%s hooks = %v", event, cmds)
		}
		if err := exec.CommandContext(t.Context(), "/bin/sh", "-c", cmds[0]).Run(); err != nil { //nolint:gosec // the hook under test
			t.Fatalf("%s: %v", event, err)
		}
		if got := exists(marker); got != wantMarker {
			t.Errorf("after %s the marker is %v, want %v", event, got, wantMarker)
		}
	}
	if strings.Contains(strings.Join(hooks["Notification"], ""), "waiting") {
		t.Error("Notification must not be a hook")
	}
}
