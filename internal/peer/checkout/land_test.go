package checkout_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/peer/checkout"
)

const own = "cflk/job-aaaaaaaaaa"

func TestLand_Clean(t *testing.T) {
	t.Run("the branch the Job started from advances", func(t *testing.T) {
		w := newWorld(t)
		commitFile(t, w.worker, "feature.go", "package x\n", "feature")
		req := w.request()
		req.Source = "main"

		res, err := checkout.Land(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Branch != "main" || res.Outcome != checkout.Advanced || !res.CheckedOut {
			t.Fatalf("result = %+v", res)
		}
		if w.head() != req.Commit || w.branch() != "main" {
			t.Fatalf("main is at %s on %s, want %s", w.head(), w.branch(), req.Commit)
		}
		if _, err := os.Stat(filepath.Join(w.mac, "feature.go")); err != nil {
			t.Fatalf("the work is not in the tree: %v", err)
		}
	})

	t.Run("without a branch to advance the work goes to the Job's own", func(t *testing.T) {
		w := newWorld(t)
		work := commitFile(t, w.worker, "feature.go", "package x\n", "feature")
		res, err := checkout.Land(t.Context(), w.request())
		if err != nil {
			t.Fatal(err)
		}
		if res.Branch != own || res.Outcome != checkout.Created || w.branch() != own || w.head() != work {
			t.Fatalf("result = %+v, head %s on %s", res, w.head(), w.branch())
		}
		if got := w.tip("main"); got != w.base {
			t.Fatalf("main moved to %s", got)
		}
	})
}

// A ref that does not hold the commit that was expected changes nothing: not a branch, not the tree.
func TestLand_RefusesWhatIsNotTheExpectedCommit(t *testing.T) {
	w := newWorld(t)
	commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	req := w.request()
	req.Commit = w.base

	if _, err := checkout.Land(t.Context(), req); !errors.Is(err, checkout.ErrCommitMismatch) {
		t.Fatalf("another commit under the ref: %v", err)
	}
	if branchExists(t, w.mac, own) || w.head() != w.base {
		t.Fatal("something changed for a commit that is not the expected one")
	}
	for _, bad := range []string{"main", "HEAD", "", w.base[:12]} {
		req.Commit = bad
		if _, err := checkout.Land(t.Context(), req); err == nil {
			t.Errorf("commit %q was taken as an exact commit", bad)
		}
	}
	req.Commit = w.base
	req.Ref = "--output=/tmp/x"
	if _, err := checkout.Land(t.Context(), req); err == nil {
		t.Error("an option was taken as a ref")
	}
}

func TestLand_SourceBranchThatIsNotAnAncestorKeepsItsCommits(t *testing.T) {
	w := newWorld(t)
	local := commitFile(t, w.mac, "mine.txt", "only here\n", "local work on main")
	commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	req := w.request()
	req.Source = "main"
	res, err := checkout.Land(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != own || w.tip("main") != local {
		t.Fatalf("branch %s, main at %s want %s", res.Branch, w.tip("main"), local)
	}
}

func TestLand_DirtyTreeIsLeftAlone(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.mac, "README"), "edited here\n")
	write(t, filepath.Join(w.mac, "scratch.txt"), "untracked\n")
	statusBefore := gitIn(t, w.mac, "status", "--porcelain")
	work := commitFile(t, w.worker, "feature.go", "package x\n", "feature")

	res, err := checkout.Land(t.Context(), w.request())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Dirty || res.CheckedOut || res.Branch != own || w.tip(own) != work {
		t.Fatalf("result = %+v", res)
	}
	if w.branch() != "main" || gitIn(t, w.mac, "status", "--porcelain") != statusBefore {
		t.Fatalf("the tree changed: on %s\n%s", w.branch(), gitIn(t, w.mac, "status", "--porcelain"))
	}
	if raw, _ := os.ReadFile(filepath.Join(w.mac, "README")); string(raw) != "edited here\n" {
		t.Fatalf("README = %q", raw)
	}
	if gitIn(t, w.mac, "stash", "list") != "" {
		t.Fatal("something was stashed")
	}
}

func TestLand_Worktree(t *testing.T) {
	w := newWorld(t)
	write(t, filepath.Join(w.mac, "README"), "edited here\n")
	work := commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	target := filepath.Join(filepath.Dir(w.mac), "x")
	req := w.request()
	req.Worktree = "../x"

	dest, err := checkout.Destination(req)
	if err != nil || dest != target {
		t.Fatalf("destination = %q, %v", dest, err)
	}
	res, err := checkout.Land(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Worktree || res.Dir != target || gitIn(t, target, "rev-parse", "HEAD") != work {
		t.Fatalf("result = %+v", res)
	}
	if raw, _ := os.ReadFile(filepath.Join(w.mac, "README")); string(raw) != "edited here\n" {
		t.Fatalf("README = %q", raw)
	}
	if _, err := checkout.Land(t.Context(), req); !errors.Is(err, checkout.ErrWorktreeExists) {
		t.Fatalf("a path that exists: %v", err)
	}
}

func TestLand_DivergentLocalBranchSendsTheWorkToTheAlternative(t *testing.T) {
	w := newWorld(t)
	gitIn(t, w.mac, "switch", "-q", "-c", own)
	mine := commitFile(t, w.mac, "mine.txt", "mine\n", "my commit")
	gitIn(t, w.mac, "switch", "-q", "main")
	work := commitFile(t, w.worker, "feature.go", "package x\n", "feature")

	res, err := checkout.Land(t.Context(), w.request())
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != own+"@g1" || w.tip(own+"@g1") != work || w.tip(own) != mine {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(strings.Join(res.Warnings, "|"), "local branch "+own+" has commits") {
		t.Fatalf("no warning: %+v", res.Warnings)
	}
}

func TestLand_WhenEveryBranchHasCommitsOfItsOwnNothingChanges(t *testing.T) {
	w := newWorld(t)
	for _, b := range []string{own, own + "@g1"} {
		gitIn(t, w.mac, "switch", "-q", "-c", b)
		commitFile(t, w.mac, b+".txt", "mine\n", "my commit on "+b)
	}
	gitIn(t, w.mac, "switch", "-q", "main")
	commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	req := w.request()
	head, tips := w.head(), [2]string{w.tip(own), w.tip(own + "@g1")}

	_, err := checkout.Land(t.Context(), req)
	if !errors.Is(err, checkout.ErrBranchDiverged) {
		t.Fatalf("err = %v", err)
	}
	if w.head() != head || w.branch() != "main" || w.tip(own) != tips[0] || w.tip(own+"@g1") != tips[1] {
		t.Errorf("something changed: head %s branch %s", w.head(), w.branch())
	}
}

func TestLand_WhenTheOnlyCandidateIsMissingThereIsNoBranch(t *testing.T) {
	w := newWorld(t)
	commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	req := w.request()
	req.Source, req.Branch, req.Alt = "no-such-branch", "", ""

	if _, err := checkout.Land(t.Context(), req); !errors.Is(err, checkout.ErrNoBranch) {
		t.Fatalf("err = %v", err)
	}
}

func TestLand_NotAGitRepository(t *testing.T) {
	w := newWorld(t)
	req := w.request()
	req.Dir = t.TempDir()
	if _, err := checkout.Land(t.Context(), req); !errors.Is(err, checkout.ErrNotRepo) {
		t.Fatalf("err = %v", err)
	}
}

// What a Job commits cannot run code here: a tracked core.hooksPath is not honoured.
func TestLand_AHookOfTheCheckedOutTreeDoesNotRun(t *testing.T) {
	for name, worktree := range map[string]string{"switch": "", "worktree": "../hooked"} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			marker := filepath.Join(t.TempDir(), "ran")
			gitIn(t, w.mac, "config", "core.hooksPath", ".hooks")
			hook := filepath.Join(w.worker, ".hooks", "post-checkout")
			write(t, hook, "#!/bin/sh\ntouch '"+marker+"'\n")
			if err := os.Chmod(hook, 0o700); err != nil { //nolint:gosec // a hook must be executable
				t.Fatal(err)
			}
			gitIn(t, w.worker, "add", "-A")
			gitIn(t, w.worker, "commit", "-q", "-m", "hook")
			req := w.request()
			req.Worktree = worktree
			if _, err := checkout.Land(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the post-checkout hook of the pulled tree ran")
			}
		})
	}
}

// A branch that is at the commit but checked out in another worktree is not switched to:
// the work goes to the next candidate.
func TestLand_ABranchAtTheCommitThatAnotherWorktreeHasCheckedOutIsSkipped(t *testing.T) {
	w := newWorld(t)
	work := commitFile(t, w.worker, "feature.go", "package x\n", "feature")
	req := w.request()
	gitIn(t, w.mac, "branch", own, work)
	gitIn(t, w.mac, "worktree", "add", "-q", filepath.Join(filepath.Dir(w.mac), "other"), own)

	res, err := checkout.Land(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != own+"@g1" || w.head() != work {
		t.Fatalf("branch %q, head %s", res.Branch, w.head())
	}
}

// An ignored file that the work versions, or whose path collides with one it versions (a
// file where a directory is, or the other way round), is never overwritten by the switch or
// by the fast-forward: git would remove it without a word.
var collisions = map[string]struct{ ignore, local, remote string }{
	"the work versions the ignored file itself":                      {".env\n", ".env", ".env"},
	"the work versions a file where the ignored one has a directory": {"cache/credentials\n", "cache/credentials", "cache"},
	"the work versions a directory where the ignored one is a file":  {"cache\n", "cache", "cache/x"},
}

func collide(t *testing.T, ff bool, ignore, local, remote string) {
	t.Helper()
	w := newWorld(t)
	commitFile(t, w.mac, ".gitignore", ignore, "ignore")
	gitIn(t, w.worker, "pull", "-q", w.mac, "main")
	write(t, filepath.Join(w.worker, remote), "remote\n")
	gitIn(t, w.worker, "add", "-f", remote)
	gitIn(t, w.worker, "commit", "-q", "-m", "work")
	work := gitIn(t, w.worker, "rev-parse", "HEAD")
	req := w.request()
	before := w.tip("main")
	if ff {
		req.Source = "main"
	}
	write(t, filepath.Join(w.mac, local), "SECRET=local\n")

	res, err := checkout.Land(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.CheckedOut || w.branch() != "main" || w.tip("main") != before || w.tip(own) != work {
		t.Fatalf("on %s, main %s (was %s), result = %+v", w.branch(), w.tip("main"), before, res)
	}
	raw, _ := os.ReadFile(filepath.Join(w.mac, local)) //nolint:gosec // a path of the test
	if string(raw) != "SECRET=local\n" {
		t.Fatalf("%s = %q", local, raw)
	}
	if !slices.ContainsFunc(res.Warnings, func(s string) bool { return strings.Contains(s, "--worktree") && strings.Contains(s, local) }) {
		t.Fatalf("warnings = %q", res.Warnings)
	}
}

func TestLand_AnIgnoredPathIsNotOverwrittenByTheSwitch(t *testing.T) {
	for name, c := range collisions {
		t.Run(name, func(t *testing.T) { collide(t, false, c.ignore, c.local, c.remote) })
	}
}

func TestLand_AnIgnoredPathIsNotOverwrittenByTheFastForward(t *testing.T) {
	for name, c := range collisions {
		t.Run(name, func(t *testing.T) { collide(t, true, c.ignore, c.local, c.remote) })
	}
}
