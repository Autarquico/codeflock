//go:build linux || darwin

package runner_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
	"github.com/Autarquico/codeflock/internal/adapters/harness/localhost"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/runner"
)

// probeAgent is scriptedAgent that looks at the Job on disk when it is asked to
// plan, and may leave its session to be discovered.
type probeAgent struct {
	scriptedAgent
	store    *job.Store
	seen     *job.Job
	discover []string
}

func (a probeAgent) Prepare(ctx context.Context, in runner.Input) (runner.Plan, error) {
	j, err := a.store.Get(in.Job.ID)
	if err != nil {
		return runner.Plan{}, err
	}
	*a.seen = j
	plan, err := a.scriptedAgent.Prepare(ctx, in)
	if a.discover != nil {
		plan.SessionID = ""
	}
	return plan, err
}

func (a probeAgent) Discover(context.Context, runner.Input, time.Time) ([]string, error) {
	return a.discover, nil
}

func (p peerUnderTest) withProbe(t *testing.T, stopper runner.Stopper, discover []string) (*runner.Runner, scenarioJob, *job.Job) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "script.json")
	sj := p.newJob(t, "probe", script, "Ready?")
	seen := new(job.Job)
	agent := probeAgent{scriptedAgent: scriptedAgent{script: script, mu: &sync.Mutex{}, asked: new([]string)}, store: p.store, seen: seen, discover: discover}
	return p.runnerWith(stopper, map[string]runner.Agent{"probe": agent}), sj, seen
}

func TestGivenALaunchWhenTheAgentIsPlannedThenTheJobAlreadyRecordsTheLaunchRunning(t *testing.T) {
	p, _ := newPeer(t, runner.Stopper{})
	r, sj, seen := p.withProbe(t, runner.Stopper{}, nil)
	launched, err := r.Launch(t.Context(), sj.id, "work")
	if err != nil {
		t.Fatal(err)
	}
	if seen.State != job.Running || seen.Launch == nil || seen.Launch.ID != launched.Launch.ID {
		t.Errorf("job when the harness was planned = %+v, want running with launch %s", seen, launched.Launch.ID)
	}
}

func TestGivenALaunchThatFailsWithItsGroupGoneThenTheJobIsAsItWas(t *testing.T) {
	stopper := runner.Stopper{Grace: 2 * time.Second, KillWait: 2 * time.Second, Poll: 20 * time.Millisecond}
	p, _ := newPeer(t, stopper)
	r, sj, _ := p.withProbe(t, stopper, []string{"one", "two"}) // two sessions: the discovery fails
	if _, err := r.Launch(t.Context(), sj.id, "work"); err == nil {
		t.Fatal("launch with two discovered sessions succeeded")
	}
	if j, _ := p.store.Get(sj.id); j.State != job.Stopped || j.Launch != nil || j.SessionID != "" {
		t.Errorf("job = %+v, want stopped and never launched", j)
	}
}

func TestGivenALaunchThatFailsWithItsGroupSurvivingThenTheJobStaysStoppingWithTheLaunchAndIsNotLaunchedAgain(t *testing.T) {
	swallowed := runner.Stopper{
		Grace: 50 * time.Millisecond, KillWait: 50 * time.Millisecond, Poll: 10 * time.Millisecond,
		Signal: func(int, syscall.Signal) error { return nil },
	}
	p, _ := newPeer(t, swallowed)
	r, sj, _ := p.withProbe(t, swallowed, []string{"one", "two"})
	_, err := r.Launch(t.Context(), sj.id, "work")
	if err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Fatalf("launch = %v, want an error saying the job is stopping", err)
	}
	j, _ := p.store.Get(sj.id)
	if j.State != job.Stopping || j.Launch == nil {
		t.Fatalf("job = %+v, want stopping with its launch recorded", j)
	}
	if _, err := r.Launch(t.Context(), sj.id, "again"); !errors.Is(err, job.ErrInvalidTransition) {
		t.Errorf("a second launch = %v, want it refused", err)
	}
	patient := p.runnerWith(runner.Stopper{Grace: 2 * time.Second, KillWait: 2 * time.Second, Poll: 20 * time.Millisecond}, nil)
	if err := patient.Stop(t.Context(), sj.id); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestGivenAnUnreadableWrapperRecordThenNothingIsConcludedFromIt(t *testing.T) {
	p, scripts := newPeer(t, runner.Stopper{}, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	launched, err := p.run.Launch(t.Context(), sj.id, "work")
	if err != nil {
		t.Fatal(err)
	}
	dir := p.store.LaunchDir(sj.id, launched.Launch.ID)
	record := filepath.Join(dir, "wrapper.json")
	original, err := os.ReadFile(record) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	// Put the record back before the cleanup stops the job: it must be able to.
	t.Cleanup(func() { _ = os.WriteFile(record, original, 0o600) }) //nolint:gosec // a file of the test
	if err := os.WriteFile(record, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.run.Observe(t.Context(), launched); err == nil {
		t.Error("observing a launch with a corrupt wrapper record said something")
	}
	if err := p.run.Stop(t.Context(), sj.id); err == nil {
		t.Error("stop reported a launch with a corrupt wrapper record as stopped")
	}
	if j, _ := p.store.Get(sj.id); j.State == job.Stopped || j.State == job.Lost {
		t.Errorf("state = %s: concluded from an unreadable record", j.State)
	}
}

func TestGivenAnAgentThatFinishedCleanlyWhenStopIsAskedThenItIsDoneNotStopped(t *testing.T) {
	p, scripts := newPeer(t, runner.Stopper{}, "fake")
	sj := p.newJob(t, "fake", scripts["fake"], "Ready?")
	launched, err := p.run.Launch(t.Context(), sj.id, "work")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.run.Send(t.Context(), sj.id, "go"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the agent to exit and its session to end", func() bool {
		f, err := p.run.Observe(t.Context(), launched)
		return err == nil && f.Exit != nil && !f.SessionAlive && f.GroupEmpty
	})
	// The stored Job still says running: nobody reconciled it. Stop must.
	if err := p.run.Stop(t.Context(), sj.id); !errors.Is(err, job.ErrInvalidTransition) {
		t.Errorf("stop = %v, want it refused: the agent is done", err)
	}
	if j, _ := p.store.Get(sj.id); j.State != job.Done {
		t.Errorf("state = %s, want done", j.State)
	}
}

func TestGivenAHostWithConfigDirsThenTheHarnessIsToldTheSameOnes(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a stub binary
			t.Fatal(err)
		}
	}
	home, claudeDir, codexDir := t.TempDir(), t.TempDir(), t.TempDir()
	host, err := localhost.New(localhost.Options{Home: home, Env: []string{
		"PATH=" + bin, claude.ConfigHomeEnv + "=" + claudeDir, codex.ConfigHomeEnv + "=" + codexDir,
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := runner.New(runner.Config{Host: host})
	worktree := t.TempDir()
	in := runner.Input{Worktree: worktree, LaunchDir: t.TempDir(), Waiting: "w", SessionFile: "s", NotifyCmd: []string{"n"}, Prompt: "p"}
	for harnessID, want := range map[string]string{claude.ID: claude.ConfigHomeEnv + "=" + claudeDir, codex.ID: codex.ConfigHomeEnv + "=" + codexDir} {
		plan, err := runner.PrepareFor(r, harnessID, in)
		if err != nil {
			t.Fatalf("%s: %v", harnessID, err)
		}
		for _, w := range []string{want, "HOME=" + home, "PATH=" + bin} {
			if !contains(plan.Env, w) {
				t.Errorf("%s: env %v lacks %s", harnessID, plan.Env, w)
			}
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// With CLAUDE_CONFIG_DIR set Claude Code keeps its global state inside it, so a
// caller that has not set it must not be given one: the harness would start as new.
func TestGivenAHostWithoutClaudeConfigDirThenTheHarnessIsNotGivenOne(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a stub binary
		t.Fatal(err)
	}
	host, err := localhost.New(localhost.Options{Home: t.TempDir(), Env: []string{"PATH=" + bin}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runner.PrepareFor(runner.New(runner.Config{Host: host}), claude.ID, runner.Input{Worktree: t.TempDir(), LaunchDir: t.TempDir(), Waiting: "w", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range plan.Env {
		if strings.HasPrefix(e, claude.ConfigHomeEnv+"=") {
			t.Errorf("env %v sets CLAUDE_CONFIG_DIR", plan.Env)
		}
	}
}
