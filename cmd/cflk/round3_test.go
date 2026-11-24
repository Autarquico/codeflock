//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/remote"
	"github.com/Autarquico/codeflock/internal/peer/runner"
	"github.com/Autarquico/codeflock/internal/peer/wire"
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
)

// pushedRun is the ask for a run whose code is already in the mirror of tp.
func pushedRun(t *testing.T, tp twoPeers, repo string) (wire.Request, string) {
	t.Helper()
	mirror, _, err := tp.call(t, wire.Request{Op: wire.OpMirror, Repo: "example.com/o/r"}, "")
	if err != nil {
		t.Fatal(err)
	}
	id := job.NewID()
	runGit(t, repo, "push", "-q", mirror.Mirror, "HEAD:refs/cflk/"+id+"/g1")
	return wire.Request{Op: wire.OpRun, Job: id, Harness: "claude", Repo: "example.com/o/r", Commit: runGit(t, repo, "rev-parse", "HEAD"), CreatedBy: "laptop"}, id
}

func TestRunsOfTheSameIdThatOverlapAnswerTheSameJobAndStartOneAgent(t *testing.T) {
	repo := repoWithOrigin(t)
	tp := newTwoPeers(t, neverEnds)
	run, id := pushedRun(t, tp, repo)
	const asks = 5
	answers := make([]wire.Response, asks)
	errs := make([]error, asks)
	var wg sync.WaitGroup
	for i := range asks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i], _, errs[i] = remote.Call(t.Context(), tp.dialer, tp.peer, run, []byte("work"))
		}()
	}
	wg.Wait()
	for i := range asks {
		if errs[i] != nil || answers[i].Job.ID != id || answers[i].Job.State != "running" {
			t.Errorf("ask %d: %+v, %v", i, answers[i].Job, errs[i])
		}
	}
	if jobs, _ := tp.remote.List(t.Context()); len(jobs) != 1 {
		t.Errorf("an agent per ask: %v", jobs)
	}
}

func TestARunFindsTheJobAnEarlierStartLeftWithNoLaunchAndStartsItAgain(t *testing.T) {
	repo := repoWithOrigin(t)
	tp := newTwoPeers(t, neverEnds)
	run, id := pushedRun(t, tp, repo)
	p, _ := tp.remote.open()
	if _, err := p.store.CreateAs(id, job.Job{Repo: run.Repo, Harness: harnessIDs["claude"], Gen: 1, Peer: "mac-mini", State: job.Stopped, History: []job.Entry{{Gen: 1, Peer: "mac-mini", Commit: run.Commit}}}); err != nil {
		t.Fatal(err)
	}
	resp, _, err := remote.Call(t.Context(), tp.dialer, tp.peer, run, []byte("work"))
	if err != nil || resp.Job.ID != id || resp.Job.State != "running" {
		t.Fatalf("a run over a Job that never launched: %+v, %v", resp.Job, err)
	}
}

func TestAFailedWorktreeLeavesNoBranchAndNoRegistration(t *testing.T) {
	repo := repoWithOrigin(t)
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil { //nolint:gosec // a hook of a test
		t.Fatal(err)
	}
	l := peerWith(t, fake, neverEnds)
	if _, err := l.Run(t.Context(), cli.RunRequest{Prompt: "work"}); err == nil {
		t.Fatal("a worktree whose hook fails started a Job")
	}
	if branches := runGit(t, repo, "branch", "--list", "cflk/*"); branches != "" {
		t.Errorf("the branch stayed:\n%s", branches)
	}
	if lines := strings.Split(runGit(t, repo, "worktree", "list"), "\n"); len(lines) != 1 {
		t.Errorf("git still lists the worktree:\n%s", strings.Join(lines, "\n"))
	}
	if jobs, _ := l.List(t.Context()); len(jobs) != 0 {
		t.Errorf("Jobs left: %v", jobs)
	}
}

func TestAFailedRunWhoseJobStayedIsNotReportedAsRefused(t *testing.T) {
	tf := newTestFleet(t, []fakeharness.Step{neverEnds}, "mac-mini")
	serves := tf.dialer.Peers["mac-mini"]
	tf.dialer.Peers["mac-mini"] = func(ctx context.Context, in io.Reader, out io.Writer) error {
		req, body, err := wire.ReadRequest(in)
		if err != nil || req.Op != wire.OpRun {
			return serves(ctx, replay(t, req, body...), out)
		}
		_ = serves(ctx, replay(t, req, body...), io.Discard) // the Job is there, and running
		return wire.WriteResponse(out, wire.Response{Error: &wire.Error{Code: "CF_INTERNAL", Message: "launch failed (job kept)"}}, nil)
	}
	_, err := tf.Run(t.Context(), cli.RunRequest{On: "mac-mini", Prompt: "work"})
	if err == nil || strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "exists on the peer") || !strings.Contains(err.Error(), "cflk show job-") {
		t.Errorf("a run that left its Job: %v", err)
	}
}

func TestARefusedRunIsReportedAsRefusedWhenThePeerHasNoJob(t *testing.T) {
	tf := newTestFleet(t, []fakeharness.Step{neverEnds}, "mac-mini")
	serves := tf.dialer.Peers["mac-mini"]
	tf.dialer.Peers["mac-mini"] = func(ctx context.Context, in io.Reader, out io.Writer) error {
		req, body, err := wire.ReadRequest(in)
		if err != nil || req.Op != wire.OpRun {
			return serves(ctx, replay(t, req, body...), out)
		}
		return wire.WriteResponse(out, wire.Response{Error: &wire.Error{Code: "CF_HARNESS_UNAVAILABLE", Message: "no claude here"}}, nil)
	}
	_, err := tf.Run(t.Context(), cli.RunRequest{On: "mac-mini", Prompt: "work"})
	if err == nil || !strings.Contains(err.Error(), "the peer refused it") {
		t.Errorf("a refused run: %v", err)
	}
}

func TestRunWithAnIdRetriesTheDelegationOfThatJobAndTheFailureSaysHow(t *testing.T) {
	tf := newTestFleet(t, []fakeharness.Step{neverEnds}, "mac-mini")
	serves := tf.dialer.Peers["mac-mini"]
	lose := true
	tf.dialer.Peers["mac-mini"] = func(ctx context.Context, in io.Reader, out io.Writer) error {
		req, body, err := wire.ReadRequest(in)
		if err != nil || req.Op != wire.OpRun || !lose {
			return serves(ctx, replay(t, req, body...), out)
		}
		_ = serves(ctx, replay(t, req, body...), io.Discard)
		return errors.New("exit status 255: Connection closed by remote host")
	}
	_, err := tf.Run(t.Context(), cli.RunRequest{On: "mac-mini", Prompt: "work"})
	id := regexp.MustCompile(`job-[a-z2-7]{10}`).FindString(err.Error())
	if id == "" || !strings.Contains(err.Error(), "cflk run --id "+id+" --on mac-mini --harness claude --branch ") {
		t.Fatalf("the failure does not say how to ask again: %v", err)
	}
	lose = false
	v, err := tf.Run(t.Context(), cli.RunRequest{On: "mac-mini", Prompt: "work", ID: id})
	if err != nil || v.ID != id {
		t.Fatalf("the retry: %+v, %v", v, err)
	}
	if jobs, _ := tf.peers["mac-mini"].List(t.Context()); len(jobs) != 1 {
		t.Errorf("the retry started another agent: %+v", jobs)
	}
}

func TestAnIdOfARetryIsValidatedAndNeedsThePeerItWentTo(t *testing.T) {
	tf := newTestFleet(t, []fakeharness.Step{neverEnds}, "mac-mini")
	for name, req := range map[string]cli.RunRequest{
		"not an id":     {On: "mac-mini", ID: "job-$(id)", Prompt: "x"},
		"no peer":       {ID: "job-aaaaaaaaaa", Prompt: "x"},
		"any peer":      {On: "any", ID: "job-aaaaaaaaaa", Prompt: "x"},
		"this peer":     {On: "laptop", ID: "job-aaaaaaaaaa", Prompt: "x"},
		"option as id":  {On: "mac-mini", ID: "--on", Prompt: "x"},
		"uppercase id":  {On: "mac-mini", ID: "job-AAAAAAAAAA", Prompt: "x"},
		"too short id":  {On: "mac-mini", ID: "job-aaa", Prompt: "x"},
		"empty-ish id ": {On: "mac-mini", ID: " ", Prompt: "x"},
	} {
		if _, err := tf.Run(t.Context(), req); codeOf(err) != domain.CodeInvalidRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	if dialed := tf.dialer.Dialed; len(dialed) != 0 {
		t.Errorf("a refused id dialed a peer: %v", dialed)
	}
}

func TestPsReadsTheLocalJobsWithinItsBudgetEvenWhenAnOrderHoldsTheLockOfOne(t *testing.T) {
	tf := newTestFleet(t, []fakeharness.Step{neverEnds})
	here, err := tf.Run(t.Context(), cli.RunRequest{Prompt: "here"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := tf.local.open()
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.store.WithLock(here.ID, func(*job.Job) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	tf.budget = 300 * time.Millisecond
	start := time.Now()
	list, err := tf.List(t.Context(), "")
	close(release)
	<-done
	if took := time.Since(start); err != nil || took > 2*time.Second || len(list.Jobs) != 1 || list.Jobs[0].ID != here.ID || !strings.Contains(list.Jobs[0].Problem, "busy") {
		t.Fatalf("ps took %v: %+v, %v", took, list, err)
	}
}

func TestAStopIsGivenTheWindowOfTheStopperAndTheOtherOrdersTheUsualOne(t *testing.T) {
	f := &fleet{}
	if alone := runner.WrapperGrace + runner.GraceMargin + runner.DefaultKillWait; f.timeoutOf(wire.OpStop) <= alone {
		t.Errorf("a stop has %v, and the stopper alone may take %v", f.timeoutOf(wire.OpStop), alone)
	}
	if f.timeoutOf(wire.OpSend) != remote.DefaultOrderTimeout {
		t.Errorf("a send has %v", f.timeoutOf(wire.OpSend))
	}
	f.orderTimeout = time.Second
	if f.timeoutOf(wire.OpStop) != time.Second {
		t.Errorf("a configured order timeout is not honoured: %v", f.timeoutOf(wire.OpStop))
	}
}

func TestAWorktreeThatCouldNotRunLeavesABranchThatWasAlreadyThere(t *testing.T) {
	repo := repoWithOrigin(t)
	l := peerWith(t, fake, neverEnds)
	p, err := l.open()
	if err != nil {
		t.Fatal(err)
	}
	head := runGit(t, repo, "rev-parse", "HEAD")
	j := job.Job{ID: "job-aaaaaaaaaa", Branch: "cflk/job-aaaaaaaaaa", Gen: 1}
	runGit(t, repo, "branch", j.Branch, head)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.addWorktree(ctx, repo, j, head); err == nil {
		t.Fatal("a cancelled context created a worktree")
	}
	if got := runGit(t, repo, "branch", "--list", j.Branch); got == "" {
		t.Error("the branch that was already there was deleted")
	}
}
