package checkout

import (
	"context"
	"fmt"
)

// Place is Create for the branch of a Job that comes back to a repository that
// had it: a branch that is behind the commit advances to it (it is the Job's own
// earlier work), one that holds commits the work does not is refused, and so is
// one that a working tree has checked out and would have to move under it. The
// checkout is never touched: only the ref changes.
func Place(ctx context.Context, dir, ref, commit, branch string) (Outcome, error) {
	if err := checkInputs(ref, commit); err != nil {
		return "", err
	}
	r := repo{dir: dir}
	var err error
	if r.dir, err = r.toplevel(ctx); err != nil {
		return "", err
	}
	oid, exists := r.branchOID(ctx, branch)
	if !exists || oid == commit {
		return Create(ctx, dir, ref, commit, branch)
	}
	if !r.validBranch(ctx, branch) {
		return "", fmt.Errorf("%w: %q", ErrInvalidBranch, branch)
	}
	if err := r.expect(ctx, ref, commit); err != nil {
		return "", err
	}
	behind, err := r.isAncestor(ctx, oid, commit)
	switch {
	case err != nil:
		return "", err
	case !behind:
		return "", fmt.Errorf("%w: %s has commits that the work does not: nothing was changed", ErrBranchDiverged, branch)
	}
	checkedOut, err := r.checkedOut(ctx)
	if err != nil {
		return "", err
	}
	if checkedOut[branch] {
		return "", fmt.Errorf("%w: %s is checked out and is behind the work: remove that worktree (cflk clean) or switch away first", ErrBranchExists, branch)
	}
	if _, err := r.run(ctx, "update-ref", "-m", "cflk checkout", "refs/heads/"+branch, commit, oid); err != nil {
		return "", err
	}
	return Advanced, nil
}
