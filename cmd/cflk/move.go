package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/remote"
	"github.com/Autarquico/codeflock/internal/peer/runner"
	"github.com/Autarquico/codeflock/internal/peer/server"
	"github.com/Autarquico/codeflock/internal/peer/wire"
	"github.com/Autarquico/codeflock/internal/worker/gitops"
)

// This file is the source of a movement: the peer that owns the Job stops it,
// seals its work, reserves the move and hands the code and the conversation to
// the destination (move_recv.go). The rule is the spec's: the source keeps the
// Job until the destination confirms it started, and any doubt leaves the Job
// `moving` until a person resolves it with --retry or --abort.

// network is how this peer reaches the others.
type network struct {
	transport remote.Transport
	discover  func(context.Context) (discovery.Result, error)
}

// faults lets a test stop a movement dead at a named point, right after a
// durable write and before its effect, as a crash would. Production has none.
type faults interface{ At(point string) error }

func (p *peer) fault(point string) error {
	if p.faults == nil {
		return nil
	}
	return p.faults.At(point)
}

// The points of the source of a movement.
const (
	pointReserved  = "move:reserved"  // `moving` written
	pointPushed    = "move:pushed"    // the code is in the destination's mirror
	pointPrepared  = "move:prepared"  // the destination has the session and a worktree
	pointActivated = "move:activated" // the destination started the Job
	pointMoved     = "move:moved"     // `moved` written
	pointAborted   = "move:aborted"   // `stopped` written after an abort
)

// Deadlines of the orders of a movement: a prepare carries the code's session, an
// activate waits for the agent to start.
const (
	prepareTimeout  = remote.DefaultPushTimeout
	activateTimeout = runner.DefaultStartTimeout + runner.DefaultDiscoverTimeout + remote.DefaultOrderTimeout
	// moveTimeout is how long a peer has to carry out a whole move that someone
	// else asked it for: stop, seal, push, prepare and activate.
	moveTimeout = stopTimeout + 2*remote.DefaultPushTimeout + activateTimeout
)

// move is the use case behind `cflk move` and the move ops: the Job is here.
func (l *localJobs) move(ctx context.Context, in server.MoveInput) (cli.JobView, error) {
	p, err := l.open()
	if err != nil {
		return cli.JobView{}, err
	}
	if l.net == nil {
		return cli.JobView{}, invalid("this peer has no way to reach the others")
	}
	if !job.ValidID(in.Job) {
		return cli.JobView{}, invalid("%q is not a Job id (job-xxxxxxxxxx)", in.Job)
	}
	switch in.Mode {
	case server.MoveRetry:
		return l.moveRetry(ctx, p, in)
	case server.MoveAbort:
		return l.moveAbort(ctx, p, in)
	}
	return l.moveStart(ctx, p, in)
}

func (l *localJobs) moveStart(ctx context.Context, p *peer, in server.MoveInput) (cli.JobView, error) {
	if !wire.ValidName(p.name) {
		return cli.JobView{}, invalid("the name of this machine, %q, cannot travel to a peer: use letters, digits, '.', '_' and '-'", p.name)
	}
	target, err := l.findTarget(ctx, in.To)
	if err != nil {
		return cli.JobView{}, err
	}
	if target.Name == p.name {
		return cli.JobView{}, invalid("job %s is on %s already", in.Job, p.name)
	}
	if !target.Online {
		return cli.JobView{}, domain.NewError(domain.CodePeerUnreachable, fmt.Sprintf("peer %s is offline (Tailscale)", target.Name), nil)
	}
	j, _, err := l.read(ctx, in.Job)
	if err != nil {
		return cli.JobView{}, err
	}
	if err := movable(j, in.Gen); err != nil {
		return cli.JobView{}, err
	}
	if err := p.runner.Stop(ctx, in.Job, runner.Expect(in.Gen)); err != nil {
		return cli.JobView{}, mapErr(err)
	}
	sealed, archive, err := p.reserve(ctx, in, target.Name)
	if err != nil {
		return cli.JobView{}, err
	}
	return l.deliver(ctx, p, target, sealed, archive)
}

// movable says why a Job cannot start a movement, if it cannot.
func movable(j job.Job, gen int) error {
	if err := j.ExpectGen(gen); err != nil {
		return mapErr(err)
	}
	switch j.State {
	case job.Moving:
		return movingError(j)
	case job.Done, job.Finishing, job.Moved:
		return domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("job %s is %s: it cannot be moved", j.ID, j.State), nil)
	}
	return nil
}

func movingError(j job.Job) error {
	to := ""
	if j.Move != nil {
		to = " to " + j.Move.To
	}
	return domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf(
		"job %s is moving%s: `cflk move --retry %s` repeats the move, `cflk move --abort %s` gives it up", j.ID, to, j.ID, j.ID), nil)
}

// reserve is steps 2 and 3 of a movement, under the Job's lock: the work is
// sealed into a commit, the session is exported, and `moving` is written with
// the transfer. The Job returned is the record as written.
func (p *peer) reserve(ctx context.Context, in server.MoveInput, to string) (job.Job, []byte, error) {
	var sealed job.Job
	var archive []byte
	err := p.store.WithLockSave(in.Job, func(j *job.Job, save func() error) error {
		if err := movable(*j, in.Gen); err != nil {
			return err
		}
		if j.State != job.Stopped {
			return domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("job %s is %s, not stopped: it cannot be moved until `cflk stop` ends its agent", j.ID, j.State), nil)
		}
		if j.SessionID == "" {
			return domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("job %s has no conversation to move: its agent never opened a session", j.ID), nil)
		}
		wt := p.store.WorktreeDir(j.ID, j.Gen)
		if err := cleanSubmodules(ctx, wt); err != nil {
			return err
		}
		commit, err := p.seal(ctx, *j)
		if err != nil {
			return fmt.Errorf("sealing the work of job %s: %w", j.ID, err)
		}
		if archive, err = p.exportSession(ctx, *j); err != nil {
			return err
		}
		if err := j.Transition(job.Moving); err != nil {
			return err
		}
		j.Move = &job.Move{To: to, Gen: j.Gen + 1, Transfer: job.NewTransferID(), Commit: commit}
		if err := save(); err != nil {
			return err
		}
		sealed = *j
		return p.fault(pointReserved)
	})
	return sealed, archive, mapErr(err)
}

func (p *peer) exportSession(ctx context.Context, j job.Job) ([]byte, error) {
	exp, err := p.sessions.Export(ctx, j.SessionID, j.Harness)
	if err != nil {
		return nil, fmt.Errorf("exporting the conversation of job %s: %w", j.ID, err)
	}
	return exp.Archive, nil
}

// seal commits what the worktree has, tracked or not and not ignored, with the
// guards of gitops (what a sealing leaves out, never the files of the user's
// secrets by name), and keeps the commit reachable under refs/cflk/<job>/g<gen+1>
// in the repository of the worktree. The branch and the files of the worktree
// are not touched: a movement that is given up leaves the Job as it was.
func (p *peer) seal(ctx context.Context, j job.Job) (string, error) {
	wt := p.store.WorktreeDir(j.ID, j.Gen)
	if err := os.MkdirAll(filepath.Join(p.root, "worker"), 0o700); err != nil {
		return "", err
	}
	g, err := gitops.New(ctx, gitops.Options{Paths: config.Paths{DataDir: p.root}, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		return "", err
	}
	head, err := g.Head(ctx, wt)
	if err != nil {
		return "", err
	}
	sealed, err := g.Snapshot(ctx, wt, gitops.SnapshotIntent{Project: j.ID, ParentOID: head, At: time.Now(), Exclude: config.DefaultCheckpointExclude()}, gitops.Forbidden{})
	if err != nil {
		return "", err
	}
	ref, err := wire.MirrorRef(j.ID, j.Gen+1)
	if err != nil {
		return "", err
	}
	if _, err := git(ctx, wt, "update-ref", ref, sealed.Commit); err != nil {
		return "", err
	}
	return sealed.Commit, nil
}

// cleanSubmodules refuses a worktree with a submodule that has changes of its
// own: they would not travel with the commit.
func cleanSubmodules(ctx context.Context, wt string) error {
	out, err := git(ctx, wt, "status", "--porcelain=v2", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 9 && (f[0] == "1" || f[0] == "2") && len(f[2]) == 4 && f[2][0] == 'S' && (f[2][2] == 'M' || f[2][3] == 'U') {
			return domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("the submodule %s has changes that a move would not take: commit or discard them first", f[len(f)-1]), nil)
		}
	}
	return nil
}

// deliver is steps 4 to 6 for the Job as reserved: push the code, prepare, activate
// and, with the destination's ok, write `moved`. Whatever else happens leaves the
// Job moving; an activate the destination refuses before starting anything is the
// one answer that sends it back to stopped.
func (l *localJobs) deliver(ctx context.Context, p *peer, target discovery.Peer, j job.Job, archive []byte) (cli.JobView, error) {
	mv := *j.Move
	ref, err := wire.MirrorRef(j.ID, mv.Gen)
	if err != nil {
		return cli.JobView{}, err
	}
	harness, err := wireHarness(j.Harness)
	if err != nil {
		return cli.JobView{}, err
	}
	if err := remote.PushCode(ctx, l.net.transport, target, remote.PushSpec{
		RepoDir: p.store.WorktreeDir(j.ID, j.Gen), Repo: j.Repo, Commit: mv.Commit, Ref: ref, Force: true,
	}); err != nil {
		return cli.JobView{}, stuck(j, target, err)
	}
	if err := p.fault(pointPushed); err != nil {
		return cli.JobView{}, err
	}
	if _, _, err := l.order(ctx, target, prepareTimeout, wire.Request{
		Op: wire.OpPrepare, Job: j.ID, Gen: mv.Gen, Transfer: mv.Transfer, Harness: harness, Repo: j.Repo, Commit: mv.Commit,
		CreatedBy: j.CreatedBy, History: wireHistory(j.History),
	}, archive); err != nil {
		return cli.JobView{}, stuck(j, target, err)
	}
	if err := p.fault(pointPrepared); err != nil {
		return cli.JobView{}, err
	}
	_, _, err = l.order(ctx, target, activateTimeout, wire.Request{Op: wire.OpActivate, Job: j.ID, Gen: mv.Gen, Transfer: mv.Transfer}, nil)
	if codeOf(err) == wire.CodeTransferAborted {
		if _, endErr := p.endMove(j.ID, mv, job.Stopped); endErr != nil {
			return cli.JobView{}, errors.Join(err, endErr)
		}
		return cli.JobView{}, domain.NewError(wire.CodeTransferAborted, fmt.Sprintf(
			"%s did not start job %s, which is stopped here: `cflk resume %s` resumes it (%s)", target.Name, j.ID, j.ID, cleanMessage(err)), nil)
	}
	if err != nil {
		return cli.JobView{}, stuck(j, target, err)
	}
	if err := p.fault(pointActivated); err != nil {
		return cli.JobView{}, err
	}
	moved, err := p.endMove(j.ID, mv, job.Moved)
	if err != nil {
		return cli.JobView{}, err
	}
	if err := p.fault(pointMoved); err != nil {
		return cli.JobView{}, err
	}
	return view(moved, ""), nil
}

// stuck is the error of a movement that cannot say how it ended: the Job stays
// moving and the person resolves it.
func stuck(j job.Job, target discovery.Peer, err error) error {
	msg := fmt.Sprintf("job %s stays moving to %s (%s): `cflk move --retry %s` repeats the move, `cflk move --abort %s` gives it up",
		j.ID, target.Name, cleanMessage(err), j.ID, j.ID)
	if c := codeOf(err); c != "" {
		return domain.NewError(c, msg, nil)
	}
	return errors.New(msg)
}

func cleanMessage(err error) string {
	var coded *domain.Error
	if errors.As(err, &coded) {
		return coded.Message
	}
	return err.Error()
}

// wireHistory is the latest entries of the Job's history, as they travel.
func wireHistory(h []job.Entry) []wire.HistoryEntry {
	var out []wire.HistoryEntry
	for _, e := range h {
		if wire.ValidName(e.Peer) && (e.Commit == "" || wire.ValidCommit(e.Commit)) {
			out = append(out, wire.HistoryEntry{Gen: e.Gen, Peer: e.Peer, Commit: e.Commit})
		}
	}
	if len(out) > wire.MaxHistory {
		out = out[len(out)-wire.MaxHistory:]
	}
	return out
}

// endMove writes the end of a movement under the Job's lock, only if the Job is
// still moving under that transfer: moved (the destination has it) or stopped (the
// movement was given up). Anything else means another command resolved it first.
func (p *peer) endMove(id string, mv job.Move, to job.State) (job.Job, error) {
	var out job.Job
	err := p.store.WithLock(id, func(j *job.Job) error {
		defer func() { out = *j }()
		if j.State == to && j.Move == nil && (to != job.Moved || (j.MovedTo != nil && j.MovedTo.Gen == mv.Gen)) {
			return nil // the same resolution, written already
		}
		if j.State != job.Moving || j.Move == nil || j.Move.Transfer != mv.Transfer {
			return domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf(
				"job %s is %s: it changed while it moved, check `cflk show %s`", j.ID, j.State, j.ID), nil)
		}
		if err := j.Transition(to); err != nil {
			return err
		}
		j.Move = nil
		if to == job.Moved {
			j.MovedTo = &job.Location{Peer: mv.To, Gen: mv.Gen}
		}
		return nil
	})
	return out, mapErr(err)
}

// order sends one order of a movement to the destination with its own deadline.
func (l *localJobs) order(ctx context.Context, target discovery.Peer, timeout time.Duration, req wire.Request, body []byte) (wire.Response, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return remote.Call(ctx, l.net.transport, target, req, body)
}

func (l *localJobs) findTarget(ctx context.Context, name string) (discovery.Peer, error) {
	res, err := l.net.discover(ctx)
	if err != nil {
		return discovery.Peer{}, err
	}
	return findPeer(res, name)
}

// pending is the Job as it is while a movement waits to be resolved, and the
// destination the move named.
func (l *localJobs) pending(ctx context.Context, in server.MoveInput) (job.Job, discovery.Peer, error) {
	j, _, err := l.read(ctx, in.Job)
	if err != nil {
		return j, discovery.Peer{}, err
	}
	if err := j.ExpectGen(in.Gen); err != nil {
		return j, discovery.Peer{}, mapErr(err)
	}
	if j.State != job.Moving || j.Move == nil {
		return j, discovery.Peer{}, domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("job %s is %s: there is no move to resolve", j.ID, j.State), nil)
	}
	target, err := l.findTarget(ctx, j.Move.To)
	return j, target, err
}

// moveRetry repeats the push, the prepare and the activate of the move the Job
// is in, with the same transfer: what the destination already did it answers as
// it did, so the agent is never started twice.
func (l *localJobs) moveRetry(ctx context.Context, p *peer, in server.MoveInput) (cli.JobView, error) {
	j, target, err := l.pending(ctx, in)
	if err != nil {
		return cli.JobView{}, err
	}
	archive, err := p.exportSession(ctx, j)
	if err != nil {
		return cli.JobView{}, err
	}
	return l.deliver(ctx, p, target, j, archive)
}

// moveAbort asks the destination to give the transfer up. It only counts when the
// destination answers: if it activated the Job already, the Job is its and this
// peer writes `moved`; if it does not answer, nothing changes.
func (l *localJobs) moveAbort(ctx context.Context, p *peer, in server.MoveInput) (cli.JobView, error) {
	j, target, err := l.pending(ctx, in)
	if err != nil {
		return cli.JobView{}, err
	}
	mv := *j.Move
	resp, _, err := l.order(ctx, target, remote.DefaultOrderTimeout, wire.Request{Op: wire.OpAbortTransfer, Job: j.ID, Gen: mv.Gen, Transfer: mv.Transfer}, nil)
	if err != nil {
		return cli.JobView{}, domain.NewError(cmp.Or(codeOf(err), domain.CodePeerNoAnswer), fmt.Sprintf(
			"the abort is refused: %s did not confirm it (%s); job %s stays moving", target.Name, cleanMessage(err), j.ID), nil)
	}
	if resp.Transfer.State == wire.TransferActivated {
		if _, err := p.endMove(j.ID, mv, job.Moved); err != nil {
			return cli.JobView{}, err
		}
		return cli.JobView{}, domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf(
			"the abort failed: %s had started job %s already, which is moved there", target.Name, j.ID), nil)
	}
	stopped, err := p.endMove(j.ID, mv, job.Stopped)
	if err != nil {
		return cli.JobView{}, err
	}
	if err := p.fault(pointAborted); err != nil {
		return cli.JobView{}, err
	}
	return view(stopped, ""), nil
}
