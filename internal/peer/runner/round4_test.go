//go:build linux || darwin

package runner_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/runner"
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
	"github.com/Autarquico/codeflock/internal/worker/execwrap"
	"github.com/Autarquico/codeflock/internal/worker/procident"
)

func TestGivenASessionKnownBeforeTheStartThenTheJobRecordsItBeforeAnythingStarts(t *testing.T) {
	p, scripts := newPeer(t, runner.Stopper{}, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	var seen job.Job
	r := runner.New(runner.Config{
		Store: p.store, Tmux: p.rt,
		Wrapper: func(dir string) []string {
			seen, _ = p.store.Get(sj.id) // the session is started right after this
			return []string{"/usr/bin/env", asWrapper + "=" + dir, os.Args[0]}
		},
		Agents: map[string]runner.Agent{"fake": scriptedAgent{script: scripts["fake"], mu: &sync.Mutex{}, asked: new([]string)}},
	})
	if _, err := r.Launch(t.Context(), sj.id, "work"); err != nil {
		t.Fatal(err)
	}
	if seen.SessionID != "session-of-the-agent" {
		t.Errorf("job when the session started = %+v, want its session recorded", seen)
	}
}

func TestGivenAJobWhoseLaunchDiscoveredASessionItNeverRecordedThenResumingUsesIt(t *testing.T) {
	p, scripts := newPeer(t, runner.Stopper{}, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	l := job.NewLaunch(sj.id)
	if err := p.store.WithLock(sj.id, func(j *job.Job) error { j.Launch = &l; return nil }); err != nil {
		t.Fatal(err)
	}
	dir := p.store.LaunchDir(sj.id, l.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, runner.SessionFile), []byte("discovered-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.run.Launch(t.Context(), sj.id, ""); err != nil {
		t.Fatal(err)
	}
	if got := *p.asked; len(got) != 1 || got[0] != "discovered-1" {
		t.Errorf("session asked to resume = %v, want [discovered-1]", got)
	}
}

func TestGivenASendThatFailsBeforeTheEnterThenTheWaitingMarkerIsStillThere(t *testing.T) {
	p, scripts := newPeer(t, runner.Stopper{}, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	l := job.NewLaunch(sj.id) // its tmux session does not exist: the send fails
	if err := p.store.WithLock(sj.id, func(j *job.Job) error { j.Launch, j.State = &l, job.Waiting; return nil }); err != nil {
		t.Fatal(err)
	}
	marker := p.store.WaitingPath(sj.id, l.ID)
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.run.Send(t.Context(), sj.id, "text"); err == nil {
		t.Fatal("send to a session that does not exist succeeded")
	}
	if !exists(marker) {
		t.Error("the waiting marker was lost by a send that never reached the agent")
	}
	if got, _ := p.store.Get(sj.id); got.State != job.Waiting {
		t.Errorf("state = %s, want waiting", got.State)
	}
	t.Cleanup(func() { _ = p.store.WithLock(sj.id, func(j *job.Job) error { return j.Transition(job.Stopping) }) })
}

// A harness that ends by itself while a Stop begins is done, not stopped.
func TestGivenAnAgentThatFinishedByItselfWhenStopSeesItAliveThenTheJobIsDoneNotStopped(t *testing.T) {
	var once sync.Once
	stopper := runner.Stopper{
		Grace: 2 * time.Second, KillWait: 2 * time.Second, Poll: 20 * time.Millisecond,
		Verify: func(pid int, start string) procident.Verdict {
			v := procident.Verify(pid, start)
			once.Do(func() { v = procident.Same }) // the first look, reconcile's, finds it alive
			return v
		},
	}
	p, scripts := newPeer(t, stopper, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	raw, err := jsonScript([]fakeharness.Step{{Emit: &fakeharness.Line{Type: harness.EventInputRequired, Message: "bye"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scripts["fake"], raw, 0o600); err != nil {
		t.Fatal(err)
	}
	launched, err := p.run.Launch(t.Context(), sj.id, "work")
	if err != nil {
		t.Fatal(err)
	}
	exit := p.store.ExitPath(sj.id, launched.Launch.ID)
	waitFor(t, "the agent to end by itself", func() bool {
		var e execwrap.Exit
		ok, _ := execwrap.Read(filepath.Dir(exit), filepath.Base(exit), &e)
		return ok
	})
	if err := p.run.Stop(t.Context(), sj.id); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.store.Get(sj.id); got.State != job.Done {
		t.Errorf("state = %s, want done", got.State)
	}
}

// A Stop that was cut short after writing stopping, on a Job that had failed by
// itself, finishes as stopped when retried: the old exit is not a new outcome.
func TestGivenAStopRetriedOnAJobThatHadFailedThenItEndsStopped(t *testing.T) {
	p, scripts := newPeer(t, runner.Stopper{}, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	l := job.NewLaunch(sj.id)
	if err := p.store.WithLock(sj.id, func(j *job.Job) error {
		j.Launch, j.StoppingFrom, j.State = &l, job.Failed, job.Stopping
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	code := 3
	writeExit(t, p.store.ExitPath(sj.id, l.ID), execwrap.Exit{ExitCode: &code, Cause: execwrap.CauseExit, ExitedAt: time.Now().Add(-time.Minute)})
	for range 2 { // the retry, and a second one after it
		if err := p.run.Stop(t.Context(), sj.id); err != nil {
			t.Fatal(err)
		}
		if got, _ := p.store.Get(sj.id); got.State != job.Stopped {
			t.Fatalf("state = %s, want stopped", got.State)
		}
	}
}

func writeExit(t *testing.T, path string, e execwrap.Exit) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := execwrap.Write(filepath.Dir(path), filepath.Base(path), e); err != nil {
		t.Fatal(err)
	}
}

type discoveringAgent struct {
	scriptedAgent
	ids []string
}

func (a discoveringAgent) Discover(context.Context, runner.Input, time.Time) ([]string, error) {
	return a.ids, nil
}

// A launch cut short after its harness created a session but before settle wrote
// the session file is resumed with the session the harness created.
func TestGivenALaunchCutShortBeforeItWroteItsSessionThenResumingFindsTheSessionTheHarnessCreated(t *testing.T) {
	p, scripts := newPeer(t, runner.Stopper{}, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	l := cutShortLaunch(t, p, sj.id)
	agent := discoveringAgent{scriptedAgent: scriptedAgent{script: scripts["fake"], mu: &sync.Mutex{}, asked: p.asked}, ids: []string{"rollout-1"}}
	run := p.runnerWith(runner.Stopper{}, map[string]runner.Agent{"fake": agent})
	if _, err := run.Launch(t.Context(), sj.id, ""); err != nil {
		t.Fatal(err)
	}
	if got := *p.asked; len(got) != 1 || got[0] != "rollout-1" {
		t.Errorf("session asked to resume = %v, want [rollout-1]", got)
	}
	raw, err := os.ReadFile(filepath.Join(p.store.LaunchDir(sj.id, l.ID), runner.SessionFile))
	if err != nil || string(raw) != "rollout-1" {
		t.Errorf("session file of the cut-short launch = %q, %v; want rollout-1", raw, err)
	}
}

func TestGivenSeveralSessionsSinceTheLaunchStartedThenResumingSaysItCannotChooseOne(t *testing.T) {
	p, scripts := newPeer(t, runner.Stopper{}, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	cutShortLaunch(t, p, sj.id)
	agent := discoveringAgent{scriptedAgent: scriptedAgent{script: scripts["fake"], mu: &sync.Mutex{}, asked: p.asked}, ids: []string{"a", "b"}}
	run := p.runnerWith(runner.Stopper{}, map[string]runner.Agent{"fake": agent})
	_, err := run.Launch(t.Context(), sj.id, "")
	if err == nil || !strings.Contains(err.Error(), "2 sessions") {
		t.Fatalf("resume error = %v, want it to say 2 sessions were found", err)
	}
	if got, _ := p.store.Get(sj.id); got.State != job.Stopped || got.SessionID != "" {
		t.Errorf("job = %s with session %q, want stopped and without one", got.State, got.SessionID)
	}
}

// cutShortLaunch gives the stopped Job a launch whose harness started but whose
// session file was never written.
func cutShortLaunch(t *testing.T, p peerUnderTest, id string) job.Launch {
	t.Helper()
	l := job.NewLaunch(id)
	if err := p.store.WithLock(id, func(j *job.Job) error { j.Launch = &l; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.store.LaunchDir(id, l.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := execwrap.Write(p.store.LaunchDir(id, l.ID), filepath.Base(p.store.StartedPath(id, l.ID)), execwrap.Started{StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	return l
}
