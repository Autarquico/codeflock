// Package wire is the stream two peers speak over `ssh <peer> cflk peer`: one
// JSON header line (at most 64 KiB, an op from a closed list, no field that is
// not declared) followed by a body whose length the header declares. Everything a
// peer sends is untrusted: nothing here reaches a shell, and a stream that does
// not match its header is refused whole.
package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/Autarquico/codeflock/internal/peer/job"
)

// MaxHeader is the longest header line, newline excluded.
const MaxHeader = 64 << 10

// MaxBody is the longest body of the ops of this package (a prompt, a text, a
// screen). A session archive has the limits of harness.SessionLimit instead.
const MaxBody = 1 << 20

// ErrProtocol is the class of every refusal of a malformed stream.
var ErrProtocol = errors.New("wire: protocol error")

func protocolf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, args...))
}

// Op is what a request asks for. The set is closed.
type Op string

// The ops. attach is not one of them: it is the one command that needs the
// terminal on stdin, so it has an argv of its own (`cflk peer attach`).
const (
	OpHello  Op = "hello"
	OpPS     Op = "ps"
	OpShow   Op = "show"
	OpRun    Op = "run"
	OpScreen Op = "screen"
	OpSend   Op = "send"
	OpStop   Op = "stop"
	OpResume Op = "resume"
	// OpMirror makes sure the bare mirror of a repo exists and says where it is,
	// so that the sender can push the Job's code to it.
	OpMirror Op = "mirror"
)

// Request is the header of what a peer asks. Fields an op does not use must be
// absent.
type Request struct {
	Op Op `json:"op"`
	// Job is the id of the Job the op acts on, or, for run, the id the sender
	// drew and under which the code is already in the mirror.
	Job string `json:"job,omitempty"`
	// Gen is the generation the sender expects the Job to be in.
	Gen int `json:"gen,omitempty"`
	// Harness, Repo, Commit and CreatedBy describe the Job of a run.
	Harness   string `json:"harness,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Commit    string `json:"commit,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	// Size is the length of the body that follows: the prompt of a run, the text of a send.
	Size int64 `json:"size,omitempty"`
}

// shape says which fields an op takes.
type shape struct {
	job, gen, run, repo, body bool
}

var shapes = map[Op]shape{
	OpHello:  {},
	OpPS:     {},
	OpShow:   {job: true, gen: true},
	OpStop:   {job: true, gen: true},
	OpResume: {job: true, gen: true},
	OpScreen: {job: true, gen: true},
	OpSend:   {job: true, gen: true, body: true},
	OpRun:    {job: true, run: true, repo: true, body: true},
	OpMirror: {repo: true},
}

var (
	commitPattern  = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	harnessPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	repoPattern    = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]{1,5})?(/[A-Za-z0-9._-]+){2,}$`)
	namePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// ValidName reports whether s can name a peer: letters, digits, '.', '_' and
// '-', not starting with a symbol, so that it is never an option for ssh or git.
func ValidName(s string) bool { return namePattern.MatchString(s) }

// ValidCommit reports whether s is a full git object id.
func ValidCommit(s string) bool { return commitPattern.MatchString(s) }

// Validate checks the header against the shape of its op.
func (r Request) Validate() error {
	sh, ok := shapes[r.Op]
	if !ok {
		return protocolf("unknown op %q", r.Op)
	}
	switch {
	case sh.job != (r.Job != ""):
		return protocolf("op %s: job is %s", r.Op, requiredOrForbidden(sh.job))
	case r.Job != "" && !job.ValidID(r.Job):
		return protocolf("%q is not a Job id", r.Job)
	case r.Gen < 0 || (!sh.gen && r.Gen != 0):
		return protocolf("op %s: gen %d is not valid", r.Op, r.Gen)
	case sh.repo != (r.Repo != ""):
		return protocolf("op %s: repo is %s", r.Op, requiredOrForbidden(sh.repo))
	case r.Repo != "" && !validRepo(r.Repo):
		return protocolf("%q is not a normalized repo", r.Repo)
	case sh.body && (r.Size <= 0 || r.Size > MaxBody):
		return protocolf("op %s: size %d is not between 1 and %d", r.Op, r.Size, MaxBody)
	case !sh.body && r.Size != 0:
		return protocolf("op %s takes no body", r.Op)
	}
	if sh.run {
		if !harnessPattern.MatchString(r.Harness) || !ValidCommit(r.Commit) || !ValidName(r.CreatedBy) {
			return protocolf("op run: harness, commit or created_by is not valid")
		}
	} else if r.Harness != "" || r.Commit != "" || r.CreatedBy != "" {
		return protocolf("op %s takes no run fields", r.Op)
	}
	return nil
}

func requiredOrForbidden(required bool) string {
	if required {
		return "required"
	}
	return "not allowed"
}

// validRepo accepts the shape NormalizeRepo produces: a lowercase host with an
// optional port, then at least owner and repo, each of plain characters and
// neither dotted nor ending in ".git".
func validRepo(s string) bool {
	if len(s) > 512 || !repoPattern.MatchString(s) || strings.HasSuffix(s, ".git") {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// MirrorRef is the immutable ref under which generation gen of a Job travels:
// refs/cflk/<job>/g<gen>.
func MirrorRef(jobID string, gen int) (string, error) {
	if !job.ValidID(jobID) || gen < 1 {
		return "", protocolf("no mirror ref for job %q gen %d", jobID, gen)
	}
	return fmt.Sprintf("refs/cflk/%s/g%d", jobID, gen), nil
}

// Error is how a response says that an op failed.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// JobView is a Job as peers exchange it.
type JobView struct {
	ID        string `json:"id"`
	Peer      string `json:"peer"`
	Repo      string `json:"repo"`
	Branch    string `json:"branch"`
	Harness   string `json:"harness"`
	State     string `json:"state"`
	Gen       int    `json:"gen"`
	SessionID string `json:"session_id,omitempty"`
	Terminal  string `json:"terminal,omitempty"`
	Problem   string `json:"problem,omitempty"`
}

// Validate refuses a view whose identity fields a peer could use to mislead:
// the rest is text that is only ever printed.
func (v JobView) Validate() error {
	if !job.ValidID(v.ID) || !job.State(v.State).Valid() || v.Gen < 0 {
		return protocolf("job view %q is not valid", v.ID)
	}
	return nil
}

// Hello is what a peer says about itself.
type Hello struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Harnesses []string `json:"harnesses"`
	Jobs      int      `json:"jobs"`
}

// Response is the header of what a peer answers. Exactly one of Error or the
// result of the op is present.
type Response struct {
	Error  *Error    `json:"error,omitempty"`
	Hello  *Hello    `json:"hello,omitempty"`
	Job    *JobView  `json:"job,omitempty"`
	Jobs   []JobView `json:"jobs,omitempty"`
	Mirror string    `json:"mirror,omitempty"`
	// Size is the length of the body that follows: the screen of a screen op.
	Size int64 `json:"size,omitempty"`
}

// Validate checks the identity fields of the views and the size.
func (r Response) Validate() error {
	if r.Size < 0 || r.Size > MaxBody {
		return protocolf("response size %d is not valid", r.Size)
	}
	if r.Job != nil {
		if err := r.Job.Validate(); err != nil {
			return err
		}
	}
	for _, v := range r.Jobs {
		if err := v.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// WriteFrame writes the header line and then body, whose length must be the
// Size the header declares (the caller sets it).
func WriteFrame(w io.Writer, header any, body []byte) error {
	line, err := json.Marshal(header)
	if err != nil {
		return err
	}
	if len(line) > MaxHeader {
		return protocolf("header of %d bytes", len(line))
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	_, err = w.Write(body)
	return err
}

// ReadFrame reads one header into header (a pointer to a struct) and then
// declaredSize(header) bytes of body, and requires the stream to end there. The
// header may not carry a field the struct does not declare, nor anything after
// its JSON value; a body shorter or longer than declared is refused.
func ReadFrame(r io.Reader, header any, declaredSize func() int64) ([]byte, error) {
	br := bufio.NewReader(r)
	line, err := readLine(br)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(header); err != nil {
		return nil, protocolf("header: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, protocolf("header: data after the JSON value")
	}
	size := declaredSize()
	if size < 0 || size > MaxBody {
		return nil, protocolf("body of %d bytes", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, protocolf("body truncated: declared %d bytes", size)
	}
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		return nil, protocolf("data after the declared body")
	}
	return body, nil
}

// readLine reads up to the newline, refusing a header longer than MaxHeader.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, protocolf("header: no complete line")
		}
		if b == '\n' {
			return line, nil
		}
		if len(line) >= MaxHeader {
			return nil, protocolf("header longer than %d bytes", MaxHeader)
		}
		line = append(line, b)
	}
}

// ReadRequest reads and validates a request and its body.
func ReadRequest(r io.Reader) (Request, []byte, error) {
	var req Request
	body, err := ReadFrame(r, &req, func() int64 { return req.Size })
	if err != nil {
		return Request{}, nil, err
	}
	if err := req.Validate(); err != nil {
		return Request{}, nil, err
	}
	return req, body, nil
}

// ReadResponse reads and validates a response and its body.
func ReadResponse(r io.Reader) (Response, []byte, error) {
	var resp Response
	body, err := ReadFrame(r, &resp, func() int64 { return resp.Size })
	if err != nil {
		return Response{}, nil, err
	}
	if err := resp.Validate(); err != nil {
		return Response{}, nil, err
	}
	return resp, body, nil
}

// WriteRequest validates and writes a request.
func WriteRequest(w io.Writer, req Request, body []byte) error {
	req.Size = int64(len(body))
	if err := req.Validate(); err != nil {
		return err
	}
	return WriteFrame(w, req, body)
}

// WriteResponse writes a response with body.
func WriteResponse(w io.Writer, resp Response, body []byte) error {
	resp.Size = int64(len(body))
	return WriteFrame(w, resp, body)
}
