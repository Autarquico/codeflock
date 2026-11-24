package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/wire"
)

// Clean deletes what a Job that is over left on this peer: its worktrees, the
// trust entries that were written for them, and the refs of its generations, in
// the repositories of the worktrees and in the mirror. A Job that moved keeps its
// record, the pointer to where it went; a Job that is done is forgotten whole.
// Branches in the user's repository stay: they are the user's.
func (l *localJobs) Clean(ctx context.Context, id string, force bool) (cli.JobView, error) {
	p, err := l.open()
	if err != nil {
		return cli.JobView{}, err
	}
	if !job.ValidID(id) {
		return cli.JobView{}, invalid("%q is not a Job id (job-xxxxxxxxxx)", id)
	}
	var out job.Job
	err = p.store.WithLock(id, func(j *job.Job) error {
		out = *j
		if j.State != job.Moved && j.State != job.Done {
			return domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("job %s is %s: only a Job that moved or is done can be cleaned", j.ID, j.State), nil)
		}
		if !force {
			if err := p.unretrieved(ctx, *j); err != nil {
				return err
			}
		}
		if err := p.removeWork(ctx, *j); err != nil {
			return err
		}
		if j.State == job.Done {
			return os.RemoveAll(p.store.Dir(j.ID))
		}
		return nil
	})
	return view(out, ""), mapErr(err)
}

// unretrieved refuses a clean that would delete work nothing else holds. Only
// the generation this peer last held can: every earlier one was sealed into the
// commit the next one started from. The rules, without --force:
//   - its worktree has no change beyond what was sealed when the Job left (a
//     Job that ended by itself sealed nothing: no change at all), and
//   - a Job that ended by itself has every commit of its work in a branch, tag
//     or remote-tracking ref that the clean leaves (the mirror's own cflk/<id>
//     does not count), or in the commit the Job started from.
//
// A moved Job's work is the destination's, and a finished Job's was fetched by the
// peer that finished it, so they need only the first rule.
func (p *peer) unretrieved(ctx context.Context, j job.Job) error {
	wt := p.store.WorktreeDir(j.ID, j.Gen)
	if _, err := os.Stat(wt); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	sealed := ""
	switch {
	case j.Finish != nil:
		sealed = j.Finish.Commit
	case j.State == job.Moved:
		if ref, err := wire.MirrorRef(j.ID, j.Gen+1); err == nil {
			sealed, _ = git(ctx, wt, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		}
	}
	if err := changedSince(ctx, wt, sealed); err != nil {
		return refuseClean(j, wt, err)
	}
	if j.State == job.Done && j.Finish == nil {
		if err := p.unpublished(ctx, j, wt); err != nil {
			return refuseClean(j, wt, err)
		}
	}
	return nil
}

func refuseClean(j job.Job, wt string, err error) error {
	var lost *lostWork
	if !errors.As(err, &lost) {
		return fmt.Errorf("checking %s before cleaning job %s: %w", wt, j.ID, err)
	}
	return domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf(
		"job %s: %s %s that nothing else has: copy it out, or `cflk clean --force %s` to delete it", j.ID, wt, lost.what, j.ID), nil)
}

// lostWork is what a clean would delete and nobody else holds.
type lostWork struct{ what string }

func (l *lostWork) Error() string { return l.what }

// changedSince reports a worktree whose files differ from the commit sealed (or,
// with none, from HEAD), untracked ones included and ignored ones not.
func changedSince(ctx context.Context, wt, sealed string) error {
	out, err := git(ctx, wt, "status", "--porcelain")
	if err != nil {
		return err
	}
	if out == "" {
		return nil
	}
	if sealed == "" {
		return &lostWork{"has uncommitted changes"}
	}
	dir, err := os.MkdirTemp("", "cflk-clean-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir) //nolint:errcheck // a scratch index
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	for _, args := range [][]string{{"read-tree", sealed}, {"add", "-A"}} {
		if _, err := gitEnv(ctx, wt, env, args...); err != nil {
			return err
		}
	}
	diff, err := gitEnv(ctx, wt, env, "diff-index", "--cached", "--name-only", sealed)
	if err != nil {
		return err
	}
	if diff != "" {
		return &lostWork{"has changes made after the work was sealed"}
	}
	return nil
}

// unpublished reports commits of the Job that no surviving ref holds.
func (p *peer) unpublished(ctx context.Context, j job.Job, wt string) error {
	args := []string{"rev-list", "-n", "1", "HEAD", "--not"}
	if common, err := commonDir(ctx, wt); err == nil {
		if mirror, err := filepath.EvalSymlinks(p.mirrorDir(j.Repo)); err == nil && common == mirror {
			args = append(args, "--exclude="+"cflk/"+j.ID)
		}
	}
	args = append(args, "--branches", "--remotes", "--tags")
	if len(j.History) > 0 && j.History[0].Commit != "" {
		args = append(args, j.History[0].Commit)
	}
	out, err := git(ctx, wt, args...)
	if err != nil {
		return err
	}
	if out != "" {
		return &lostWork{"has commits that no branch, tag or remote ref keeps"}
	}
	return nil
}

// removeWork removes the worktrees of the Job and what hangs from them.
func (p *peer) removeWork(ctx context.Context, j job.Job) error {
	worktrees, err := filepath.Glob(filepath.Join(p.store.Dir(j.ID), "g*"))
	if err != nil {
		return err
	}
	repos := []string{p.mirrorDir(j.Repo)}
	for _, wt := range worktrees {
		if common, err := git(ctx, wt, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
			repos = append(repos, common)
		}
		if err := untrust(p.host, j.Harness, wt); err != nil {
			return fmt.Errorf("removing the trust entry of %s: %w", wt, err)
		}
	}
	for _, repo := range repos {
		if _, err := os.Stat(repo); err != nil {
			continue
		}
		for _, wt := range worktrees {
			p.dropWorktree(ctx, repo, wt)
		}
		if err := dropRefs(ctx, repo, j.ID); err != nil {
			return err
		}
		if repo == repos[0] {
			_, _ = git(ctx, repo, "branch", "-D", "--", "cflk/"+j.ID) // only the mirror's: the user's branches are theirs
		}
	}
	for _, wt := range worktrees {
		if err := os.RemoveAll(wt); err != nil {
			return err
		}
	}
	return nil
}

func dropRefs(ctx context.Context, repo, id string) error {
	out, err := git(ctx, repo, "for-each-ref", "--format=%(refname)", "refs/cflk/"+id+"/")
	if err != nil {
		return err
	}
	var errs []error
	for ref := range strings.SplitSeq(out, "\n") {
		if ref != "" {
			if _, err := git(ctx, repo, "update-ref", "-d", ref); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// untrust takes back the entry that launching the harness in the worktree wrote.
func untrust(host harness.Host, harnessID, wt string) error {
	switch harnessID {
	case claude.ID:
		return (claude.Adapter{}).UntrustFolder(host, wt)
	case codex.ID:
		return (codex.Adapter{}).UntrustFolder(host, wt)
	}
	return nil
}
