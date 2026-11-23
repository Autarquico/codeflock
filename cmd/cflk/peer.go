package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Autarquico/codeflock/internal/buildinfo"
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
	var installed []string
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

func (s peerService) Stop(ctx context.Context, id string) (wire.JobView, error) {
	v, err := s.l.Stop(ctx, id)
	return wire.JobView(v), err
}

func (s peerService) Resume(ctx context.Context, id string) (wire.JobView, error) {
	v, err := s.l.Resume(ctx, id)
	return wire.JobView(v), err
}

func (s peerService) Send(ctx context.Context, id, text string) (wire.JobView, error) {
	v, err := s.l.Send(ctx, id, text)
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
	mirror := p.mirrorDir(in.Repo)
	pushed, err := git(ctx, mirror, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil || pushed != in.Commit {
		return wire.JobView{}, invalid("%s is not at %s in the mirror of %s: push the code first", ref, in.Commit, in.Repo)
	}
	if err := p.tmux.Check(ctx); err != nil {
		return wire.JobView{}, err
	}
	v, err := p.start(ctx, startSpec{
		repoDir: mirror, repo: in.Repo, harness: harnessID, commit: in.Commit,
		prompt: in.Prompt, createdBy: in.CreatedBy, id: in.Job,
	})
	return wire.JobView(v), err
}

// servePeer is `cflk peer`: one request on stdin, one response on stdout.
func (l *localJobs) servePeer(ctx context.Context, in io.Reader, out io.Writer) error {
	return peerService{l}.server().Serve(ctx, in, out)
}

// attachPeer is `cflk peer attach [--read-only] JOB`.
func (l *localJobs) attachPeer(ctx context.Context, id string, readOnly bool) error {
	return peerService{l}.server().Attach(ctx, id, readOnly)
}
