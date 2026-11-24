// Package server is the side of `cflk peer` that answers other peers: it reads
// one request from a stream, runs it on the local use cases and writes one
// response. It knows the wire and nothing of how Jobs run.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/wire"
)

// RunInput is a Job that another peer delegates. Its code is already in the
// local mirror of Repo, under refs/cflk/<Job>/g1, at Commit.
type RunInput struct {
	Job       string
	Harness   string
	Repo      string
	Commit    string
	CreatedBy string
	Prompt    string
}

// MoveMode is which of the three movement commands a move op is.
type MoveMode string

// The modes of a move.
const (
	MoveStart MoveMode = "start"
	MoveRetry MoveMode = "retry"
	MoveAbort MoveMode = "abort"
)

// MoveInput is a Job to move, to retry or to take back. Gen is the generation the
// sender expects the Job to be in.
type MoveInput struct {
	Job  string
	Gen  int
	To   string
	Mode MoveMode
}

// PrepareInput is a movement to receive: the code is in the local mirror under
// refs/cflk/<Job>/g<Gen> at Commit, and the session comes as the archive.
type PrepareInput struct {
	Job       string
	Gen       int
	Transfer  string
	Harness   string
	Repo      string
	Commit    string
	CreatedBy string
	History   []wire.HistoryEntry
}

// TransferInput names a movement the destination was told about.
type TransferInput struct {
	Job      string
	Gen      int
	Transfer string
}

// FinishInput is a Job whose owner is asked to stop and seal it for `pull --finish`;
// To is the peer that gets the commit.
type FinishInput struct {
	Job string
	Gen int
	To  string
}

// FinishNotice names a finish the owner has under way: the notice that its
// commit arrived, or the order to give it up. Finish may be empty in an abort.
type FinishNotice struct {
	Job    string
	Gen    int
	Finish string
}

// Local is what this peer does with its own Jobs: the same use cases the local
// commands run, so that a request from another peer and a command typed here
// cannot differ.
type Local interface {
	Hello(ctx context.Context) (wire.Hello, error)
	List(ctx context.Context) ([]wire.JobView, error)
	Show(ctx context.Context, id string) (wire.JobView, error)
	// Stop, Resume and Send check gen (when it is not zero) under the lock of the
	// Job, in the same hold as the change.
	Stop(ctx context.Context, id string, gen int) (wire.JobView, error)
	Resume(ctx context.Context, id string, gen int, force bool) (wire.JobView, error)
	Screen(ctx context.Context, id string) (string, error)
	Send(ctx context.Context, id string, gen int, text string) (wire.JobView, error)
	// RunFromMirror starts the Job of a delegation.
	RunFromMirror(ctx context.Context, in RunInput) (wire.JobView, error)
	// Mirror makes sure the bare mirror of repo exists and returns its path.
	Mirror(ctx context.Context, repo string) (string, error)
	// Move moves, or repeats the move of, or takes back the move of a Job that is here.
	Move(ctx context.Context, in MoveInput) (wire.JobView, error)
	// Prepare receives a movement: the worktree and the session archive, nothing started.
	Prepare(ctx context.Context, in PrepareInput, archive []byte) (wire.Transfer, error)
	// Activate starts the Job of a prepared movement.
	Activate(ctx context.Context, in TransferInput) (wire.JobView, error)
	// AbortTransfer gives a movement up, unless it was activated already.
	AbortTransfer(ctx context.Context, in TransferInput) (wire.Transfer, error)
	// Finish stops and seals the Job, writes `finishing` and sends the commit to In.To.
	Finish(ctx context.Context, in FinishInput) (wire.Finish, error)
	// FinishDone marks done the Job whose finish arrived; FinishAbort gives the finish up.
	FinishDone(ctx context.Context, in FinishNotice) (wire.JobView, error)
	FinishAbort(ctx context.Context, in FinishNotice) (wire.JobView, error)
	// AttachHere attaches the terminal of this process to the Job's, and fails if
	// there is no terminal: it never opens a window.
	AttachHere(ctx context.Context, id string, readOnly bool) error
}

// Server answers the requests of other peers.
type Server struct{ Local Local }

// maxMessage bounds the text of an error that goes to another peer.
const maxMessage = 1024

// Serve answers the one request on in with one response on out. A request that
// is not valid is answered, when it can be, with CF_INVALID_REQUEST and returned.
func (s Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	req, body, err := wire.ReadRequest(in)
	if err != nil {
		_ = wire.WriteResponse(out, failure(domain.NewError(domain.CodeInvalidRequest, err.Error(), nil)), nil)
		return err
	}
	resp, respBody, err := s.dispatch(ctx, req, body)
	if err != nil {
		resp, respBody = failure(err), nil
	}
	return wire.WriteResponse(out, resp, respBody)
}

// Attach is `cflk peer attach [--read-only] <job>`: the one op whose stdin is a
// terminal, so its operands are the argv and are validated like a header's.
func (s Server) Attach(ctx context.Context, id string, readOnly bool) error {
	if !job.ValidID(id) {
		return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%q is not a Job id", id), nil)
	}
	return s.Local.AttachHere(ctx, id, readOnly)
}

func (s Server) dispatch(ctx context.Context, req wire.Request, body []byte) (wire.Response, []byte, error) {
	if req.Op == wire.OpShow || req.Op == wire.OpScreen {
		if err := s.expectGen(ctx, req); err != nil {
			return wire.Response{}, nil, err
		}
	}
	switch req.Op {
	case wire.OpHello:
		h, err := s.Local.Hello(ctx)
		return wire.Response{Hello: &h}, nil, err
	case wire.OpPS:
		jobs, err := s.Local.List(ctx)
		if err != nil {
			return wire.Response{}, nil, err
		}
		if len(jobs) > wire.MaxListedJobs {
			return wire.Response{}, nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("this peer has %d Jobs: more than a listing carries (%d)", len(jobs), wire.MaxListedJobs), nil)
		}
		views := make([]wire.JobView, len(jobs))
		for i, v := range jobs {
			views[i] = v.Clamp()
		}
		return wire.Response{Jobs: views}, nil, nil
	case wire.OpShow:
		return one(s.Local.Show(ctx, req.Job))
	case wire.OpStop:
		return one(s.Local.Stop(ctx, req.Job, req.Gen))
	case wire.OpResume:
		return one(s.Local.Resume(ctx, req.Job, req.Gen, req.Force))
	case wire.OpSend:
		return one(s.Local.Send(ctx, req.Job, req.Gen, string(body)))
	case wire.OpScreen:
		screen, err := s.Local.Screen(ctx, req.Job)
		return wire.Response{}, []byte(screen), err
	case wire.OpRun:
		return one(s.Local.RunFromMirror(ctx, RunInput{
			Job: req.Job, Harness: req.Harness, Repo: req.Repo, Commit: req.Commit, CreatedBy: req.CreatedBy, Prompt: string(body),
		}))
	case wire.OpMirror:
		path, err := s.Local.Mirror(ctx, req.Repo)
		return wire.Response{Mirror: path}, nil, err
	case wire.OpMove:
		return one(s.Local.Move(ctx, MoveInput{Job: req.Job, Gen: req.Gen, To: req.To, Mode: MoveStart}))
	case wire.OpMoveRetry:
		return one(s.Local.Move(ctx, MoveInput{Job: req.Job, Gen: req.Gen, Mode: MoveRetry}))
	case wire.OpMoveAbort:
		return one(s.Local.Move(ctx, MoveInput{Job: req.Job, Gen: req.Gen, Mode: MoveAbort}))
	case wire.OpPrepare:
		t, err := s.Local.Prepare(ctx, PrepareInput{
			Job: req.Job, Gen: req.Gen, Transfer: req.Transfer, Harness: req.Harness, Repo: req.Repo,
			Commit: req.Commit, CreatedBy: req.CreatedBy, History: req.History,
		}, body)
		return wire.Response{Transfer: &t}, nil, err
	case wire.OpActivate:
		return one(s.Local.Activate(ctx, TransferInput{Job: req.Job, Gen: req.Gen, Transfer: req.Transfer}))
	case wire.OpAbortTransfer:
		t, err := s.Local.AbortTransfer(ctx, TransferInput{Job: req.Job, Gen: req.Gen, Transfer: req.Transfer})
		return wire.Response{Transfer: &t}, nil, err
	case wire.OpFinish:
		f, err := s.Local.Finish(ctx, FinishInput{Job: req.Job, Gen: req.Gen, To: req.To})
		return wire.Response{Finish: &f}, nil, err
	case wire.OpFinishDone:
		return one(s.Local.FinishDone(ctx, FinishNotice{Job: req.Job, Gen: req.Gen, Finish: req.Finish}))
	case wire.OpFinishAbort:
		return one(s.Local.FinishAbort(ctx, FinishNotice{Job: req.Job, Gen: req.Gen, Finish: req.Finish}))
	}
	return wire.Response{}, nil, domain.NewError(domain.CodeInvalidRequest, "unknown op", nil)
}

// expectGen refuses a read (show, screen) of a generation the Job is not in. It
// reads the Job before, with no lock: nothing changes. The orders that change
// the Job check the generation under its lock.
func (s Server) expectGen(ctx context.Context, req wire.Request) error {
	if req.Gen == 0 {
		return nil
	}
	v, err := s.Local.Show(ctx, req.Job)
	if err != nil {
		return err
	}
	if v.Gen != req.Gen || v.State == string(job.Moved) {
		return domain.NewError(domain.CodeStaleLease, fmt.Sprintf("job %s is at generation %d (%s), not %d", req.Job, v.Gen, v.State, req.Gen), nil)
	}
	return nil
}

func one(v wire.JobView, err error) (wire.Response, []byte, error) {
	v = v.Clamp()
	return wire.Response{Job: &v}, nil, err
}

func failure(err error) wire.Response {
	var coded *domain.Error
	code, msg := string(domain.CodeInternal), err.Error()
	if errors.As(err, &coded) {
		code, msg = string(coded.Code), coded.Message
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > maxMessage {
		msg = strings.ToValidUTF8(msg[:maxMessage], "")
	}
	return wire.Response{Error: &wire.Error{Code: code, Message: msg}}
}
