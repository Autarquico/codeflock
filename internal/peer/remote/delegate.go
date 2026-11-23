package remote

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/wire"
)

// Delegation is a Job to start on another peer from a commit of a repository
// on this machine.
type Delegation struct {
	// RepoDir is the repository the commit is in.
	RepoDir string
	// Repo is its normalized origin.
	Repo      string
	Commit    string
	Harness   string
	CreatedBy string
	Prompt    string
}

// PushArgv is the `git push` that sends commit to ref of the target: an argv,
// with no shell and `--` before the URL.
func PushArgv(t PushTarget, commit, ref string) []string {
	argv := []string{"git"}
	for _, c := range t.Config {
		argv = append(argv, "-c", c)
	}
	return append(argv, "push", "--quiet", "--", t.URL, commit+":"+ref)
}

// Delegate starts the Job on the peer: it asks for the mirror of the repo,
// pushes the commit to refs/cflk/<job>/g1 in it, and asks for the run. The code
// is in the mirror before the run is asked for; the conversation is not in git.
func Delegate(ctx context.Context, t Transport, peer discovery.Peer, d Delegation) (wire.JobView, error) {
	if !wire.ValidCommit(d.Commit) {
		return wire.JobView{}, fmt.Errorf("%q is not a full commit id", d.Commit)
	}
	mirror, _, err := Call(ctx, t, peer, wire.Request{Op: wire.OpMirror, Repo: d.Repo}, nil)
	if err != nil {
		return wire.JobView{}, err
	}
	if want := job.RepoHash(d.Repo) + ".git"; path.Base(mirror.Mirror) != want {
		return wire.JobView{}, fmt.Errorf("peer %s answered the mirror %q, not the one of this repo", peer.Name, path.Base(mirror.Mirror))
	}
	target, err := t.PushTarget(peer, mirror.Mirror)
	if err != nil {
		return wire.JobView{}, err
	}
	id := job.NewID()
	ref, err := wire.MirrorRef(id, 1)
	if err != nil {
		return wire.JobView{}, err
	}
	if err := runGit(ctx, d.RepoDir, PushArgv(target, d.Commit, ref)); err != nil {
		return wire.JobView{}, fmt.Errorf("sending the code to %s: %w", peer.Name, err)
	}
	resp, _, err := Call(ctx, t, peer, wire.Request{
		Op: wire.OpRun, Job: id, Harness: d.Harness, Repo: d.Repo, Commit: d.Commit, CreatedBy: d.CreatedBy,
	}, []byte(d.Prompt))
	if err != nil {
		return wire.JobView{}, err
	}
	if resp.Job == nil {
		return wire.JobView{}, fmt.Errorf("peer %s answered a run without a Job", peer.Name)
	}
	return *resp.Job, nil
}

func runGit(ctx context.Context, dir string, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // PushArgv: a fixed argv, no shell
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{buf: &stderr, room: maxStderr}
	if err := cmd.Run(); err != nil {
		if msg := strings.Join(strings.Fields(stderr.String()), " "); msg != "" {
			return fmt.Errorf("git push: %s", msg)
		}
		return fmt.Errorf("git push: %w", err)
	}
	return nil
}
