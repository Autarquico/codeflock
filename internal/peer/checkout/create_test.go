package checkout_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codeflock/internal/peer/checkout"
)

func TestCreate_MakesTheBranchAtTheCommitAndChecksNothingOut(t *testing.T) {
	w := newWorld(t)
	commitFile(t, w.worker, "fix.go", "package fix\n", "the fix")
	req := w.request()

	got, err := checkout.Create(t.Context(), w.mac, req.Ref, req.Commit, own)
	if err != nil || got != checkout.Created || w.tip(own) != req.Commit {
		t.Fatalf("create = %q, %v", got, err)
	}
	if w.branch() != "main" || w.head() != w.base {
		t.Fatalf("the checkout moved: %s at %s", w.branch(), w.head())
	}
	again, err := checkout.Create(t.Context(), w.mac, req.Ref, req.Commit, own)
	if err != nil || again != checkout.Unchanged {
		t.Fatalf("second create = %q, %v", again, err)
	}
}

func TestCreate_NeverMovesABranchThatExists(t *testing.T) {
	w := newWorld(t)
	commitFile(t, w.worker, "fix.go", "package fix\n", "the fix")
	req := w.request()
	gitIn(t, w.mac, "branch", "mine")

	if _, err := checkout.Create(t.Context(), w.mac, req.Ref, req.Commit, "mine"); !errors.Is(err, checkout.ErrBranchExists) || w.tip("mine") != w.base {
		t.Fatalf("create onto an existing branch: %v; mine at %s", err, w.tip("mine"))
	}
}

func TestCreate_RefusesWhatIsNotTheExpectedCommit(t *testing.T) {
	w := newWorld(t)
	commitFile(t, w.worker, "fix.go", "package fix\n", "the fix")
	req := w.request()

	if _, err := checkout.Create(t.Context(), w.mac, req.Ref, w.base, own); !errors.Is(err, checkout.ErrCommitMismatch) {
		t.Fatalf("another commit under the ref: %v", err)
	}
	if branchExists(t, w.mac, own) {
		t.Fatal("a branch was made for a commit that is not the expected one")
	}
}

func TestCreate_RefusesBranchNamesThatAreNotNewPlainBranches(t *testing.T) {
	w := newWorld(t)
	commitFile(t, w.worker, "fix.go", "package fix\n", "the fix")
	req := w.request()
	gitIn(t, w.mac, "checkout", "-q", "-b", "other")
	gitIn(t, w.mac, "checkout", "-q", "main")
	gitIn(t, w.mac, "branch", "-D", "other")

	for name, want := range map[string]error{
		"@{-1}": checkout.ErrInvalidBranch, "-f": checkout.ErrInvalidBranch,
		"bad..name": checkout.ErrInvalidBranch, "main": checkout.ErrBranchExists,
	} {
		if _, err := checkout.Create(t.Context(), w.mac, req.Ref, req.Commit, name); !errors.Is(err, want) {
			t.Errorf("branch %q: %v, want %v", name, err, want)
		}
		if branchExists(t, w.mac, "other") {
			t.Fatalf("branch %q created the branch it expands to", name)
		}
	}
}

func TestCreate_NeverMakesABranchThatHeadNamesWithoutACommit(t *testing.T) {
	w := newWorld(t)
	commitFile(t, w.worker, "fix.go", "package fix\n", "the fix")
	req := w.request()
	unborn := filepath.Join(filepath.Dir(w.mac), "unborn")
	gitIn(t, filepath.Dir(w.mac), "init", "-q", "-b", own, unborn)
	gitIn(t, unborn, "fetch", "-q", w.worker, "+HEAD:"+jobRef)

	if _, err := checkout.Create(t.Context(), unborn, req.Ref, req.Commit, own); !errors.Is(err, checkout.ErrBranchExists) {
		t.Fatalf("create onto the unborn branch of HEAD: %v", err)
	}
	if branchExists(t, unborn, own) {
		t.Fatal("the branch HEAD names was created under the checkout")
	}
}
