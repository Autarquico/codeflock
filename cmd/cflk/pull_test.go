//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/server"
	"github.com/Autarquico/codeflock/internal/peer/wire"
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
)

// delegated is a Job that laptop started on another peer: its branch is nowhere
// in the repository the commands run in, and its conversation is real.
func (n *moveNet) delegated(t *testing.T, on string) cli.JobView {
	t.Helper()
	v, err := n.fleet("laptop").Run(t.Context(), cli.RunRequest{On: on, Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the Job runs", func() bool { return n.record(t, on, v.ID).SessionID != "" })
	return v
}

func mustPull(t *testing.T, f *fleet, req cli.PullRequest) cli.JobView {
	t.Helper()
	v, err := f.Pull(t.Context(), req)
	if err != nil {
		t.Fatalf("pull %+v: %v", req, err)
	}
	return v
}

func (n *moveNet) writeInto(t *testing.T, peer string, v cli.JobView, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(n.peers[peer].store().WorktreeDir(v.ID, v.Gen), name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPullMovesTheJobToThisPeerWhereItKeepsRunning(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.delegated(t, "mini")

	pulled := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID})
	if pulled.State != "moved" || pulled.MovedTo != "laptop" {
		t.Fatalf("source view = %+v", pulled)
	}
	here := n.record(t, "laptop", v.ID)
	if here.Gen != 2 || here.Peer != "laptop" || here.State != job.Running && here.State != job.Waiting || n.launchesOn("laptop") != 1 {
		t.Fatalf("here = %+v, launches %v", here, n.launches())
	}
	_, err := n.fleet("laptop").Pull(t.Context(), cli.PullRequest{ID: v.ID})
	requireCode(t, err, domain.CodeInvalidRequest) // it is here already
}

func TestPullFinishBringsTheBranchWithoutTouchingTheCheckoutAndMarksTheJobDone(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.delegated(t, "mini")
	n.writeInto(t, "mini", v, "notes.txt", "half done")
	head := runGit(t, ".", "rev-parse", "HEAD")
	runGit(t, ".", "branch", "cflk/"+v.ID, head) // the Job's own earlier work: behind, so it advances

	done := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID, Finish: true})
	if done.State != "done" || done.Branch != "cflk/"+v.ID {
		t.Fatalf("view = %+v", done)
	}
	there := n.record(t, "mini", v.ID)
	if there.State != job.Done || there.Finish == nil || there.Launch == nil {
		t.Fatalf("owner = %+v", there)
	}
	if got := runGit(t, ".", "show", "cflk/"+v.ID+":notes.txt"); got != "half done" {
		t.Fatalf("the branch lacks the work: %q", got)
	}
	if tip := runGit(t, ".", "rev-parse", "cflk/"+v.ID); tip != there.Finish.Commit {
		t.Fatalf("branch at %s, sealed %s", tip, there.Finish.Commit)
	}
	if runGit(t, ".", "rev-parse", "HEAD") != head || runGit(t, ".", "branch", "--show-current") != "main" || runGit(t, ".", "status", "--porcelain") != "" {
		t.Fatal("the checkout changed")
	}
	if left := runGit(t, ".", "for-each-ref", "refs/cflk/"); left != "" {
		t.Fatalf("refs left in the repository: %s", left)
	}
	if log := runGit(t, ".", "log", "--all", "--oneline", "-S"+secretPlan); log != "" {
		t.Fatalf("the conversation is in git: %s", log)
	}
	_, err := n.fleet("mini").Resume(t.Context(), v.ID)
	requireCode(t, err, domain.CodeInvalidTransition) // a done Job does not come back
	again := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID, Finish: true})
	if again.State != "done" {
		t.Fatalf("a repeated finish = %+v", again)
	}
}

func TestPullFinishPushOnlyPublishesTheBranch(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.delegated(t, "mini")
	n.writeInto(t, "mini", v, "notes.txt", "to publish")
	bare := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, ".", "init", "-q", "--bare", bare)
	runGit(t, ".", "remote", "set-url", "--push", "origin", bare) // identity stays example.com/o/r

	mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID, Finish: true, Push: true})
	branch := "cflk/" + v.ID
	if got, want := runGit(t, bare, "rev-parse", branch), runGit(t, ".", "rev-parse", branch); got != want {
		t.Fatalf("origin has %s, the branch is %s", got, want)
	}
	if refs := runGit(t, bare, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/"+branch {
		t.Fatalf("origin refs = %q", refs)
	}
	if log := runGit(t, bare, "log", "--all", "--oneline", "-S"+secretPlan); log != "" {
		t.Fatalf("the conversation reached origin: %s", log)
	}
}

func TestAFinishAbortedGoesBackToStoppedAndTheJobCanRunAgain(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.delegated(t, "mini")
	n.cutNext("mini", wire.OpFinishDone, false)

	_, err := n.fleet("laptop").Pull(t.Context(), cli.PullRequest{ID: v.ID, Finish: true})
	if err == nil || !strings.Contains(err.Error(), "--abort") {
		t.Fatalf("err = %v", err)
	}
	if got := n.record(t, "mini", v.ID); got.State != job.Finishing || got.Finish == nil {
		t.Fatalf("owner = %+v", got)
	}
	// While it finishes it neither resumes nor moves.
	_, err = n.fleet("mini").Resume(t.Context(), v.ID)
	requireCode(t, err, domain.CodeInvalidTransition)
	_, err = n.fleet("mini").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "laptop"})
	requireCode(t, err, domain.CodeInvalidTransition)

	stopped := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID, Finish: true, Abort: true})
	if stopped.State != "stopped" {
		t.Fatalf("abort left %s", stopped.State)
	}
	if got := n.record(t, "mini", v.ID); got.Finish != nil {
		t.Fatalf("the finish is still recorded: %+v", got.Finish)
	}
	if _, err := n.fleet("mini").Resume(t.Context(), v.ID); err != nil {
		t.Fatalf("resume after the abort: %v", err)
	}
	_, err = n.fleet("laptop").Pull(t.Context(), cli.PullRequest{ID: v.ID, Finish: true, Abort: true})
	requireCode(t, err, domain.CodeInvalidTransition) // nothing to give up now
}

func TestAFinishNoticeOrAbortOfAnotherFinishIsRefused(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.delegated(t, "mini")
	n.cutNext("mini", wire.OpFinishDone, false)
	_, _ = n.fleet("laptop").Pull(t.Context(), cli.PullRequest{ID: v.ID, Finish: true})
	mini := n.peers["mini"]
	other := server.FinishNotice{Job: v.ID, Gen: 1, Finish: "aaaaaaaaaaaaaaaa"}

	_, err := mini.finishDone(t.Context(), other)
	requireCode(t, err, domain.CodeInvalidTransition)
	_, err = mini.finishAbort(t.Context(), other)
	requireCode(t, err, domain.CodeInvalidTransition)
	if got := n.record(t, "mini", v.ID); got.State != job.Finishing {
		t.Fatalf("owner = %s", got.State)
	}
	own := server.FinishNotice{Job: v.ID, Gen: 1, Finish: n.record(t, "mini", v.ID).Finish.ID}
	if _, err := mini.finishDone(t.Context(), own); err != nil {
		t.Fatal(err)
	}
	if _, err := mini.finishDone(t.Context(), own); err != nil {
		t.Fatalf("the same notice twice: %v", err)
	}
	_, err = mini.finishAbort(t.Context(), own)
	requireCode(t, err, domain.CodeInvalidTransition) // a late abort of a finished Job
}

func TestAPeerThatDoesNotAnswerMidFinishLeavesTheJobFinishingAndARetryEndsIt(t *testing.T) {
	for name, cut := range map[string]struct {
		op    wire.Op
		after bool
	}{
		"the finish is not heard":          {wire.OpFinish, false},
		"the answer to the finish is lost": {wire.OpFinish, true},
		"the notice is not heard":          {wire.OpFinishDone, false},
		"the notice is heard, ack lost":    {wire.OpFinishDone, true},
	} {
		t.Run(name, func(t *testing.T) {
			n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
			v := n.delegated(t, "mini")
			n.writeInto(t, "mini", v, "notes.txt", "work")
			n.cutNext("mini", cut.op, cut.after)

			_, err := n.fleet("laptop").Pull(t.Context(), cli.PullRequest{ID: v.ID, Finish: true})
			if err == nil || !strings.Contains(err.Error(), "pull "+v.ID+" --finish") {
				t.Fatalf("err = %v", err)
			}
			first := n.record(t, "mini", v.ID)
			if first.Finish != nil && first.State != job.Finishing && first.State != job.Done {
				t.Fatalf("owner = %+v", first)
			}

			done := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID, Finish: true})
			last := n.record(t, "mini", v.ID)
			if done.State != "done" || last.State != job.Done || last.Finish == nil {
				t.Fatalf("after the retry: %+v / %+v", done, last)
			}
			if first.Finish != nil && first.Finish.ID != last.Finish.ID {
				t.Fatalf("the retry sealed again: %s then %s", first.Finish.ID, last.Finish.ID)
			}
			if runGit(t, ".", "rev-parse", "cflk/"+v.ID) != last.Finish.Commit {
				t.Fatal("the branch is not the sealed commit")
			}
		})
	}
}

func TestAnAbortThatTheOwnerDoesNotConfirmChangesNothing(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.delegated(t, "mini")
	n.cutNext("mini", wire.OpFinishDone, false)
	_, _ = n.fleet("laptop").Pull(t.Context(), cli.PullRequest{ID: v.ID, Finish: true})
	n.cutNext("mini", wire.OpFinishAbort, false)

	_, err := n.fleet("laptop").Pull(t.Context(), cli.PullRequest{ID: v.ID, Finish: true, Abort: true})
	if err == nil || !strings.Contains(err.Error(), "abort is refused") {
		t.Fatalf("err = %v", err)
	}
	if got := n.record(t, "mini", v.ID); got.State != job.Finishing {
		t.Fatalf("owner = %s", got.State)
	}
}

func TestAFinishFromAnotherRepositoryOrOverOtherWorkStopsNothingOrKeepsTheJobFinishing(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.delegated(t, "mini")
	home, _ := os.Getwd()

	other := t.TempDir()
	runGit(t, other, "init", "-q", "-b", "main")
	runGit(t, other, "remote", "add", "origin", "https://example.com/o/another.git")
	t.Chdir(other)
	_, err := n.fleet("laptop").Pull(t.Context(), cli.PullRequest{ID: v.ID, Finish: true})
	requireCode(t, err, domain.CodeInvalidRequest)
	if got := n.record(t, "mini", v.ID); got.State == job.Stopped || got.State == job.Finishing {
		t.Fatalf("the Job was stopped by a finish that was refused: %s", got.State)
	}
	t.Chdir(home)

	// A branch of that name with work the Job does not have.
	branch := "cflk/" + v.ID
	runGit(t, ".", "switch", "-q", "-c", "side")
	runGit(t, ".", "commit", "-q", "--allow-empty", "-m", "elsewhere")
	runGit(t, ".", "branch", branch)
	runGit(t, ".", "switch", "-q", "main")
	_, err = n.fleet("laptop").Pull(t.Context(), cli.PullRequest{ID: v.ID, Finish: true})
	requireCode(t, err, domain.CodeInvalidTransition)
	if got := n.record(t, "mini", v.ID); got.State != job.Finishing {
		t.Fatalf("owner = %s", got.State)
	}
	runGit(t, ".", "branch", "-q", "-D", branch)
	if done := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID, Finish: true}); done.State != "done" {
		t.Fatalf("after clearing the branch: %+v", done)
	}
}

func TestAFinishOfAJobHereTakesItsBranchFromItsWorktree(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.delegated(t, "mini")
	mustMove(t, n.fleet("mini"), cli.MoveRequest{ID: v.ID, To: "laptop"}) // now it is here, in the mirror of laptop
	n.writeInto(t, "laptop", cli.JobView{ID: v.ID, Gen: 2}, "here.txt", "local")

	done := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID, Finish: true})
	if done.State != "done" || runGit(t, ".", "show", "cflk/"+v.ID+":here.txt") != "local" {
		t.Fatalf("view = %+v", done)
	}
}

func TestAJobBroughtBackToWhereItStartedFinishesDetachingItsOwnOldWorktree(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, To: "mini"})
	n.writeInto(t, "mini", cli.JobView{ID: v.ID, Gen: 2}, "notes.txt", "from the mini")
	old := n.peers["laptop"].store().WorktreeDir(v.ID, 1)
	branch := "cflk/" + v.ID
	if runGit(t, old, "branch", "--show-current") != branch {
		t.Fatal("the first worktree should have the branch checked out")
	}

	done := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID, Finish: true})
	if done.State != "done" {
		t.Fatalf("view = %+v", done)
	}
	sealed := n.record(t, "mini", v.ID).Finish.Commit
	if tip := runGit(t, ".", "rev-parse", branch); tip != sealed {
		t.Fatalf("branch at %s, sealed %s", tip, sealed)
	}
	if runGit(t, old, "branch", "--show-current") != "" {
		t.Fatal("the old worktree still has the branch")
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("the old worktree is gone: %v", err)
	}
}

func TestALocalFinishOfADirtyJobSealsItsChangesIntoTheBranch(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop")
	v := n.started(t, "laptop")
	n.writeInto(t, "laptop", v, "notes.txt", "uncommitted")

	done := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID, Finish: true})
	if done.State != "done" {
		t.Fatalf("view = %+v", done)
	}
	branch := "cflk/" + v.ID
	if got := runGit(t, ".", "show", branch+":notes.txt"); got != "uncommitted" {
		t.Fatalf("the branch lacks the work: %q", got)
	}
	if tip, sealed := runGit(t, ".", "rev-parse", branch), n.record(t, "laptop", v.ID).Finish.Commit; tip != sealed {
		t.Fatalf("branch at %s, sealed %s", tip, sealed)
	}
	if left := runGit(t, ".", "for-each-ref", "refs/cflk/"); left != "" {
		t.Fatalf("refs left: %s", left)
	}
}

func TestAFinishRefusedByPlaceKeepsTheSealedWorkAndTheRetryFinishesWithTheSameId(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop")
	v := n.started(t, "laptop")
	n.writeInto(t, "laptop", v, "notes.txt", "work")
	branch := "cflk/" + v.ID
	// The user's own worktree has the branch: not the Job's, so it is never touched.
	runGit(t, n.peers["laptop"].store().WorktreeDir(v.ID, 1), "switch", "--detach")
	theirs := filepath.Join(t.TempDir(), "theirs")
	runGit(t, ".", "worktree", "add", theirs, branch)

	_, err := n.fleet("laptop").Pull(t.Context(), cli.PullRequest{ID: v.ID, Finish: true})
	if err == nil || !strings.Contains(err.Error(), "checked out") {
		t.Fatalf("first finish = %v", err)
	}
	first := n.record(t, "laptop", v.ID)
	if first.State != job.Finishing {
		t.Fatalf("state = %s", first.State)
	}
	runGit(t, ".", "worktree", "remove", "--force", theirs)

	done := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID, Finish: true})
	if done.State != "done" || n.record(t, "laptop", v.ID).Finish.ID != first.Finish.ID {
		t.Fatalf("retry = %+v", done)
	}
	if got := runGit(t, ".", "show", branch+":notes.txt"); got != "work" {
		t.Fatalf("the branch lacks the work: %q", got)
	}
}

func TestCleanRefusesADoneJobThatEndedByItselfWithUnsealedChanges(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop")
	v := n.started(t, "laptop")
	p, _ := n.peers["laptop"].open()
	if err := p.runner.Stop(t.Context(), v.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.store.WithLock(v.ID, func(j *job.Job) error { j.State = job.Done; return nil }); err != nil {
		t.Fatal(err)
	}
	n.writeInto(t, "laptop", v, "notes.txt", "mine")

	_, err := n.fleet("laptop").Clean(t.Context(), v.ID, false)
	requireCode(t, err, domain.CodeInvalidTransition)
	if _, err := os.Stat(filepath.Join(p.store.WorktreeDir(v.ID, 1), "notes.txt")); err != nil {
		t.Fatalf("the file is gone: %v", err)
	}
	if _, err := n.fleet("laptop").Clean(t.Context(), v.ID, true); err != nil {
		t.Fatalf("forced clean: %v", err)
	}
}

func TestMovableRefusesAJobWithoutASessionBeforeAnythingStops(t *testing.T) {
	err := movable(job.Job{ID: "job-aaaaaaaaaa", State: job.Running, Gen: 1}, 1)
	requireCode(t, err, domain.CodeInvalidTransition)
}
