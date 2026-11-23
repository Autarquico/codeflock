// Package checkout brings the commit of a Job to a checkout of this peer without
// touching what is there. It never resets, stashes or cleans: it adds a branch,
// moves one that is behind, and switches to it only when the tree is clean. The
// commit is the exact one the caller expects, already fetched under a ref of the
// repository; anything else is refused before anything changes.
package checkout

import (
	"context"
	"fmt"
	"strings"
)

// Outcome is what became of the branch the work was put on.
type Outcome string

// The outcomes.
const (
	Created   Outcome = "created"
	Advanced  Outcome = "advanced"
	Unchanged Outcome = "unchanged"
)

// placement is where the commit goes.
type placement struct {
	branch  string
	outcome Outcome
	// merge says the branch is the one checked out here, which moves with a
	// fast-forward merge of the clean tree and not by writing the ref under it.
	merge bool
	// old is the commit the branch had when it was chosen: the move only goes through
	// if the branch is still there.
	old string
}

// place chooses the branch for commit, trying in order: the branch the Job started from
// (only if it is an ancestor of the commit, so that it advances), the Job's own branch,
// and the alternative when the first two have commits that the work does not. No branch
// is ever moved to a commit that does not contain it, and none that a working tree has
// checked out is moved under that tree.
func (l *lander) place(ctx context.Context, commit string) (placement, error) {
	var candidates []string
	for _, name := range []string{l.req.Source, l.req.Branch, l.req.Alt} {
		if name != "" {
			candidates = append(candidates, name)
		}
	}
	checkedOut, err := l.repo.checkedOut(ctx)
	if err != nil {
		return placement{}, err
	}
	current := l.repo.currentBranch(ctx)
	for i, name := range candidates {
		last := i == len(candidates)-1
		isSource := l.req.Source != "" && i == 0
		if !l.repo.validBranch(ctx, name) {
			if last {
				return placement{}, fmt.Errorf("%w: %q", ErrInvalidBranch, name)
			}
			continue
		}
		oid, exists := l.repo.branchOID(ctx, name)
		switch {
		case !exists && isSource:
			continue
		case !exists:
			return placement{branch: name, outcome: Created}, nil
		case oid == commit:
			if checkedOut[name] && (l.worktreeAsked || name != current) {
				continue
			}
			return placement{branch: name, outcome: Unchanged}, nil
		}
		behind, err := l.repo.isAncestor(ctx, oid, commit)
		if err != nil {
			return placement{}, err
		}
		if !behind {
			switch {
			case isSource:
			case last:
				return placement{}, fmt.Errorf("%w: %s has commits that the work does not: nothing was changed", ErrBranchDiverged, name)
			default:
				l.warn("the local branch %s has commits that the work does not: the work goes to %s", name, candidates[i+1])
			}
			continue
		}
		if checkedOut[name] {
			if name != current || l.worktreeAsked || l.dirty {
				continue
			}
			clash, err := l.repo.ignoredClash(ctx, commit)
			if err != nil {
				return placement{}, err
			}
			if len(clash) > 0 {
				l.warn("%s was not advanced: the work versions files that are ignored here and the merge would overwrite them (%s); pull again with --worktree", name, strings.Join(clash, ", "))
				continue
			}
			return placement{branch: name, outcome: Advanced, merge: true, old: oid}, nil
		}
		return placement{branch: name, outcome: Advanced, old: oid}, nil
	}
	return placement{}, ErrNoBranch
}

// move puts the branch where the placement says.
func (l *lander) move(ctx context.Context, pl placement, commit string) error {
	switch {
	case pl.outcome == Unchanged:
		return nil
	case pl.merge:
		_, err := l.repo.run(ctx, "merge", "--ff-only", "--quiet", commit)
		return err
	case pl.outcome == Created:
		_, err := l.repo.run(ctx, "update-ref", "-m", "cflk checkout", "refs/heads/"+pl.branch, commit, "")
		return err
	}
	_, err := l.repo.run(ctx, "update-ref", "-m", "cflk checkout", "refs/heads/"+pl.branch, commit, pl.old)
	return err
}
