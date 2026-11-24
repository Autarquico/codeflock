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
)

// Clean deletes what a Job that is over left on this peer: its worktrees, the
// trust entries that were written for them, and the refs of its generations, in
// the repositories of the worktrees and in the mirror. A Job that moved keeps its
// record, the pointer to where it went; a Job that is done is forgotten whole.
// Branches in the user's repository stay: they are the user's.
func (l *localJobs) Clean(ctx context.Context, id string) (cli.JobView, error) {
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
