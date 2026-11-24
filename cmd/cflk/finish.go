package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/checkout"
	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/remote"
	"github.com/Autarquico/codeflock/internal/peer/runner"
	"github.com/Autarquico/codeflock/internal/peer/server"
	"github.com/Autarquico/codeflock/internal/peer/wire"
)

// This file is `pull --finish`. The peer that owns the Job stops it, seals its
// work and writes `finishing` with the commit and a finish id; the peer that
// asked brings that exact commit as the branch cflk/<id> of the repository it is
// in and tells the owner, which only then marks the Job done. Whatever fails in
// between leaves the Job finishing, and `pull --finish` repeats the finish or
// `pull --finish --abort` gives it up.

// The points of a finish.
const (
	pointFinishing = "finish:sealed"  // the owner wrote `finishing`
	pointFetched   = "finish:fetched" // the branch is in the repository of whoever asked
)

// pushTimeout bounds the push of the branch to origin.
const pushTimeout = remote.DefaultPushTimeout

// finish is the owner's side: stop and seal the Job (once: a Job that is
// finishing keeps its commit) and, when another peer asked, send the commit to its mirror.
func (l *localJobs) finish(ctx context.Context, in server.FinishInput) (wire.Finish, error) {
	p, err := l.open()
	if err != nil {
		return wire.Finish{}, err
	}
	j, _, err := l.read(ctx, in.Job)
	if err != nil {
		return wire.Finish{}, err
	}
	if err := j.ExpectGen(in.Gen); err != nil {
		return wire.Finish{}, mapErr(err)
	}
	var target *discovery.Peer // resolved before anything is stopped or sealed
	if in.To != "" && in.To != p.name {
		t, err := l.reachable(ctx, in.To, in.ToNames)
		if err != nil {
			return wire.Finish{}, err
		}
		target = &t
	}
	switch j.State {
	case job.Finishing:
	case job.Done:
		if j.Finish == nil {
			return wire.Finish{}, domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("job %s is done: its agent ended by itself", j.ID), nil)
		}
		return wire.Finish{ID: j.Finish.ID, Commit: j.Finish.Commit}, nil // answered already; the commit was fetched
	case job.Moving:
		return wire.Finish{}, movingError(j)
	default:
		if err := cleanSubmodules(ctx, p.store.WorktreeDir(j.ID, j.Gen)); err != nil {
			return wire.Finish{}, err
		}
		if err := p.runner.Stop(ctx, in.Job, runner.Expect(in.Gen)); err != nil {
			return wire.Finish{}, mapErr(err)
		}
	}
	sealed, err := p.sealFinish(ctx, in)
	if err != nil {
		return wire.Finish{}, err
	}
	out := wire.Finish{ID: sealed.Finish.ID, Commit: sealed.Finish.Commit}
	if target == nil {
		return out, nil
	}
	if err := l.sendFinish(ctx, p, sealed, *target); err != nil {
		return wire.Finish{}, finishStuck(sealed.ID, err)
	}
	return out, nil
}

// sealFinish is steps 1 to 2 of a finish under the Job's lock: the work is sealed
// into a commit and `finishing` is written with it. A Job that is finishing
// already (or done by a finish) is returned as it is.
func (p *peer) sealFinish(ctx context.Context, in server.FinishInput) (job.Job, error) {
	var out job.Job
	err := p.store.WithLockSave(in.Job, func(j *job.Job, save func() error) error {
		if err := j.ExpectGen(in.Gen); err != nil {
			return err
		}
		switch {
		case j.Finish != nil && (j.State == job.Finishing || j.State == job.Done):
			out = *j
			return nil
		case j.State != job.Stopped:
			return domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("job %s is %s, not stopped: it cannot be finished until `cflk stop` ends its agent", j.ID, j.State), nil)
		}
		if err := cleanSubmodules(ctx, p.store.WorktreeDir(j.ID, j.Gen)); err != nil {
			return err
		}
		commit, err := p.seal(ctx, *j)
		if err != nil {
			return fmt.Errorf("sealing the work of job %s: %w", j.ID, err)
		}
		if err := j.Transition(job.Finishing); err != nil {
			return err
		}
		j.Finish = &job.Finish{ID: job.NewTransferID(), Commit: commit}
		if err := save(); err != nil {
			return err
		}
		out = *j
		return p.fault(pointFinishing)
	})
	return out, mapErr(err)
}

// reachable finds the peer a request names (by node ID, then by any of its names)
// and checks that it is online.
func (l *localJobs) reachable(ctx context.Context, to string, names []string) (discovery.Peer, error) {
	if l.net == nil {
		return discovery.Peer{}, invalid("this peer has no way to reach the others")
	}
	target, err := l.findTarget(ctx, to, names...)
	if err != nil {
		return discovery.Peer{}, err
	}
	if !target.Online {
		return discovery.Peer{}, domain.NewError(domain.CodePeerUnreachable, fmt.Sprintf("peer %s is offline (Tailscale)", target.Name), nil)
	}
	return target, nil
}

// sendFinish pushes the sealed commit to the mirror of the peer that asked.
func (l *localJobs) sendFinish(ctx context.Context, p *peer, j job.Job, target discovery.Peer) error {
	ref, err := wire.MirrorRef(j.ID, j.Gen+1)
	if err != nil {
		return err
	}
	return remote.PushCode(ctx, l.net.transport, target, remote.PushSpec{
		RepoDir: p.store.WorktreeDir(j.ID, j.Gen), Repo: j.Repo, Commit: j.Finish.Commit, Ref: ref, Force: true,
	})
}

// finishStuck is the error of a finish that cannot say how it ended: the Job
// stays finishing and the person repeats it or gives it up.
func finishStuck(id string, err error) error {
	msg := cleanMessage(err)
	if !strings.Contains(msg, "pull "+id+" --finish") {
		msg = fmt.Sprintf("job %s stays finishing (%s): `cflk pull %s --finish` repeats it, `cflk pull %s --finish --abort` gives it up", id, msg, id, id)
	}
	if c := codeOf(err); c != "" {
		return domain.NewError(c, msg, nil)
	}
	return errors.New(msg)
}

// finishDone marks done the Job whose finish id is the one in progress; it
// answers the same to a notice that repeats.
func (l *localJobs) finishDone(ctx context.Context, in server.FinishNotice) (cli.JobView, error) {
	return l.endFinish(ctx, in, job.Done)
}

// finishAbort gives the finish up: the Job is stopped again, if Finish is the one
// in progress (a delayed abort of an earlier finish must not cancel a later one).
func (l *localJobs) finishAbort(ctx context.Context, in server.FinishNotice) (cli.JobView, error) {
	return l.endFinish(ctx, in, job.Stopped)
}

func (l *localJobs) endFinish(_ context.Context, in server.FinishNotice, to job.State) (cli.JobView, error) {
	p, err := l.open()
	if err != nil {
		return cli.JobView{}, err
	}
	if !job.ValidID(in.Job) {
		return cli.JobView{}, invalid("%q is not a Job id (job-xxxxxxxxxx)", in.Job)
	}
	var out job.Job
	err = p.store.WithLock(in.Job, func(j *job.Job) error {
		defer func() { out = *j }()
		if err := j.ExpectGen(in.Gen); err != nil {
			return err
		}
		if j.State == to && to == job.Done && j.Finish != nil && j.Finish.ID == in.Finish {
			return nil // the same notice, answered already
		}
		if j.State != job.Finishing || j.Finish == nil || j.Finish.ID != in.Finish {
			return domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("job %s is %s: that is not the finish in progress", j.ID, j.State), nil)
		}
		if err := j.Transition(to); err != nil {
			return err
		}
		if to == job.Stopped {
			j.Finish = nil
		}
		return nil
	})
	return view(out, ""), mapErr(err)
}

// Pull brings a Job here. Alone it moves the Job to this peer; with Finish it
// stops the Job where it is and brings its branch to the repository this command
// runs in; Abort gives a finish that was left unconfirmed up.
func (f *fleet) Pull(ctx context.Context, req cli.PullRequest) (cli.JobView, error) {
	self, err := f.selfName()
	if err != nil {
		return cli.JobView{}, err
	}
	if !wire.ValidName(self) {
		return cli.JobView{}, invalid("the name of this machine, %q, cannot travel to a peer: use letters, digits, '.', '_' and '-'", self)
	}
	o, err := f.locate(ctx, req.ID)
	if err != nil {
		return cli.JobView{}, err
	}
	if req.Abort {
		return f.abortFinish(ctx, o)
	}
	if o.peer == nil && !req.Finish {
		return cli.JobView{}, invalid("job %s is on this peer already", req.ID)
	}
	me, names := self, []string(nil)
	if o.peer != nil {
		if me, names, err = f.identity(ctx); err != nil {
			return cli.JobView{}, err
		}
	}
	if req.Finish {
		return f.finish(ctx, o, me, names, req.Push)
	}
	return f.move(ctx, cli.MoveRequest{ID: req.ID, To: me}, names)
}

// finish asks the owner for the finish and brings its commit to the repository of
// the current directory as the branch cflk/<id>.
func (f *fleet) finish(ctx context.Context, o owner, self string, names []string, push bool) (cli.JobView, error) {
	src, err := sourceOf(ctx, "")
	if err != nil {
		return cli.JobView{}, err
	}
	if src.repo != o.view.Repo {
		return cli.JobView{}, invalid("job %s is of %s and this directory is of %s: run `cflk pull --finish` in a clone of the Job's repository", o.view.ID, o.view.Repo, src.repo)
	}
	p, err := f.local.open()
	if err != nil {
		return cli.JobView{}, err
	}
	var info wire.Finish
	var from string // where the commit is: the mirror of this peer, or the worktree of a Job that is here
	if o.peer == nil {
		if info, err = f.local.finish(ctx, server.FinishInput{Job: o.view.ID, Gen: o.view.Gen}); err != nil {
			return cli.JobView{}, finishStuck(o.view.ID, err)
		}
		from = p.store.WorktreeDir(o.view.ID, o.view.Gen)
	} else {
		resp, _, err := f.callWith(ctx, o, wire.Request{Op: wire.OpFinish, To: self, ToNames: names}, nil)
		if err != nil {
			return cli.JobView{}, finishStuck(o.view.ID, err)
		}
		info, from = *resp.Finish, p.mirrorDir(o.view.Repo)
	}
	if o.view.State == string(job.Done) {
		return o.view, nil // done already: its branch was fetched then, and the seal is gone
	}
	branch := "cflk/" + o.view.ID
	if err := p.releaseBranch(ctx, src.dir, o.view.ID, branch); err != nil {
		return cli.JobView{}, finishStuck(o.view.ID, err)
	}
	if err := fetchBranch(ctx, src.dir, from, o.view.ID, o.view.Gen+1, info.Commit, branch); err != nil {
		return cli.JobView{}, finishStuck(o.view.ID, err)
	}
	if err := p.fault(pointFetched); err != nil {
		return cli.JobView{}, err
	}
	if push {
		if err := pushBranch(ctx, src.dir, branch); err != nil {
			return cli.JobView{}, finishStuck(o.view.ID, err)
		}
	}
	notice := server.FinishNotice{Job: o.view.ID, Gen: o.view.Gen, Finish: info.ID}
	if o.peer == nil {
		v, err := f.local.finishDone(ctx, notice)
		if err != nil {
			return v, mapErr(err)
		}
		return v, dropSealRef(ctx, src.dir, o.view.ID, o.view.Gen+1)
	}
	resp, _, err := f.callWith(ctx, o, wire.Request{Op: wire.OpFinishDone, Finish: info.ID}, nil)
	if err != nil {
		return cli.JobView{}, finishStuck(o.view.ID, fmt.Errorf("the branch %s is here, but %s did not confirm it: %w", branch, o.peer.Name, err))
	}
	return cli.JobView(*resp.Job), dropSealRef(ctx, src.dir, o.view.ID, o.view.Gen+1)
}

// abortFinish asks the owner to leave `finishing`. It only counts when the owner answers.
func (f *fleet) abortFinish(ctx context.Context, o owner) (cli.JobView, error) {
	if o.view.FinishID == "" {
		return cli.JobView{}, domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("job %s is %s: it has no finish to give up", o.view.ID, o.view.State), nil)
	}
	if o.peer == nil {
		v, err := f.local.finishAbort(ctx, server.FinishNotice{Job: o.view.ID, Gen: o.view.Gen, Finish: o.view.FinishID})
		return v, mapErr(err)
	}
	resp, _, err := f.callWith(ctx, o, wire.Request{Op: wire.OpFinishAbort, Finish: o.view.FinishID}, nil)
	if err != nil {
		return cli.JobView{}, domain.NewError(cmpCode(err), fmt.Sprintf("the abort is refused: %s did not confirm it (%s); job %s stays as it was", o.peer.Name, cleanMessage(err), o.view.ID), nil)
	}
	return cli.JobView(*resp.Job), nil
}

func cmpCode(err error) domain.Code {
	if c := codeOf(err); c != "" {
		return c
	}
	return domain.CodePeerNoAnswer
}

// fetchBranch brings the commit under refs/cflk/<job>/g<gen> of the repository in dir
// and puts it on the branch, without touching the checkout. The ref stays: for a
// Job of this repository it is the seal's keep-alive and what a retry fetches,
// so it is dropped (dropSealRef) only once the finish is done.
func fetchBranch(ctx context.Context, dir, from, id string, gen int, commit, branch string) error {
	ref, err := wire.MirrorRef(id, gen)
	if err != nil {
		return err
	}
	if _, err := git(ctx, dir, "fetch", "--quiet", "--no-tags", "--", from, "+"+ref+":"+ref); err != nil {
		return fmt.Errorf("fetching the work: %w", err)
	}
	switch _, err := checkout.Place(ctx, dir, ref, commit, branch); {
	case errors.Is(err, checkout.ErrBranchExists), errors.Is(err, checkout.ErrBranchDiverged):
		return domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("the branch %s is here with other work (%v)", branch, err), nil)
	case err != nil:
		return err
	}
	return nil
}

// dropSealRef removes the ref a finish fetched the work under, once the branch
// holds it and the owner has marked the Job done.
func dropSealRef(ctx context.Context, dir, id string, gen int) error {
	ref, err := wire.MirrorRef(id, gen)
	if err != nil {
		return err
	}
	_, err = git(context.WithoutCancel(ctx), dir, "update-ref", "-d", ref)
	return err
}

// releaseBranch detaches the worktrees of this Job, here, that have its branch
// checked out (`cflk run` leaves the first one so): the Job is not theirs any
// more, and the branch cannot advance under a checkout. The files stay. Nothing
// that is not a worktree of this Job in the repository of dir is touched.
func (p *peer) releaseBranch(ctx context.Context, dir, id, branch string) error {
	own, err := filepath.Glob(filepath.Join(p.store.Dir(id), "g*"))
	if err != nil || len(own) == 0 {
		return err
	}
	here, err := commonDir(ctx, dir)
	if err != nil {
		return err
	}
	return p.store.WithLock(id, func(j *job.Job) error {
		if j.State != job.Moved && j.State != job.Finishing && j.State != job.Done {
			return nil
		}
		for _, wt := range own {
			if common, err := commonDir(ctx, wt); err != nil || common != here {
				continue
			}
			if head, err := git(ctx, wt, "symbolic-ref", "--quiet", "HEAD"); err != nil || head != "refs/heads/"+branch {
				continue
			}
			if _, err := git(ctx, wt, "switch", "--detach"); err != nil {
				return fmt.Errorf("detaching %s from %s: %w", wt, branch, err)
			}
		}
		return nil
	})
}

func commonDir(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(out)
}

// pushBranch publishes the branch to origin as it is: no force, no hooks, no tags.
func pushBranch(ctx context.Context, dir, branch string) error {
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	refspec := "refs/heads/" + branch + ":refs/heads/" + branch
	if _, err := git(ctx, dir, "-c", "core.hooksPath=/dev/null", "push", "--quiet", "--no-verify", "--no-follow-tags", "--", "origin", refspec); err != nil {
		return fmt.Errorf("pushing %s to origin: %w", branch, err)
	}
	return nil
}
