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
// with no shell and `--` before the URL. The URL is made to rewrite to itself
// (insteadOf and pushInsteadOf), and the longest matching rule wins, so no rule of
// the repository sends the code elsewhere.
func PushArgv(t PushTarget, commit, ref string) []string { return pushArgv(t, commit+":"+ref) }

// ForcePushArgv is PushArgv for a ref that may hold another commit: the ref of a
// movement that was given up and is made again with the work done since.
func ForcePushArgv(t PushTarget, commit, ref string) []string { return pushArgv(t, "+"+commit+":"+ref) }

func pushArgv(t PushTarget, refspec string) []string {
	argv := []string{"git"}
	for _, c := range t.Config {
		argv = append(argv, "-c", c)
	}
	argv = append(argv, "-c", "url."+t.URL+".insteadOf="+t.URL, "-c", "url."+t.URL+".pushInsteadOf="+t.URL)
	return append(argv, "push", "--quiet", "--", t.URL, refspec)
}

// PushSpec is a commit to send to the mirror of a peer.
type PushSpec struct {
	// RepoDir is a checkout of the repository the commit is in.
	RepoDir string
	// Repo is its normalized origin, which names the mirror.
	Repo   string
	Commit string
	// Ref is where it goes in the mirror: refs/cflk/<job>/g<gen>.
	Ref string
	// Force lets the push replace a ref that holds another commit.
	Force bool
	// OrderTimeout bounds the question for the mirror and PushTimeout the push;
	// zero is the default.
	OrderTimeout, PushTimeout time.Duration
}

// PushCode asks the peer for the mirror of the repo, checks that it is the mirror
// of that repo, and pushes the commit to the ref in it. The conversation is not in git.
func PushCode(ctx context.Context, t Transport, peer discovery.Peer, sp PushSpec) error {
	askCtx, cancel := context.WithTimeout(ctx, cmp.Or(sp.OrderTimeout, DefaultOrderTimeout))
	mirror, _, err := Call(askCtx, t, peer, wire.Request{Op: wire.OpMirror, Repo: sp.Repo}, nil)
	cancel()
	if err != nil {
		return err
	}
	if want := job.RepoHash(sp.Repo) + ".git"; path.Base(mirror.Mirror) != want {
		return fmt.Errorf("answered the mirror %q, not the one of this repo", path.Base(mirror.Mirror))
	}
	target, err := t.PushTarget(peer, mirror.Mirror)
	if err != nil {
		return err
	}
	pushCtx, cancel := context.WithTimeout(ctx, cmp.Or(sp.PushTimeout, DefaultPushTimeout))
	defer cancel()
	argv := PushArgv(target, sp.Commit, sp.Ref)
	if sp.Force {
		argv = ForcePushArgv(target, sp.Commit, sp.Ref)
	}
	if err := runGit(pushCtx, sp.RepoDir, argv); err != nil {
		return fmt.Errorf("sending the code: %w", err)
	}
	return nil
}

// pushEnv is the environment of the push: the caller's, without what could
// change where git sends the code or how it reaches it (GIT_SSH_COMMAND and
// GIT_SSH win over core.sshCommand; GIT_CONFIG_* add settings), and with the
// system and global git configuration off, whose url.*.insteadOf rules would
// rewrite the peer's URL. Pushing over ssh needs nothing from them: the identity
// is ssh's.
func pushEnv(environ []string) []string {
	env := make([]string, 0, len(environ)+3)
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if name == "GIT_SSH_COMMAND" || name == "GIT_SSH" || strings.HasPrefix(name, "GIT_CONFIG") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
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
	retry := fmt.Sprintf("cflk run --id %s --on %s --harness %s --branch %s -- <the same prompt>", id, peer.Name, d.Harness, d.Commit)
	order := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, cmp.Or(d.OrderTimeout, DefaultOrderTimeout))
	}
	if err := PushCode(ctx, t, peer, PushSpec{
		RepoDir: d.RepoDir, Repo: d.Repo, Commit: d.Commit, Ref: ref,
		OrderTimeout: d.OrderTimeout, PushTimeout: d.PushTimeout,
	}); err != nil {
		return wire.JobView{}, delegationError(err, id, peer, outcomeNothing, retry)
	}
	runCtx, cancel := order()
	defer cancel()
	resp, _, err := Call(runCtx, t, peer, wire.Request{
		Op: wire.OpRun, Job: id, Harness: d.Harness, Repo: d.Repo, Commit: d.Commit, CreatedBy: d.CreatedBy,
	}, []byte(d.Prompt))
	if err != nil {
		return wire.JobView{}, delegationError(err, id, peer, outcomeOfRun(ctx, t, peer, id, err, cmp.Or(d.OrderTimeout, DefaultOrderTimeout)), retry)
	}
	if resp.Job == nil || resp.Job.ID != id {
		return wire.JobView{}, delegationError(errors.New("answered the run without the Job that was asked"), id, peer, outcomeUnknown, retry)
	}
	return *resp.Job, nil
}

type outcome string

const (
	outcomeNothing outcome = "nothing was started"
	outcomeRefused outcome = "the peer refused it"
	outcomeUnknown outcome = "it may be running"
	// outcomeExists is a Job the peer has even though the run failed: its launch
	// went wrong and it was kept, because its agent may be alive.
	outcomeExists outcome = "the Job exists on the peer"
)

// outcomeOfRun is what a failed run ask says about the Job. A stream that broke
// after the request left may have started it. A peer that answered with an error
// usually did not, but may have kept the Job (a launch that could not confirm its
// agent ended), so the peer is asked about the Job: there is no refusal to report
// when it is there.
func outcomeOfRun(ctx context.Context, t Transport, peer discovery.Peer, id string, err error, timeout time.Duration) outcome {
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code == domain.CodePeerUnreachable || coded.Code == domain.CodePeerNoAnswer {
		return outcomeUnknown
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, _, probeErr := Call(probeCtx, t, peer, wire.Request{Op: wire.OpShow, Job: id}, nil)
	switch {
	case probeErr == nil && resp.Job != nil:
		return outcomeExists
	case errors.As(probeErr, &coded) && coded.Code == domain.CodeNotFound:
		return outcomeRefused
	}
	return outcomeUnknown
}

// delegationError names the Job and the peer in a failure of any step, says
// whether the Job exists, so the person can `cflk show` it, and gives the command
// that asks again for the same Job (`cflk run --id`). It keeps the code of the cause.
func delegationError(err error, id string, peer discovery.Peer, o outcome, retry string) error {
	hint := fmt.Sprintf("%s; retry with: %s", o, retry)
	switch o {
	case outcomeUnknown:
		hint = fmt.Sprintf("it may be running: check `cflk show %s` before asking again; if it is not there, retry with: %s", id, retry)
	case outcomeExists:
		hint = fmt.Sprintf("the Job exists on the peer and its agent may be running: check `cflk show %s`, not a new run", id)
	}
	var coded *domain.Error
	if errors.As(err, &coded) {
		return domain.NewError(coded.Code, fmt.Sprintf("job %s on %s: %s (%s)", id, peer.Name, coded.Message, hint), coded.Details)
	}
	return fmt.Errorf("job %s on %s: %w (%s)", id, peer.Name, err, hint)
}

func runGit(ctx context.Context, dir string, argv []string) error {
	// The protected configuration is off, and with it the caller's safe.directory:
	// the repository was already trusted when it was resolved, so say so.
	argv = append([]string{argv[0], "-c", "safe.directory=" + dir}, argv[1:]...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // PushArgv: a fixed argv, no shell
	cmd.Dir = dir
	cmd.Env = pushEnv(os.Environ())
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
