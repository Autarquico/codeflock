package checkout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Errors a caller can tell apart.
var (
	// ErrCommitMismatch is a ref that does not hold the commit that was expected.
	ErrCommitMismatch = errors.New("checkout: the ref does not hold the expected commit")
	// ErrInvalidBranch is a branch name git does not take as it is written.
	ErrInvalidBranch = errors.New("checkout: not a branch name git accepts")
	// ErrBranchDiverged is the last candidate branch having commits the work does not.
	ErrBranchDiverged = errors.New("checkout: the branch has commits that the work does not")
	// ErrBranchExists is a branch that exists at another commit and is not moved.
	ErrBranchExists = errors.New("checkout: the branch already exists at another commit")
	// ErrNoBranch is every candidate branch being unusable.
	ErrNoBranch = errors.New("checkout: no branch can take the work")
	// ErrWorktreeExists is a path for a worktree that is taken.
	ErrWorktreeExists = errors.New("checkout: the path for the worktree exists")
	// ErrNotRepo is a directory that is not in a git repository.
	ErrNotRepo = errors.New("checkout: this directory is not in a git repository")
)

var commitID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Request is what Land is asked for.
type Request struct {
	// Dir is a directory inside the repository.
	Dir string
	// Ref holds the commit in the repository already (a ref the fetch wrote).
	Ref string
	// Commit is the exact commit, as a full object id, that Ref must resolve to.
	Commit string
	// Source is the branch the work started from; it advances if it is behind. May be empty.
	Source string
	// Branch is the Job's own branch, and Alt where the work goes when both have
	// commits that it does not.
	Branch, Alt string
	// Worktree is where a new git worktree is made for the work; empty uses the checkout.
	Worktree string
}

// Result is what Land did.
type Result struct {
	Commit  string
	Branch  string
	Outcome Outcome
	// Dir is where the work is: the new worktree, or the checkout.
	Dir string
	// CheckedOut says that Dir has the branch checked out; false when the tree was dirty
	// or a switch would have overwritten ignored files.
	CheckedOut bool
	Worktree   bool
	Dirty      bool
	Warnings   []string
}

type lander struct {
	req           Request
	repo          repo
	dirty         bool
	worktreeAsked bool
	warnings      []string
}

func (l *lander) warn(format string, a ...any) {
	l.warnings = append(l.warnings, fmt.Sprintf(format, a...))
}

// Land puts the commit of req on a branch of the checkout and, when the tree allows
// it, checks it out. Everything that can refuse comes before anything changes.
func Land(ctx context.Context, req Request) (Result, error) {
	if err := checkInputs(req.Ref, req.Commit); err != nil {
		return Result{}, err
	}
	root, err := (repo{dir: req.Dir}).toplevel(ctx)
	if err != nil {
		return Result{}, err
	}
	l := &lander{req: req, repo: repo{dir: root}, worktreeAsked: req.Worktree != ""}
	dest, err := l.destination()
	if err != nil {
		return Result{}, err
	}
	if err := l.repo.expect(ctx, req.Ref, req.Commit); err != nil {
		return Result{}, err
	}
	if l.dirty, err = l.repo.dirty(ctx); err != nil {
		return Result{}, err
	}
	pl, err := l.place(ctx, req.Commit)
	if err != nil {
		return Result{}, err
	}
	if err := l.move(ctx, pl, req.Commit); err != nil {
		return Result{}, err
	}
	res := Result{Commit: req.Commit, Branch: pl.branch, Outcome: pl.outcome, Dir: l.repo.dir}
	switch {
	case l.worktreeAsked:
		if _, err := l.repo.run(ctx, "worktree", "add", "--quiet", "--", dest, pl.branch); err != nil {
			return res, err
		}
		res.Dir, res.CheckedOut, res.Worktree = dest, true, true
	case l.dirty:
		res.Dirty = true
		res.CheckedOut = l.repo.currentBranch(ctx) == pl.branch
	default:
		if l.repo.currentBranch(ctx) != pl.branch {
			clash, err := l.repo.ignoredClash(ctx, req.Commit)
			if err != nil {
				return res, err
			}
			if len(clash) > 0 {
				l.warn("%s was not switched to: the work versions files that are ignored here and a switch would overwrite them (%s); pull again with --worktree", pl.branch, strings.Join(clash, ", "))
				break
			}
			if _, err := l.repo.run(ctx, "switch", "--quiet", pl.branch); err != nil {
				return res, err
			}
		}
		res.CheckedOut = true
	}
	res.Warnings = l.warnings
	return res, nil
}

// Create makes branch at the commit that ref holds and checks nothing out. A branch that
// exists is never moved: at the commit it is left as it is, anywhere else it is refused.
func Create(ctx context.Context, dir, ref, commit, branch string) (Outcome, error) {
	if err := checkInputs(ref, commit); err != nil {
		return "", err
	}
	r := repo{dir: dir}
	var err error
	if r.dir, err = r.toplevel(ctx); err != nil {
		return "", err
	}
	if !r.validBranch(ctx, branch) {
		return "", fmt.Errorf("%w: %q", ErrInvalidBranch, branch)
	}
	if err := r.expect(ctx, ref, commit); err != nil {
		return "", err
	}
	if oid, exists := r.branchOID(ctx, branch); exists {
		if oid != commit {
			return "", fmt.Errorf("%w: %s", ErrBranchExists, branch)
		}
		return Unchanged, nil
	}
	// A branch that HEAD names without a commit is occupied: creating it would leave
	// the checkout on it without the files.
	checkedOut, err := r.checkedOut(ctx)
	if err != nil {
		return "", err
	}
	if checkedOut[branch] {
		return "", fmt.Errorf("%w: %s is checked out", ErrBranchExists, branch)
	}
	if _, err := r.run(ctx, "update-ref", "-m", "cflk checkout", "refs/heads/"+branch, commit, ""); err != nil {
		return "", err
	}
	return Created, nil
}

func checkInputs(ref, commit string) error {
	if !commitID.MatchString(commit) {
		return fmt.Errorf("checkout: %q is not a full commit id", commit)
	}
	if ref == "" || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("checkout: %q is not a ref", ref)
	}
	return nil
}

// destination is the real path of the directory the work is installed for: the worktree
// to make, or the checkout. The harness names its project directory after the real path
// (macOS /tmp is /private/tmp).
func (l *lander) destination() (string, error) {
	if !l.worktreeAsked {
		return filepath.EvalSymlinks(l.repo.dir)
	}
	abs := l.req.Worktree
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(l.req.Dir, abs)
	}
	abs = filepath.Clean(abs)
	if _, err := os.Lstat(abs); err == nil {
		return "", fmt.Errorf("%w: %s", ErrWorktreeExists, abs)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("checkout: the directory for the worktree: %w", err)
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

// Destination is the real path Land installs the work at, so that what depends on it (the
// session of the harness) can be checked before anything changes.
func Destination(req Request) (string, error) {
	root, err := (repo{dir: req.Dir}).toplevel(context.Background())
	if err != nil {
		return "", err
	}
	return (&lander{req: req, repo: repo{dir: root}, worktreeAsked: req.Worktree != ""}).destination()
}
