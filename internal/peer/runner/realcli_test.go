//go:build realcli && (linux || darwin)

package runner_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
	"github.com/Autarquico/codeflock/internal/adapters/harness/localhost"
	"github.com/Autarquico/codeflock/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/runner"
)

// These tests drive the real CLIs with the login of the machine and call a model
// (a few tokens). Claude Code gets the worktree marked as trusted in ~/.claude.json.
//
//	go test -tags realcli ./internal/peer/...
func realRunner(t *testing.T, binary, harnessID string) (*runner.Runner, *job.Store, string) {
	t.Helper()
	if _, err := exec.LookPath(binary); err != nil {
		t.Skipf("%s is not installed", binary)
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	host, err := localhost.New(localhost.Options{Home: home, Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := tmux.New(tmux.Options{Socket: tmux.SocketName(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.KillServer(context.WithoutCancel(t.Context())) })
	store := job.NewStore(t.TempDir())
	j, err := store.Create(job.Job{Repo: "example.com/o/r", Harness: harnessID, Gen: 1, Peer: "p", State: job.Stopped, CreatedBy: "p"})
	if err != nil {
		t.Fatal(err)
	}
	worktree := store.WorktreeDir(j.ID, 1)
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	// Claude's trust of the worktree lives in the real ~/.claude.json: take it out again.
	t.Cleanup(func() { _ = claude.Adapter{}.UntrustFolder(host, worktree) })
	r := runner.New(runner.Config{
		Store: store, Host: host, Tmux: rt,
		Wrapper:   func(dir string) []string { return []string{"/usr/bin/env", asWrapper + "=" + dir, os.Args[0]} },
		NotifyCmd: []string{"/usr/bin/env", asNotify + "=1", os.Args[0]},
	})
	t.Cleanup(func() { _ = r.Stop(context.WithoutCancel(t.Context()), j.ID) })
	return r, store, j.ID
}

// firstTurn is how long the first turn may take, the start of the CLI included.
var firstTurn = 40 * time.Second

func waitState(t *testing.T, r *runner.Runner, id string, want job.State, within time.Duration) {
	t.Helper()
	var last job.State
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		j, err := r.Reconcile(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if last = j.State; last == want {
			return
		}
	}
	screen, _ := r.Screen(t.Context(), id)
	t.Fatalf("the job is %s, not %s after %s; the screen:\n%s", last, want, within, screen)
}

func exerciseTurns(t *testing.T, r *runner.Runner, id string) {
	t.Helper()
	waitState(t, r, id, job.Waiting, firstTurn)
	if err := r.Send(t.Context(), id, "Reply with the single word PONG and nothing else."); err != nil {
		t.Fatal(err)
	}
	j, err := r.Reconcile(t.Context(), id)
	if err != nil || j.State != job.Running {
		t.Fatalf("after send: %+v, %v", j, err)
	}
	waitState(t, r, id, job.Waiting, 3*time.Minute)
	if screen, _ := r.Screen(t.Context(), id); !strings.Contains(string(screen), "PONG") {
		t.Errorf("the screen does not show the answer:\n%s", screen)
	}
}

func TestRealClaudeWaitsAfterATurnAndSendClearsIt(t *testing.T) {
	r, _, id := realRunner(t, "claude", claude.ID)
	j, err := r.Launch(t.Context(), id, "Reply with the single word READY and nothing else.")
	if err != nil {
		t.Fatal(err)
	}
	if j.SessionID == "" {
		t.Fatal("no session id")
	}
	exerciseTurns(t, r, id)
}

func TestRealCodexWaitsAfterATurnAndSendClearsIt(t *testing.T) {
	r, _, id := realRunner(t, "codex", codex.ID)
	j, err := r.Launch(t.Context(), id, "Reply with the single word READY and nothing else.")
	if err != nil {
		t.Fatal(err)
	}
	if j.SessionID == "" {
		t.Fatal("the session was not discovered")
	}
	exerciseTurns(t, r, id)
}
