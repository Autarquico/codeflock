//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/server"
	"github.com/Autarquico/codeflock/internal/peer/wire"
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
)

// delegatedAs is delegated to a peer the tailnet names differently from its hostname.
func (n *moveNet) delegatedAs(t *testing.T, peer, tailnet string) cli.JobView {
	t.Helper()
	v, err := n.fleet("laptop").Run(t.Context(), cli.RunRequest{On: tailnet, Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the Job runs", func() bool { return n.record(t, peer, v.ID).SessionID != "" })
	return v
}

func TestAPeerIsKnownToTheOthersByItsNodeIDWhateverItsHostnamesAre(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	n.tailnetName("laptop", "Alejandros-MBP")
	n.tailnetName("mini", "mac-mini-ts")
	v := n.delegatedAs(t, "mini", "mac-mini-ts")
	n.writeInto(t, "mini", v, "notes.txt", "work")

	done := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID, Finish: true})
	if done.State != "done" {
		t.Fatalf("finish = %+v", done)
	}
	if got := runGit(t, ".", "show", "cflk/"+v.ID+":notes.txt"); got != "work" {
		t.Fatalf("the branch lacks the work: %q", got)
	}
	w := n.delegatedAs(t, "mini", "mac-mini-ts")
	if pulled := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: w.ID}); pulled.State != "moved" || pulled.MovedTo != "Alejandros-MBP" {
		t.Fatalf("pull = %+v", pulled)
	}
}

func TestAFailedPushOfALocalFinishIsRetriedFromTheSameSeal(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop")
	v := n.started(t, "laptop")
	n.writeInto(t, "laptop", v, "notes.txt", "to publish")
	bare := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, ".", "init", "-q", "--bare", bare)
	runGit(t, ".", "remote", "set-url", "--push", "origin", filepath.Join(t.TempDir(), "nowhere.git"))

	if _, err := n.fleet("laptop").Pull(t.Context(), cli.PullRequest{ID: v.ID, Finish: true, Push: true}); err == nil {
		t.Fatal("the push to nowhere worked")
	}
	if got := n.record(t, "laptop", v.ID); got.State != job.Finishing {
		t.Fatalf("job = %s", got.State)
	}
	runGit(t, ".", "remote", "set-url", "--push", "origin", bare)
	done := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID, Finish: true, Push: true})
	branch := "cflk/" + v.ID
	if done.State != "done" || runGit(t, bare, "rev-parse", branch) != runGit(t, ".", "rev-parse", branch) {
		t.Fatalf("done = %+v", done)
	}
	if left := runGit(t, ".", "for-each-ref", "refs/cflk/"+v.ID+"/"); left != "" {
		t.Fatalf("refs left: %s", left)
	}
}

func TestARetryOrAbortFromAThirdPeerGoesToThePeerThatHoldsTheMove(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini", "desk")
	v := n.started(t, "laptop")
	n.cutNext("mini", wire.OpActivate, true)
	_, _ = n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"})
	if got := n.record(t, "laptop", v.ID); got.State != job.Moving {
		t.Fatalf("source = %s", got.State)
	}

	moved := mustMove(t, n.fleet("desk"), cli.MoveRequest{ID: v.ID, Retry: true})
	if moved.State != "moved" || moved.MovedTo != "mini" || n.launchesOn("mini") != 1 {
		t.Fatalf("retry = %+v, launches %v", moved, n.launches())
	}
}

func TestAReplayedAbortOfAnOldTransferLeavesTheWorktreeOfTheNewOne(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	n.cutNext("mini", wire.OpActivate, false)
	_, _ = n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"})
	old := n.record(t, "laptop", v.ID).Move.Transfer
	if _, err := n.fleet("laptop").ResumeForce(t.Context(), v.ID); err != nil {
		t.Fatal(err)
	}
	n.cutNext("mini", wire.OpActivate, false)
	_, _ = n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"})

	if _, err := n.peers["mini"].abortTransfer(t.Context(), server.TransferInput{Job: v.ID, Gen: 2, Transfer: old}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(n.peers["mini"].store().WorktreeDir(v.ID, 2)); err != nil {
		t.Fatalf("the worktree of the new transfer is gone: %v", err)
	}
	if moved := mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, Retry: true}); moved.State != "moved" {
		t.Fatalf("retry = %+v", moved)
	}
}

func TestAnAbortOfAFinishNeedsTheFinishItGivesUp(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.delegated(t, "mini")
	n.cutNext("mini", wire.OpFinishDone, false)
	_, _ = n.fleet("laptop").Pull(t.Context(), cli.PullRequest{ID: v.ID, Finish: true})

	_, err := n.peers["mini"].finishAbort(t.Context(), server.FinishNotice{Job: v.ID, Gen: 1})
	requireCode(t, err, domain.CodeInvalidTransition)
	if got := n.record(t, "mini", v.ID); got.State != job.Finishing {
		t.Fatalf("owner = %s", got.State)
	}
}

func (n *moveNet) done(t *testing.T, peer, id string) {
	t.Helper()
	p, _ := n.peers[peer].open()
	if err := p.runner.Stop(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := p.store.WithLock(id, func(j *job.Job) error { j.State = job.Done; return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestCleanKeepsWhatNothingElseHolds(t *testing.T) {
	t.Run("a moved Job is refused for changes made after it was sealed, not for what travelled", func(t *testing.T) {
		n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
		v := n.started(t, "laptop")
		n.writeInto(t, "laptop", v, "notes.txt", "travelled")
		mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, To: "mini"})
		n.writeInto(t, "laptop", v, "late.txt", "after")

		_, err := n.fleet("laptop").Clean(t.Context(), v.ID, false)
		requireCode(t, err, domain.CodeInvalidTransition)
		if err := os.Remove(filepath.Join(n.peers["laptop"].store().WorktreeDir(v.ID, 1), "late.txt")); err != nil {
			t.Fatal(err)
		}
		if _, err := n.fleet("laptop").Clean(t.Context(), v.ID, false); err != nil {
			t.Fatalf("what travelled is not lost: %v", err)
		}
	})
	t.Run("a Job back where it began is not held by the files it left in an older generation", func(t *testing.T) {
		n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
		v := n.started(t, "laptop")
		n.writeInto(t, "laptop", v, "notes.txt", "travelled")
		mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, To: "mini"})
		mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID})
		n.done(t, "laptop", v.ID)
		_, err := n.fleet("laptop").Clean(t.Context(), v.ID, false)
		requireCode(t, err, domain.CodeInvalidTransition) // the seal commit lives in this peer's mirror only
		runGit(t, n.peers["laptop"].store().WorktreeDir(v.ID, 3), "branch", "retrieved", "HEAD")

		if _, err := n.fleet("laptop").Clean(t.Context(), v.ID, false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a Job that ended by itself needs its commits in a branch that survives", func(t *testing.T) {
		n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
		v := n.delegated(t, "mini")
		wt := n.peers["mini"].store().WorktreeDir(v.ID, 1)
		runGit(t, wt, "commit", "--allow-empty", "-m", "unpublished")
		n.done(t, "mini", v.ID)

		_, err := n.fleet("mini").Clean(t.Context(), v.ID, false)
		requireCode(t, err, domain.CodeInvalidTransition)
		runGit(t, wt, "branch", "keep", "HEAD")
		if _, err := n.fleet("mini").Clean(t.Context(), v.ID, false); err != nil {
			t.Fatalf("commits in a branch: %v", err)
		}
	})
	t.Run("force deletes it all", func(t *testing.T) {
		n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
		v := n.delegated(t, "mini")
		n.writeInto(t, "mini", v, "notes.txt", "mine")
		n.done(t, "mini", v.ID)

		if _, err := n.fleet("mini").Clean(t.Context(), v.ID, false); err == nil {
			t.Fatal("uncommitted work was deleted")
		}
		if _, err := n.fleet("mini").Clean(t.Context(), v.ID, true); err != nil {
			t.Fatal(err)
		}
	})
}

func TestADirtySubmoduleIsSeenWhateverItsChange(t *testing.T) {
	sub := t.TempDir()
	runGit(t, sub, "init", "-q")
	if err := os.WriteFile(filepath.Join(sub, "f.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, sub, "add", ".")
	runGit(t, sub, "commit", "-qm", "sub")
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sp ace")
	runGit(t, repo, "commit", "-qm", "with a submodule")
	if err := cleanSubmodules(t.Context(), repo); err != nil {
		t.Fatalf("a clean submodule: %v", err)
	}
	for name, change := range map[string]func(){
		"modified":  func() { _ = os.WriteFile(filepath.Join(repo, "sp ace", "f.txt"), []byte("two"), 0o600) },
		"untracked": func() { _ = os.WriteFile(filepath.Join(repo, "sp ace", "new.txt"), []byte("x"), 0o600) },
	} {
		runGit(t, filepath.Join(repo, "sp ace"), "checkout", "-q", ".")
		runGit(t, filepath.Join(repo, "sp ace"), "clean", "-fdq")
		change()
		if err := cleanSubmodules(t.Context(), repo); codeOf(err) != domain.CodeInvalidTransition {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
