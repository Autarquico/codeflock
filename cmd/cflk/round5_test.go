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
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
)

func TestCleanLooksAtEveryGenerationThisPeerKept(t *testing.T) {
	back := func(t *testing.T) (*moveNet, cli.JobView) {
		n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
		v := n.started(t, "laptop")
		n.writeInto(t, "laptop", v, "notes.txt", "sealed")
		mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, To: "mini"})
		mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID})
		n.done(t, "laptop", v.ID)
		runGit(t, n.peers["laptop"].store().WorktreeDir(v.ID, 3), "branch", "retrieved", "HEAD")
		return n, v
	}
	t.Run("a file made in an older generation after it was sealed", func(t *testing.T) {
		n, v := back(t)
		old := n.peers["laptop"].store().WorktreeDir(v.ID, 1)
		if err := os.WriteFile(filepath.Join(old, "late.txt"), []byte("only here"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := n.fleet("laptop").Clean(t.Context(), v.ID, false)
		requireCode(t, err, domain.CodeInvalidTransition)
		if err := os.Remove(filepath.Join(old, "late.txt")); err != nil {
			t.Fatal(err)
		}
		if _, err := n.fleet("laptop").Clean(t.Context(), v.ID, false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a change that is staged and whose file looks as it was sealed", func(t *testing.T) {
		n, v := back(t)
		old := n.peers["laptop"].store().WorktreeDir(v.ID, 1)
		file := filepath.Join(old, "notes.txt")
		if err := os.WriteFile(file, []byte("staged only"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, old, "add", "notes.txt")
		if err := os.WriteFile(file, []byte("sealed"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := n.fleet("laptop").Clean(t.Context(), v.ID, false)
		requireCode(t, err, domain.CodeInvalidTransition)
	})
}

func TestARepeatedFinishOfADoneJobAnswersWithoutFetching(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	n.writeInto(t, "laptop", v, "notes.txt", "work")
	if done := mustPull(t, n.fleet("laptop"), cli.PullRequest{ID: v.ID, Finish: true}); done.State != "done" {
		t.Fatalf("finish = %+v", done)
	}
	if left := runGit(t, ".", "for-each-ref", "refs/cflk/"); left != "" {
		t.Fatalf("the seal ref is meant to be gone: %s", left)
	}
	again, err := n.fleet("laptop").Pull(t.Context(), cli.PullRequest{ID: v.ID, Finish: true})
	if err != nil || again.State != "done" {
		t.Fatalf("repeat = %+v, %v", again, err)
	}
}

func TestAnOwnerResolvesTheCallerByNodeIDThenByAnyOfItsNames(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.delegated(t, "mini")
	n.writeInto(t, "mini", v, "notes.txt", "work")

	// Nothing is stopped or sealed for a caller the owner cannot place.
	_, err := n.peers["mini"].finish(t.Context(), server.FinishInput{Job: v.ID, Gen: v.Gen, To: "nzzz", ToNames: []string{"nobody"}})
	requireCode(t, err, domain.CodeInvalidRequest)
	if got := n.record(t, "mini", v.ID); got.State == job.Stopped || got.State == job.Finishing {
		t.Fatalf("the Job was touched: %s", got.State)
	}
	info, err := n.peers["mini"].finish(t.Context(), server.FinishInput{Job: v.ID, Gen: v.Gen, To: "nzzz", ToNames: []string{"nobody", "laptop"}})
	if err != nil || info.ID == "" {
		t.Fatalf("finish by a known name = %+v, %v", info, err)
	}
}
