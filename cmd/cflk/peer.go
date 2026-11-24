package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Autarquico/codeflock/internal/buildinfo"
	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/server"
	"github.com/Autarquico/codeflock/internal/peer/wire"
)

// peerService is what `cflk peer` answers other peers with: the local use cases
// of localJobs, in the views the wire carries.
type peerService struct{ l *localJobs }

func (s peerService) server() server.Server { return server.Server{Local: s} }

func (s peerService) Hello(context.Context) (wire.Hello, error) {
	p, err := s.l.open()
	if err != nil {
		return wire.Hello{}, err
	}
	jobs, err := p.store.List()
	if err != nil {
		return wire.Hello{}, err
	}
	installed := []string{}
	for _, name := range []string{"claude", "codex"} {
		if _, err := exec.LookPath(name); err == nil {
			installed = append(installed, name)
		}
	}
	return wire.Hello{Name: p.name, Version: buildinfo.Get().Version, Harnesses: installed, Jobs: len(jobs)}, nil
}

func (s peerService) List(ctx context.Context) ([]wire.JobView, error) {
	views, err := s.l.List(ctx)
	out := make([]wire.JobView, len(views))
	for i, v := range views {
		out[i] = wire.JobView(v)
	}
	return out, err
}

func (s peerService) Show(ctx context.Context, id string) (wire.JobView, error) {
	v, err := s.l.Show(ctx, id)
	return wire.JobView(v), err
}

func (s peerService) Stop(ctx context.Context, id string, gen int) (wire.JobView, error) {
	v, err := s.l.stop(ctx, id, gen)
	return wire.JobView(v), err
}

func (s peerService) Resume(ctx context.Context, id string, gen int, force bool) (wire.JobView, error) {
	v, err := s.l.resume(ctx, id, gen, force)
	return wire.JobView(v), err
}

func (s peerService) Send(ctx context.Context, id string, gen int, text string) (wire.JobView, error) {
	v, err := s.l.send(ctx, id, gen, text)
	return wire.JobView(v), err
}

func (s peerService) Move(ctx context.Context, in server.MoveInput) (wire.JobView, error) {
	v, err := s.l.move(ctx, in)
	return wire.JobView(v), err
}

func (s peerService) Prepare(ctx context.Context, in server.PrepareInput, archive []byte) (wire.Transfer, error) {
	return s.l.prepare(ctx, in, archive)
}

func (s peerService) Activate(ctx context.Context, in server.TransferInput) (wire.JobView, error) {
	return s.l.activate(ctx, in)
}

func (s peerService) AbortTransfer(ctx context.Context, in server.TransferInput) (wire.Transfer, error) {
	return s.l.abortTransfer(ctx, in)
}

func (s peerService) Finish(ctx context.Context, in server.FinishInput) (wire.Finish, error) {
	return s.l.finish(ctx, in)
}

func (s peerService) FinishDone(ctx context.Context, in server.FinishNotice) (wire.JobView, error) {
	v, err := s.l.finishDone(ctx, in)
	return wire.JobView(v), err
}

func (s peerService) FinishAbort(ctx context.Context, in server.FinishNotice) (wire.JobView, error) {
	v, err := s.l.finishAbort(ctx, in)
	return wire.JobView(v), err
}

func (s peerService) Screen(ctx context.Context, id string) (string, error) {
	return s.l.Screen(ctx, id)
}

func (s peerService) AttachHere(ctx context.Context, id string, readOnly bool) error {
	return s.l.AttachHere(ctx, id, readOnly)
}

// Mirror makes sure the bare mirror of repo exists and returns where it is. The
// path is a function of the repo alone: <share>/repos/<sha256 of repo>.git.
func (s peerService) Mirror(ctx context.Context, repo string) (string, error) {
	p, err := s.l.open()
	if err != nil {
		return "", err
	}
	dir := p.mirrorDir(repo)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(p.mirrors, 0o700); err != nil {
		return "", err
	}
	if _, err := git(ctx, "", "init", "--bare", "--quiet", "--", dir); err != nil {
		return "", err
	}
	return dir, nil
}

func (p *peer) mirrorDir(repo string) string {
	return filepath.Join(p.mirrors, job.RepoHash(repo)+".git")
}

// RunFromMirror starts a delegated Job from the ref the sender pushed to the
// mirror: refs/cflk/<job>/g1 must exist and be exactly the commit it declared.
// It is idempotent on the id: asks for the same id take turns (the start is
// serialized on it), and one that finds the Job already here, from this repo and
// with its launch recorded, answers that Job, so a sender whose first answer was
// lost, or whose first ask is still running, gets the same outcome and never a
// second agent. The same id with another repo, harness or commit
// is CF_IDEMPOTENCY_MISMATCH. The ref stays after a failed start: it is
// immutable for the id, and a retry may already have pushed it.
func (s peerService) RunFromMirror(ctx context.Context, in server.RunInput) (wire.JobView, error) {
	p, err := s.l.open()
	if err != nil {
		return wire.JobView{}, err
	}
	harnessID, ok := harnessIDs[in.Harness]
	if !ok || in.Harness == "" {
		return wire.JobView{}, invalid("unknown harness %q: use claude or codex", in.Harness)
	}
	ref, err := wire.MirrorRef(in.Job, 1)
	if err != nil {
		return wire.JobView{}, err
	}
	release, err := p.store.LockStart(in.Job)
	if err != nil {
		return wire.JobView{}, err
	}
	defer release()
	mirror := p.mirrorDir(in.Repo)
	pushed, err := git(ctx, mirror, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil || pushed != in.Commit {
		return wire.JobView{}, invalid("%s is not at %s in the mirror of %s: push the code first", ref, in.Commit, in.Repo)
	}
	if v, found, err := p.launchedJob(ctx, mirror, harnessID, in); found || err != nil {
		return v, err
	}
	v, err := p.startFromMirror(ctx, mirror, harnessID, in)
	if err != nil {
		return wire.JobView{}, err
	}
	return wire.JobView(v), nil
}

// launchedJob is the Job this peer already has under the id of in, from its repo,
// when its launch is recorded. A Job with none is what a start left when it died
// before launching: it ran nothing and is discarded, so that the start begins again.
// The start lock is held, so no start of this id is in progress.
func (p *peer) launchedJob(ctx context.Context, mirror, harnessID string, in server.RunInput) (wire.JobView, bool, error) {
	var out wire.JobView
	found := false
	err := p.store.WithLock(in.Job, func(j *job.Job) error {
		switch {
		case j.Repo != in.Repo || j.Harness != harnessID || createdAt(*j) != in.Commit:
			return domain.NewError(domain.CodeIdempotencyMismatch, fmt.Sprintf("job %s already exists here for another repo, harness or commit", j.ID), nil)
		case j.State == job.Stopped && j.Launch == nil:
			if !p.discard(ctx, mirror, *j) {
				return fmt.Errorf("job %s is left over from a start that did not finish and could not be removed", j.ID)
			}
		default:
			out, found = wire.JobView(view(*j, "")), true
		}
		return nil
	})
	if errors.Is(err, job.ErrNotFound) {
		return wire.JobView{}, false, nil
	}
	return out, found, err
}

// createdAt is the commit the Job was created at.
func createdAt(j job.Job) string {
	if len(j.History) == 0 {
		return ""
	}
	return j.History[0].Commit
}

func (p *peer) startFromMirror(ctx context.Context, mirror, harnessID string, in server.RunInput) (cli.JobView, error) {
	if err := p.tmux.Check(ctx); err != nil {
		return cli.JobView{}, err
	}
	return p.start(ctx, startSpec{
		repoDir: mirror, repo: in.Repo, harness: harnessID, commit: in.Commit,
		prompt: in.Prompt, createdBy: in.CreatedBy, id: in.Job,
	})
}

// servePeer is `cflk peer`: one request on stdin, one response on stdout.
func (l *localJobs) servePeer(ctx context.Context, in io.Reader, out io.Writer) error {
	useLoginPath(ctx)
	return peerService{l}.server().Serve(ctx, in, out)
}

// attachPeer is `cflk peer attach [--read-only] JOB`.
func (l *localJobs) attachPeer(ctx context.Context, id string, readOnly bool) error {
	useLoginPath(ctx)
	return peerService{l}.server().Attach(ctx, id, readOnly)
}
