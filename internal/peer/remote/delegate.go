package remote

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/Autarquico/codeflock/internal/domain"
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
	// ID is the Job id to use; empty draws one. Asking again after a failure
	// with the id the failure named is safe: the peer answers the Job it already has.
	ID string
	// OrderTimeout bounds each question to the peer and PushTimeout the push;
	// zero is the default.
	OrderTimeout, PushTimeout time.Duration
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

// Deadlines of a delegation: the questions to the peer are quick, the push is
// the code of a repository.
const (
	DefaultOrderTimeout = 30 * time.Second
	DefaultPushTimeout  = 5 * time.Minute
)

// Delegate starts the Job on the peer: it asks for the mirror of the repo,
// pushes the commit to refs/cflk/<job>/g1 in it, and asks for the run. The code
// is in the mirror before the run is asked for; the conversation is not in git.
//
// The id is drawn here, before anything leaves, so every failure can name it, and
// the run is idempotent on it: asking again with the same id (d.ID) returns the
// Job the first ask started instead of a second one.
func Delegate(ctx context.Context, t Transport, peer discovery.Peer, d Delegation) (wire.JobView, error) {
	if !wire.ValidCommit(d.Commit) {
		return wire.JobView{}, fmt.Errorf("%q is not a full commit id", d.Commit)
	}
	id := cmp.Or(d.ID, job.NewID())
	ref, err := wire.MirrorRef(id, 1)
	if err != nil {
		return wire.JobView{}, err
	}
	order := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, cmp.Or(d.OrderTimeout, DefaultOrderTimeout))
	}
	askCtx, cancel := order()
	mirror, _, err := Call(askCtx, t, peer, wire.Request{Op: wire.OpMirror, Repo: d.Repo}, nil)
	cancel()
	if err != nil {
		return wire.JobView{}, delegationError(err, id, peer, outcomeNothing)
	}
	if want := job.RepoHash(d.Repo) + ".git"; path.Base(mirror.Mirror) != want {
		return wire.JobView{}, delegationError(fmt.Errorf("answered the mirror %q, not the one of this repo", path.Base(mirror.Mirror)), id, peer, outcomeNothing)
	}
	target, err := t.PushTarget(peer, mirror.Mirror)
	if err != nil {
		return wire.JobView{}, delegationError(err, id, peer, outcomeNothing)
	}
	pushCtx, cancel := context.WithTimeout(ctx, cmp.Or(d.PushTimeout, DefaultPushTimeout))
	err = runGit(pushCtx, d.RepoDir, PushArgv(target, d.Commit, ref))
	cancel()
	if err != nil {
		return wire.JobView{}, delegationError(fmt.Errorf("sending the code: %w", err), id, peer, outcomeNothing)
	}
	runCtx, cancel := order()
	defer cancel()
	resp, _, err := Call(runCtx, t, peer, wire.Request{
		Op: wire.OpRun, Job: id, Harness: d.Harness, Repo: d.Repo, Commit: d.Commit, CreatedBy: d.CreatedBy,
	}, []byte(d.Prompt))
	if err != nil {
		return wire.JobView{}, delegationError(err, id, peer, outcomeOfRun(err))
	}
	if resp.Job == nil || resp.Job.ID != id {
		return wire.JobView{}, delegationError(errors.New("answered the run without the Job that was asked"), id, peer, outcomeUnknown)
	}
	return *resp.Job, nil
}

type outcome string

const (
	outcomeNothing outcome = "nothing was started"
	outcomeRefused outcome = "the peer refused it"
	outcomeUnknown outcome = "it may be running"
)

// outcomeOfRun is what a failed run ask says about the Job. A peer that answered
// with an error did not start it; a stream that broke after the request left
// may have.
func outcomeOfRun(err error) outcome {
	var coded *domain.Error
	if errors.As(err, &coded) && coded.Code != domain.CodePeerUnreachable && coded.Code != domain.CodePeerNoAnswer {
		return outcomeRefused
	}
	return outcomeUnknown
}

// delegationError names the Job and the peer in a failure of any step, and says
// whether the Job exists, so the person can `cflk show` it or ask again. It keeps
// the code of the cause.
func delegationError(err error, id string, peer discovery.Peer, o outcome) error {
	var hint string
	if o == outcomeUnknown {
		hint = fmt.Sprintf("it may be running: check `cflk show %s` before asking again", id)
	} else {
		hint = string(o)
	}
	var coded *domain.Error
	if errors.As(err, &coded) {
		return domain.NewError(coded.Code, fmt.Sprintf("job %s on %s: %s (%s)", id, peer.Name, coded.Message, hint), coded.Details)
	}
	return fmt.Errorf("job %s on %s: %w (%s)", id, peer.Name, err, hint)
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
