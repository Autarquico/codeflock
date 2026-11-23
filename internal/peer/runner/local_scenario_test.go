//go:build linux || darwin

package runner_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/runner"
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
	"github.com/Autarquico/codeflock/internal/worker/execwrap"
	"github.com/Autarquico/codeflock/internal/worker/procident"
)

// Scenarios of docs/specs/p2p-peers.md that F2 covers on one peer: a real tmux
// server on its own socket, the real wrapper, and the fake harness as the agent.

// scriptedAgent is a harness that runs the fake harness with a script and
// remembers the session it was asked to resume.
type scriptedAgent struct {
	script string
	mu     *sync.Mutex
	asked  *[]string
}

func (a scriptedAgent) Prepare(_ context.Context, in runner.Input) (runner.Plan, error) {
	a.mu.Lock()
	*a.asked = append(*a.asked, in.SessionID)
	a.mu.Unlock()
	return runner.Plan{
		Argv:      []string{os.Args[0], "--script", a.script},
		Env:       []string{fakeharness.AsFakeharnessEnv + "=1"},
		SessionID: "session-of-the-agent",
	}, nil
}

func (scriptedAgent) Discover(context.Context, runner.Input, time.Time) ([]string, error) {
	return nil, nil
}

type peerUnderTest struct {
	rt    *tmux.Runtime
	store *job.Store
	run   *runner.Runner
	asked *[]string
}

// scenarioJob is a Job of the peer and the file where its agent writes what it receives.
type scenarioJob struct {
	id, received string
}

func newPeer(t *testing.T, stopper runner.Stopper, harnesses ...string) (peerUnderTest, map[string]string) {
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

	asked, mu := new([]string), new(sync.Mutex)
	agents, scripts := map[string]runner.Agent{}, map[string]string{}
	for _, name := range harnesses {
		scripts[name] = filepath.Join(t.TempDir(), "script.json")
		agents[name] = scriptedAgent{script: scripts[name], mu: mu, asked: asked}
	}
	store := job.NewStore(t.TempDir())
	p := peerUnderTest{rt: rt, store: store, asked: asked}
	p.run = p.runnerWith(stopper, agents)
	return p, scripts
}

func (p peerUnderTest) runnerWith(stopper runner.Stopper, agents map[string]runner.Agent) *runner.Runner {
	return runner.New(runner.Config{
		Store: p.store, Tmux: p.rt,
		Wrapper: func(dir string) []string { return []string{"/usr/bin/env", asWrapper + "=" + dir, os.Args[0]} },
		Agents:  agents,
		Stopper: stopper,
	})
}

// newJob stores a stopped Job of the harness whose agent asks question, waits for one
// line, writes it to a file and ends.
func (p peerUnderTest) newJob(t *testing.T, harnessName, script, question string) scenarioJob {
	t.Helper()
	received := filepath.Join(t.TempDir(), "received")
	steps := []fakeharness.Step{
		{Emit: &fakeharness.Line{Type: harness.EventInputRequired, Message: question}},
		{ReadLine: &fakeharness.ReadLine{TimeoutMS: 20000, To: received}},
	}
	raw, err := jsonScript(steps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := p.store.Create(job.Job{Repo: "example.com/o/r", Harness: harnessName, Gen: 1, Peer: "p", State: job.Stopped, CreatedBy: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.store.WorktreeDir(j.ID, 1), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.run.Stop(context.WithoutCancel(t.Context()), j.ID) })
	return scenarioJob{id: j.ID, received: received}
}

func (p peerUnderTest) state(t *testing.T, id string) job.State {
	t.Helper()
	j, err := p.run.Reconcile(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j.State
}

// theAgentAsks marks the Job as waiting the way the hooks of the harness do.
func (p peerUnderTest) theAgentAsks(t *testing.T, id string) {
	t.Helper()
	j, err := p.store.Get(id)
	if err != nil || j.Launch == nil {
		t.Fatalf("job %s: %+v, %v", id, j, err)
	}
	if err := os.WriteFile(p.store.WaitingPath(id, j.Launch.ID), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (p peerUnderTest) screenShows(t *testing.T, id, text string) bool {
	t.Helper()
	b, err := p.run.Screen(t.Context(), id)
	return err == nil && strings.Contains(string(b), text)
}

func receivedLine(path string) string {
	b, _ := os.ReadFile(path) //nolint:gosec // a file of the test
	return strings.TrimSpace(string(b))
}

func TestGivenAWaitingJobWhenTheModelReadsTheScreenAndSendsTheAnswerThenTheAgentGetsItAndGoesOn(t *testing.T) {
	p, scripts := newPeer(t, runner.Stopper{}, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Which branch should I use?")
	if _, err := p.run.Launch(t.Context(), sj.id, "work"); err != nil {
		t.Fatal(err)
	}
	p.theAgentAsks(t, sj.id)
	if got := p.state(t, sj.id); got != job.Waiting {
		t.Fatalf("state = %s, want waiting", got)
	}

	eventually(t, "the screen to show the question", func() bool { return p.screenShows(t, sj.id, "Which branch should I use?") })
	if err := p.run.Send(t.Context(), sj.id, "use main"); err != nil {
		t.Fatal(err)
	}

	eventually(t, "the agent to receive the answer", func() bool { return receivedLine(sj.received) == "use main" })
	eventually(t, "the agent to go on and end", func() bool { return p.state(t, sj.id) == job.Done })
}

func TestGivenAClaudeJobAndACodexJobWhenEachAgentAsksThenBothAreWaitingAndRunningAgainOnceAnswered(t *testing.T) {
	p, scripts := newPeer(t, runner.Stopper{}, "fake-claude", "fake-codex")
	jobs := []scenarioJob{
		p.newJob(t, "fake-claude", scripts["fake-claude"], "Allow this edit?"),
		p.newJob(t, "fake-codex", scripts["fake-codex"], "Run the tests?"),
	}
	for _, sj := range jobs {
		if _, err := p.run.Launch(t.Context(), sj.id, "work"); err != nil {
			t.Fatal(err)
		}
		if got := p.state(t, sj.id); got != job.Running {
			t.Fatalf("job %s before asking = %s, want running", sj.id, got)
		}
	}

	for _, sj := range jobs {
		p.theAgentAsks(t, sj.id)
	}
	for _, sj := range jobs {
		if got := p.state(t, sj.id); got != job.Waiting {
			t.Errorf("job %s after asking = %s, want waiting", sj.id, got)
		}
	}

	for _, sj := range jobs {
		if err := p.run.Send(t.Context(), sj.id, "yes"); err != nil {
			t.Fatal(err)
		}
		j, err := p.store.Get(sj.id)
		if err != nil || j.State != job.Running {
			t.Errorf("job %s right after the answer = %+v, %v; want running", sj.id, j, err)
		}
	}
}

func TestGivenARunningJobWhenThePeerRestartsThenItIsLostNothingRelaunchesItAndResumeKeepsItsSession(t *testing.T) {
	p, scripts := newPeer(t, runner.Stopper{Grace: 2 * time.Second, KillWait: 2 * time.Second, Poll: 20 * time.Millisecond}, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	launched, err := p.run.Launch(t.Context(), sj.id, "work")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.state(t, sj.id); got != job.Running {
		t.Fatalf("state = %s, want running", got)
	}

	// A restart takes the tmux server and every process with it, and leaves no exit record.
	var w execwrap.Wrapper
	if ok, err := execwrap.Read(p.store.LaunchDir(sj.id, launched.Launch.ID), execwrap.WrapperFile, &w); err != nil || !ok {
		t.Fatalf("wrapper record: %v, %v", ok, err)
	}
	if err := p.rt.KillServer(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
	// SIGKILL is asynchronous: observing while the group is still dying would (rightly) write
	// stopping, which only a stop leaves. A restart is over once nothing of the group is left.
	eventually(t, "the group to be gone", func() bool {
		empty, err := procident.GroupEmpty(w.PGID)
		return err == nil && empty
	})
	eventually(t, "the job to be seen as lost", func() bool { return p.state(t, sj.id) == job.Lost })

	after, err := p.store.Get(sj.id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Launch == nil || after.Launch.ID != launched.Launch.ID {
		t.Errorf("a new launch appeared after the restart: %+v", after.Launch)
	}
	if n := len(*p.asked); n != 1 {
		t.Errorf("the agent was prepared %d times, want 1 (nothing relaunches a lost job)", n)
	}

	resumed, err := p.run.Launch(t.Context(), sj.id, "")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != job.Running || resumed.Launch.ID == launched.Launch.ID {
		t.Errorf("after resume = %+v", resumed)
	}
	if got := (*p.asked)[len(*p.asked)-1]; got != launched.SessionID {
		t.Errorf("resume asked for session %q, want %q", got, launched.SessionID)
	}
	if got := p.state(t, sj.id); got != job.Running {
		t.Errorf("state after resume = %s, want running", got)
	}
}

func TestGivenAStopWhoseProcessSurvivesWhenStoppingThenTheJobStaysStoppingAndStoppingAgainEndsItStopped(t *testing.T) {
	swallowed := runner.Stopper{
		Grace: 50 * time.Millisecond, KillWait: 50 * time.Millisecond, Poll: 10 * time.Millisecond,
		Signal: func(int, syscall.Signal) error { return nil }, // the process ignores every signal
	}
	p, scripts := newPeer(t, swallowed, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	if _, err := p.run.Launch(t.Context(), sj.id, "work"); err != nil {
		t.Fatal(err)
	}

	if err := p.run.Stop(t.Context(), sj.id); err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Fatalf("stop with survivors = %v, want an error saying it stays stopping", err)
	}
	if got := p.state(t, sj.id); got != job.Stopping {
		t.Fatalf("state after the failed stop = %s, want stopping (reconcile must not end it)", got)
	}

	again := p.runnerWith(runner.Stopper{Grace: 2 * time.Second, KillWait: 2 * time.Second, Poll: 20 * time.Millisecond}, map[string]runner.Agent{})
	if err := again.Stop(t.Context(), sj.id); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	if j, err := p.store.Get(sj.id); err != nil || j.State != job.Stopped {
		t.Fatalf("job after the second stop = %+v, %v; want stopped", j, err)
	}
}

func TestGivenAWaitingJobWhenLaunchedAgainThenItIsRefusedAndTheLaunchIsKept(t *testing.T) {
	p, scripts := newPeer(t, runner.Stopper{}, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	launched, err := p.run.Launch(t.Context(), sj.id, "work")
	if err != nil {
		t.Fatal(err)
	}
	p.theAgentAsks(t, sj.id)
	if got := p.state(t, sj.id); got != job.Waiting {
		t.Fatalf("state = %s, want waiting", got)
	}

	for _, state := range []job.State{job.Waiting, job.Running, job.Stopping, job.Done} {
		err := p.store.WithLock(sj.id, func(j *job.Job) error { j.State = state; return nil })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.run.Launch(t.Context(), sj.id, ""); !errors.Is(err, job.ErrInvalidTransition) {
			t.Errorf("launch of a %s Job = %v, want ErrInvalidTransition", state, err)
		}
	}
	// Back to what it was, so that the cleanup stops the agent.
	if err := p.store.WithLock(sj.id, func(j *job.Job) error { j.State = job.Waiting; return nil }); err != nil {
		t.Fatal(err)
	}
	if j, _ := p.store.Get(sj.id); j.Launch == nil || j.Launch.ID != launched.Launch.ID {
		t.Errorf("a refused launch replaced the launch: %+v", j.Launch)
	}
	if n := len(*p.asked); n != 1 {
		t.Errorf("the agent was prepared %d times, want 1", n)
	}
}

func TestGivenAStopInProgressWhenTheJobIsReadThenItShowsStoppingWithoutWaiting(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	slow := runner.Stopper{
		Grace: 2 * time.Second, KillWait: 2 * time.Second, Poll: 20 * time.Millisecond,
		Signal: func(pgid int, sig syscall.Signal) error {
			once.Do(func() { close(entered); <-release })
			return syscall.Kill(-pgid, sig)
		},
	}
	p, scripts := newPeer(t, slow, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	if _, err := p.run.Launch(t.Context(), sj.id, "work"); err != nil {
		t.Fatal(err)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- p.run.Stop(context.WithoutCancel(t.Context()), sj.id) }()
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the stop never signalled")
	}
	// Reading takes the lock of the Job: it must not wait for the stop to end.
	read := make(chan job.State, 1)
	go func() {
		j, _ := p.run.Reconcile(context.WithoutCancel(t.Context()), sj.id)
		read <- j.State
	}()
	select {
	case got := <-read:
		if got != job.Stopping {
			t.Errorf("state during the stop = %s, want stopping", got)
		}
	case <-time.After(5 * time.Second):
		t.Error("reading the Job waited for the stop to end")
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := p.state(t, sj.id); got != job.Stopped {
		t.Errorf("state after the stop = %s, want stopped", got)
	}
}
