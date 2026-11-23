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

// Local is what this peer does with its own Jobs: the same use cases the local
// commands run, so that a request from another peer and a command typed here
// cannot differ.
type Local interface {
	Hello(ctx context.Context) (wire.Hello, error)
	List(ctx context.Context) ([]wire.JobView, error)
	Show(ctx context.Context, id string) (wire.JobView, error)
	Stop(ctx context.Context, id string) (wire.JobView, error)
	Resume(ctx context.Context, id string) (wire.JobView, error)
	Screen(ctx context.Context, id string) (string, error)
	Send(ctx context.Context, id, text string) (wire.JobView, error)
	// RunFromMirror starts the Job of a delegation.
	RunFromMirror(ctx context.Context, in RunInput) (wire.JobView, error)
	// Mirror makes sure the bare mirror of repo exists and returns its path.
	Mirror(ctx context.Context, repo string) (string, error)
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
	if err := s.expectGen(ctx, req); err != nil {
		return wire.Response{}, nil, err
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
		return one(s.Local.Stop(ctx, req.Job))
	case wire.OpResume:
		return one(s.Local.Resume(ctx, req.Job))
	case wire.OpSend:
		return one(s.Local.Send(ctx, req.Job, string(body)))
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
	}
	return wire.Response{}, nil, domain.NewError(domain.CodeInvalidRequest, "unknown op", nil)
}

// expectGen refuses an order for a generation the Job is not in. It reads the
// Job before the order runs, so it is not under the Job's lock: F4 moves the
// check inside it.
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
