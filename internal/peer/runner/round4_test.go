//go:build linux || darwin

package runner_test

import (
	"os"
	"path/filepath"
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

func TestGivenASendThatFailsThenTheWaitingMarkerIsNotRestored(t *testing.T) {
	p, scripts := newPeer(t, runner.Stopper{}, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	l := job.NewLaunch(sj.id) // its tmux session does not exist: the send fails
	if err := p.store.WithLock(sj.id, func(j *job.Job) error { j.Launch = &l; return j.Transition(job.Running) }); err != nil {
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
	if exists(marker) {
		t.Error("the marker was restored after a failed send")
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
