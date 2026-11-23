//go:build linux || darwin

package runner_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/runner"
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
)

// fakeAgent runs the fake harness: it reads one line from its terminal into a file and exits 0.
type fakeAgent struct{ script string }

func (a fakeAgent) Prepare(_ context.Context, _ runner.Input) (runner.Plan, error) {
	return runner.Plan{
		Argv:      []string{os.Args[0], "--script", a.script},
		Env:       []string{fakeharness.AsFakeharnessEnv + "=1"},
		SessionID: "fake-session",
	}, nil
}

func (fakeAgent) Discover(context.Context, runner.Input, time.Time) ([]string, error) {
	return nil, nil
}

type env struct {
	r     *runner.Runner
	store *job.Store
	id    string
	out   string
}

func newEnv(t *testing.T, steps ...fakeharness.Step) env {
	t.Helper()
	return newEnvWith(t, func(script string) runner.Agent { return fakeAgent{script: script} }, steps...)
}

func newEnvWith(t *testing.T, agent func(script string) runner.Agent, steps ...fakeharness.Step) env {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	rt, err := tmux.New(tmux.Options{Socket: tmux.SocketName(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Check(t.Context()); err != nil {
		t.Skipf("tmux: %v", err)
	}
	t.Cleanup(func() { _ = rt.KillServer(context.WithoutCancel(t.Context())) })

	work := t.TempDir()
	out := filepath.Join(work, "received")
	steps = append([]fakeharness.Step{{ReadLine: &fakeharness.ReadLine{TimeoutMS: 20000, To: out}}}, steps...)
	script := filepath.Join(work, "script.json")
	raw, err := jsonScript(steps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	store := job.NewStore(t.TempDir())
	j, err := store.Create(job.Job{Repo: "example.com/o/r", Harness: "fake", Gen: 1, Peer: "p", State: job.Stopped, CreatedBy: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.WorktreeDir(j.ID, 1), 0o700); err != nil {
		t.Fatal(err)
	}
	r := runner.New(runner.Config{
		Store: store, Tmux: rt,
		Wrapper: func(dir string) []string { return []string{"/usr/bin/env", asWrapper + "=" + dir, os.Args[0]} },
		Agents:  map[string]runner.Agent{"fake": agent(script)},
		Stopper: runner.Stopper{Grace: 2 * time.Second, KillWait: 2 * time.Second, Poll: 20 * time.Millisecond},
	})
	t.Cleanup(func() { _ = r.Stop(context.WithoutCancel(t.Context()), j.ID) })
	return env{r: r, store: store, id: j.ID, out: out}
}

func (e env) state(t *testing.T) job.State {
	t.Helper()
	j, err := e.r.Reconcile(t.Context(), e.id)
	if err != nil {
		t.Fatal(err)
	}
	return j.State
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestLaunchWaitingSendAndExit(t *testing.T) {
	e := newEnv(t)
	j, err := e.r.Launch(t.Context(), e.id, "do it")
	if err != nil {
		t.Fatal(err)
	}
	if j.State != job.Running || j.Launch == nil || j.SessionID != "fake-session" {
		t.Fatalf("job after launch = %+v", j)
	}
	if want := job.SessionName(e.id, j.Launch.ID); j.Launch.Session != want {
		t.Errorf("session = %q, want %q", j.Launch.Session, want)
	}
	for _, name := range []string{"started.json", "wrapper.json"} {
		if _, err := os.Stat(filepath.Join(e.store.LaunchDir(e.id, j.Launch.ID), name)); err != nil {
			t.Errorf("record %s: %v", name, err)
		}
	}
	if got := e.state(t); got != job.Running {
		t.Fatalf("state = %s, want running", got)
	}
	if _, err := e.r.Screen(t.Context(), e.id); err != nil {
		t.Errorf("screen: %v", err)
	}

	// The agent's hook says it waits.
	marker := e.store.WaitingPath(e.id, j.Launch.ID)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := e.state(t); got != job.Waiting {
		t.Fatalf("state = %s, want waiting", got)
	}

	if err := e.r.Send(t.Context(), e.id, "the answer"); err != nil {
		t.Fatal(err)
	}
	if exists(marker) {
		t.Error("send did not clear the waiting marker")
	}
	eventually(t, "the agent to receive the text", func() bool {
		b, _ := os.ReadFile(e.out) //nolint:gosec // a file of the test
		return strings.TrimSpace(string(b)) == "the answer"
	})
	eventually(t, "the job to be done", func() bool { return e.state(t) == job.Done })
}

func TestStopEndsTheGroupAndTheSession(t *testing.T) {
	e := newEnv(t)
	j, err := e.r.Launch(t.Context(), e.id, "do it")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.r.Stop(t.Context(), e.id); err != nil {
		t.Fatal(err)
	}
	got, err := e.store.Get(e.id)
	if err != nil || got.State != job.Stopped {
		t.Fatalf("job = %+v, err %v", got, err)
	}
	f, err := e.r.Observe(t.Context(), got)
	if err != nil {
		t.Fatal(err)
	}
	if f.SessionAlive || !f.GroupEmpty {
		t.Errorf("facts after stop = %+v (launch %s)", f, j.Launch.ID)
	}
}

func TestLaunchThatDoesNotStartLeavesTheJobAlone(t *testing.T) {
	e := newEnv(t)
	if err := os.RemoveAll(e.store.WorktreeDir(e.id, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Launch(t.Context(), e.id, "x"); err == nil {
		t.Fatal("launch without a worktree succeeded")
	}
	j, _ := e.store.Get(e.id)
	if j.State != job.Stopped || j.Launch != nil {
		t.Errorf("job changed: %+v", j)
	}
}

// envDumpAgent runs a shell that writes its own environment to a file.
type envDumpAgent struct{ dump string }

func (a envDumpAgent) Prepare(context.Context, runner.Input) (runner.Plan, error) {
	return runner.Plan{Argv: []string{"/bin/sh", "-c", "env > " + a.dump}, SessionID: "fake-session"}, nil
}

func (envDumpAgent) Discover(context.Context, runner.Input, time.Time) ([]string, error) {
	return nil, nil
}

func TestHarnessStartsWithoutTheSessionMarkersOfTheCaller(t *testing.T) {
	t.Setenv("CLAUDE_CODE_CHILD_SESSION", "1")
	t.Setenv("CODEX_THREAD_ID", "thread")
	t.Setenv("CLAUDE_CONFIG_DIR", "/keep/me")
	dump := filepath.Join(t.TempDir(), "env")
	e := newEnvWith(t, func(string) runner.Agent { return envDumpAgent{dump: dump} })
	if _, err := e.r.Launch(t.Context(), e.id, "x"); err != nil {
		t.Fatal(err)
	}
	var got string
	eventually(t, "the harness to dump its environment", func() bool {
		b, err := os.ReadFile(dump) //nolint:gosec // a file of the test
		got = string(b)
		return err == nil && strings.Contains(got, "PATH=")
	})
	for _, marker := range []string{"CLAUDE_CODE_CHILD_SESSION=", "CODEX_THREAD_ID="} {
		if strings.Contains(got, marker) {
			t.Errorf("harness environment has %s", marker)
		}
	}
	if !strings.Contains(got, "CLAUDE_CONFIG_DIR=/keep/me") {
		t.Error("harness environment lost CLAUDE_CONFIG_DIR")
	}
}
