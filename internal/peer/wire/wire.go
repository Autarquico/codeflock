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
	"reflect"
	"regexp"
	"strings"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/job"
)

// MaxHeader is the longest header line, newline excluded.
const MaxHeader = 64 << 10

// MaxBody is the longest body of the ops of this package (a prompt, a text, a
// screen). A session archive has the limits of harness.SessionLimit instead.
const MaxBody = 1 << 20

// MaxArchive is the longest body of a prepare: the session archive, which has
// the limits of harness.SessionLimit (the archive is compressed, so it is never
// longer than what it holds).
const MaxArchive = harness.DefaultMaxSessionBytes

// MaxHistory is the most history entries a prepare carries (the latest ones).
const MaxHistory = 32

// MaxListedJobs is the most Jobs one ps answers with, and MaxListBody the
// longest body that carries them: the list travels in the body, not in the
// header, so that a long history cannot overflow the header limit. With the
// text of every view bounded (maxViewText) MaxListedJobs views fit in MaxListBody.
const (
	MaxListedJobs = 1000
	MaxListBody   = 8 << 20
)

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
	// OpMove, OpMoveRetry and OpMoveAbort ask the peer that owns a Job to move it
	// to another peer, to repeat a movement left unconfirmed, or to take it back.
	OpMove      Op = "move"
	OpMoveRetry Op = "move-retry"
	OpMoveAbort Op = "move-abort"
	// OpPrepare, OpActivate and OpAbortTransfer are what the source of a movement
	// says to its destination: receive the code and the session and start nothing,
	// start the agent, give the movement up.
	OpPrepare       Op = "prepare"
	OpActivate      Op = "activate"
	OpAbortTransfer Op = "abort-transfer"
	// OpFinish asks the peer that owns a Job to stop it, seal it and send the commit
	// to the peer named in `to`; OpFinishDone says the commit arrived, and the Job is
	// done; OpFinishAbort gives the finish up.
	OpFinish      Op = "finish"
	OpFinishDone  Op = "finish-done"
	OpFinishAbort Op = "finish-abort"
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
	// To is the peer a move sends the Job to.
	To string `json:"to,omitempty"`
	// Transfer identifies a movement; History is the ownership of the Job so far (the
	// latest MaxHistory entries); Force asks a resume to go ahead of a movement
	// that was not confirmed.
	Transfer string         `json:"transfer,omitempty"`
	History  []HistoryEntry `json:"history,omitempty"`
	Force    bool           `json:"force,omitempty"`
	// Finish identifies a `pull --finish` the owner has under way.
	Finish string `json:"finish,omitempty"`
	// Size is the length of the body that follows: the prompt of a run, the text
	// of a send, the session archive of a prepare.
	Size int64 `json:"size,omitempty"`
}

// HistoryEntry is one ownership step of a Job.
type HistoryEntry struct {
	Gen    int    `json:"gen"`
	Peer   string `json:"peer"`
	Commit string `json:"commit,omitempty"`
}

// shape says which fields an op takes.
type shape struct {
	job, gen, run, repo, body bool
	// genRequired: the generation is a positive number, not "whatever it is".
	genRequired bool
	// to, transfer, history and force are the fields of a movement.
	to, transfer, history, force bool
	// archive: the body is a session archive and may be as long as one.
	archive bool
	// finish: the id of a finish is allowed, and required if finishRequired.
	finish, finishRequired bool
}

var shapes = map[Op]shape{
	OpHello:  {},
	OpPS:     {},
	OpShow:   {job: true, gen: true},
	OpStop:   {job: true, gen: true},
	OpResume: {job: true, gen: true, force: true},
	OpScreen: {job: true, gen: true},
	OpSend:   {job: true, gen: true, body: true},
	OpRun:    {job: true, run: true, repo: true, body: true},
	OpMirror: {repo: true},

	OpMove:      {job: true, gen: true, genRequired: true, to: true},
	OpMoveRetry: {job: true, gen: true, genRequired: true},
	OpMoveAbort: {job: true, gen: true, genRequired: true},
	OpPrepare: {
		job: true, gen: true, genRequired: true, run: true, repo: true, body: true, archive: true,
		transfer: true, history: true,
	},
	OpActivate:      {job: true, gen: true, genRequired: true, transfer: true},
	OpAbortTransfer: {job: true, gen: true, genRequired: true, transfer: true},

	OpFinish:      {job: true, gen: true, genRequired: true, to: true},
	OpFinishDone:  {job: true, gen: true, genRequired: true, finish: true, finishRequired: true},
	OpFinishAbort: {job: true, gen: true, genRequired: true, finish: true},
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

// checkPresent refuses a header that names a field its op does not take, even
// with a zero value: "absent" is not the same as "0" or "".
func (r Request) checkPresent(keys map[string]bool) error {
	sh := shapes[r.Op]
	for key, allowed := range map[string]bool{
		"job": sh.job, "gen": sh.gen, "repo": sh.repo, "size": sh.body,
		"harness": sh.run, "commit": sh.run, "created_by": sh.run,
		"to": sh.to, "transfer": sh.transfer, "history": sh.history, "force": sh.force, "finish": sh.finish,
	} {
		if keys[key] && !allowed {
			return protocolf("op %s: field %s is not allowed", r.Op, key)
		}
	}
	return nil
}

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
	case r.Gen < 0 || (!sh.gen && r.Gen != 0) || (sh.genRequired && r.Gen == 0):
		return protocolf("op %s: gen %d is not valid", r.Op, r.Gen)
	case sh.repo != (r.Repo != ""):
		return protocolf("op %s: repo is %s", r.Op, requiredOrForbidden(sh.repo))
	case r.Repo != "" && !validRepo(r.Repo):
		return protocolf("%q is not a normalized repo", r.Repo)
	case sh.body && (r.Size <= 0 || r.Size > sh.bodyLimit()):
		return protocolf("op %s: size %d is not between 1 and %d", r.Op, r.Size, sh.bodyLimit())
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
	return r.validateTransfer(sh)
}

func (sh shape) bodyLimit() int64 {
	if sh.archive {
		return MaxArchive
	}
	return MaxBody
}

// validateTransfer checks the fields of a movement.
func (r Request) validateTransfer(sh shape) error {
	switch {
	case sh.transfer != (r.Transfer != ""):
		return protocolf("op %s: transfer is %s", r.Op, requiredOrForbidden(sh.transfer))
	case r.Transfer != "" && !job.ValidTransferID(r.Transfer):
		return protocolf("%q is not a transfer id", r.Transfer)
	case sh.to != (r.To != ""):
		return protocolf("op %s: to is %s", r.Op, requiredOrForbidden(sh.to))
	case r.To != "" && !ValidName(r.To):
		return protocolf("%q cannot name a peer", r.To)
	case !sh.finish && r.Finish != "", sh.finishRequired && r.Finish == "":
		return protocolf("op %s: finish is %s", r.Op, requiredOrForbidden(sh.finishRequired))
	case r.Finish != "" && !job.ValidTransferID(r.Finish):
		return protocolf("%q is not a finish id", r.Finish)
	case !sh.history && len(r.History) > 0:
		return protocolf("op %s takes no history", r.Op)
	case len(r.History) > MaxHistory:
		return protocolf("a history of %d entries", len(r.History))
	case r.Force && !sh.force:
		return protocolf("op %s cannot be forced", r.Op)
	}
	for _, e := range r.History {
		if e.Gen < 1 || !ValidName(e.Peer) || (e.Commit != "" && !ValidCommit(e.Commit)) {
			return protocolf("a history entry is not valid")
		}
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
	// MovedTo and MovedGen say where a Job that is moved lives now.
	MovedTo  string `json:"moved_to,omitempty"`
	MovedGen int    `json:"moved_gen,omitempty"`
}

// maxViewText bounds each text field of a view.
const maxViewText = 512

func (v *JobView) texts() []*string {
	return []*string{&v.Peer, &v.Repo, &v.Branch, &v.Harness, &v.SessionID, &v.Terminal, &v.Problem, &v.MovedTo}
}

// Clamp cuts the text fields of the view to what a peer accepts.
func (v JobView) Clamp() JobView {
	for _, f := range v.texts() {
		if len(*f) > maxViewText {
			*f = strings.ToValidUTF8((*f)[:maxViewText], "")
		}
	}
	return v
}

// Validate refuses a view whose identity fields a peer could use to mislead,
// or whose text is longer than the bound: the rest is text that is only ever printed.
func (v JobView) Validate() error {
	if !job.ValidID(v.ID) || !job.State(v.State).Valid() || v.Gen < 0 || v.MovedGen < 0 {
		return protocolf("job view %q is not valid", v.ID)
	}
	for _, f := range v.texts() {
		if len(*f) > maxViewText {
			return protocolf("job view %q has text longer than %d bytes", v.ID, maxViewText)
		}
	}
	return nil
}

// Transfer states: what a destination says of a movement.
const (
	TransferPrepared  = "prepared"
	TransferActivated = "activated"
	TransferAborted   = "aborted"
)

// CodeTransferAborted is the error of an order for a movement that is given up:
// the destination has not started anything and never will for that transfer.
const CodeTransferAborted domain.Code = "CF_TRANSFER_ABORTED"

// Transfer is what a destination answers about a movement.
type Transfer struct {
	ID    string `json:"id"`
	Gen   int    `json:"gen"`
	State string `json:"state"`
}

// Validate checks the shape of what a peer says about a movement.
func (t Transfer) Validate() error {
	switch t.State {
	case TransferPrepared, TransferActivated, TransferAborted:
	default:
		return protocolf("transfer state %q", t.State)
	}
	if !job.ValidTransferID(t.ID) || t.Gen < 1 {
		return protocolf("transfer %q is not valid", t.ID)
	}
	return nil
}

// Finish is what the owner answers to a finish: the id of the finish and the
// exact commit it sealed, which it has sent to the peer that asked.
type Finish struct {
	ID     string `json:"id"`
	Commit string `json:"commit"`
}

// Validate checks the shape of what a peer says about a finish.
func (f Finish) Validate() error {
	if !job.ValidTransferID(f.ID) || !ValidCommit(f.Commit) {
		return protocolf("finish %q is not valid", f.ID)
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
	Error  *Error   `json:"error,omitempty"`
	Hello  *Hello   `json:"hello,omitempty"`
	Job    *JobView `json:"job,omitempty"`
	Mirror string   `json:"mirror,omitempty"`
	// Transfer is the answer of prepare and abort-transfer.
	Transfer *Transfer `json:"transfer,omitempty"`
	// Finish is the answer of finish.
	Finish *Finish `json:"finish,omitempty"`
	// Listing says the body is the list of Jobs (a ps), which WriteResponse
	// writes from Jobs and ReadResponse reads into it. An empty list is a
	// non-nil empty Jobs.
	Listing bool      `json:"listing,omitempty"`
	Jobs    []JobView `json:"-"`
	// Size is the length of the body that follows: the screen of a screen op, or the list.
	Size int64 `json:"size,omitempty"`
}

// Limits on what a response may carry: the text of a peer is only ever printed,
// so it is bounded rather than interpreted.
const (
	maxErrorMessage = 4096
	maxHarnesses    = 32
	maxHelloText    = 256
	maxMirrorPath   = 4096
)

var errorCodePattern = regexp.MustCompile(`^CF_[A-Z0-9_]{1,60}$`)

// Validate checks that the response says one thing (an error or a result, never
// both), the identity fields of the views, the size and the bounds of the text.
func (r Response) Validate() error {
	limit := int64(MaxBody)
	if r.Listing {
		limit = MaxListBody
	}
	if r.Size < 0 || r.Size > limit || (r.Listing && r.Size == 0) {
		return protocolf("response size %d is not valid", r.Size)
	}
	if len(r.Jobs) > MaxListedJobs {
		return protocolf("%d Jobs in one listing", len(r.Jobs))
	}
	results := 0
	for _, present := range []bool{r.Hello != nil, r.Job != nil, r.Listing || r.Jobs != nil, r.Mirror != "", r.Transfer != nil, r.Finish != nil} {
		if present {
			results++
		}
	}
	if r.Error != nil {
		if results > 0 || r.Size != 0 {
			return protocolf("response carries an error and a result")
		}
		if !errorCodePattern.MatchString(r.Error.Code) || len(r.Error.Message) > maxErrorMessage {
			return protocolf("response error is not valid")
		}
	}
	if results > 1 {
		return protocolf("response carries more than one result")
	}
	if len(r.Mirror) > maxMirrorPath {
		return protocolf("mirror path of %d bytes", len(r.Mirror))
	}
	if r.Hello != nil {
		if err := r.Hello.Validate(); err != nil {
			return err
		}
	}
	if r.Job != nil {
		if err := r.Job.Validate(); err != nil {
			return err
		}
	}
	if r.Transfer != nil {
		if err := r.Transfer.Validate(); err != nil {
			return err
		}
	}
	if r.Finish != nil {
		if err := r.Finish.Validate(); err != nil {
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

// Expect checks that the response is the answer to req: the result its op
// gives, about the Job that was asked, and nothing else.
func (r Response) Expect(req Request) error {
	var ok bool
	bare := r.Transfer == nil && r.Finish == nil
	switch req.Op {
	case OpHello:
		ok = r.Hello != nil && r.Job == nil && !r.Listing && r.Mirror == "" && r.Size == 0 && bare
	case OpPS:
		ok = r.Listing && r.Hello == nil && r.Job == nil && r.Mirror == "" && bare
	case OpShow, OpStop, OpResume, OpSend, OpRun, OpMove, OpMoveRetry, OpMoveAbort, OpActivate, OpFinishDone, OpFinishAbort:
		ok = r.Job != nil && r.Job.ID == req.Job && r.Hello == nil && !r.Listing && r.Mirror == "" && r.Size == 0 && bare
	case OpScreen:
		ok = r.Hello == nil && r.Job == nil && !r.Listing && r.Mirror == "" && bare
	case OpMirror:
		ok = r.Mirror != "" && r.Hello == nil && r.Job == nil && !r.Listing && r.Size == 0 && bare
	case OpFinish:
		ok = r.Finish != nil && r.Hello == nil && r.Job == nil && !r.Listing && r.Mirror == "" && r.Size == 0 && r.Transfer == nil
	case OpPrepare, OpAbortTransfer:
		ok = r.Transfer != nil && r.Transfer.ID == req.Transfer && r.Transfer.Gen == req.Gen &&
			r.Hello == nil && r.Job == nil && !r.Listing && r.Mirror == "" && r.Size == 0
	}
	if !ok {
		return protocolf("the answer is not the one op %s asks for", req.Op)
	}
	return nil
}

// Validate bounds what a peer says about itself.
func (h Hello) Validate() error {
	if h.Jobs < 0 || len(h.Name) > maxHelloText || len(h.Version) > maxHelloText || len(h.Harnesses) > maxHarnesses {
		return protocolf("hello is not valid")
	}
	for _, name := range h.Harnesses {
		if !harnessPattern.MatchString(name) {
			return protocolf("hello: %q is not a harness name", name)
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
// header may not carry a field the struct does not declare, a field spelled
// other than as declared, a field twice, a null, nor anything after its JSON
// value; a body shorter or longer than declared is refused.
func ReadFrame(r io.Reader, header any, declaredSize func() int64) ([]byte, error) {
	body, _, err := readFrame(r, header, declaredSize, MaxBody)
	return body, err
}

// readFrame is ReadFrame with the longest body it takes, and says which fields
// the header named.
func readFrame(r io.Reader, header any, declaredSize func() int64, limit int64) ([]byte, map[string]bool, error) {
	br := bufio.NewReader(r)
	line, err := readLine(br)
	if err != nil {
		return nil, nil, err
	}
	keys, err := strictKeys(line, reflect.TypeOf(header))
	if err != nil {
		return nil, nil, protocolf("header: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(header); err != nil {
		return nil, nil, protocolf("header: %v", err)
	}
	size := declaredSize()
	if size < 0 || size > limit {
		return nil, nil, protocolf("body of %d bytes", size)
	}
	// The body grows as it arrives: a header that declares a long one does not make us allocate it.
	var buf bytes.Buffer
	if n, err := io.CopyN(&buf, br, size); err != nil || n != size {
		return nil, nil, protocolf("body truncated: declared %d bytes", size)
	}
	body := buf.Bytes()
	if body == nil {
		body = []byte{}
	}
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		return nil, nil, protocolf("data after the declared body")
	}
	return body, keys, nil
}

// strictKeys checks what encoding/json lets through: the JSON text must be one
// value of type t whose object keys are exactly the declared names (the
// decoder accepts any case), none twice and no value null. It returns the keys
// of the top-level object.
func strictKeys(data []byte, t reflect.Type) (map[string]bool, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	keys := map[string]bool{}
	if err := walk(dec, t, keys); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the JSON value")
	}
	return keys, nil
}

// walk consumes one JSON value that should be a t. keys, if not nil, collects
// the keys of the object.
func walk(dec *json.Decoder, t reflect.Type, keys map[string]bool) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return errors.New("null is not a value")
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch {
	case delim == '{' && t.Kind() == reflect.Struct:
		fields := declaredFields(t)
		seen := map[string]bool{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := keyTok.(string)
			ft, declared := fields[key]
			switch {
			case !declared:
				return fmt.Errorf("unknown field %q", key)
			case seen[key]:
				return fmt.Errorf("field %q twice", key)
			}
			seen[key] = true
			if keys != nil {
				keys[key] = true
			}
			if err := walk(dec, ft, nil); err != nil {
				return err
			}
		}
	case delim == '[' && t.Kind() == reflect.Slice:
		for dec.More() {
			if err := walk(dec, t.Elem(), nil); err != nil {
				return err
			}
		}
	default:
		// Not the shape of t: the decoder says so; here the value is only skipped.
		for depth := 1; depth > 0; {
			tok, err := dec.Token()
			if err != nil {
				return err
			}
			if d, ok := tok.(json.Delim); ok {
				if d == '{' || d == '[' {
					depth++
				} else {
					depth--
				}
			}
		}
		return nil
	}
	_, err = dec.Token()
	return err
}

// declaredFields is the field of a struct by the exact name of its json tag.
func declaredFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for f := range t.Fields() {
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" && name != "-" {
			fields[name] = f.Type
		}
	}
	return fields
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
	body, keys, err := readFrame(r, &req, func() int64 {
		if sh, ok := shapes[req.Op]; ok && req.Size > sh.bodyLimit() {
			return -1 // longer than its op takes: refused before any of it is read
		}
		return req.Size
	}, MaxArchive)
	if err != nil {
		return Request{}, nil, err
	}
	if err := req.Validate(); err != nil {
		return Request{}, nil, err
	}
	if err := req.checkPresent(keys); err != nil {
		return Request{}, nil, err
	}
	return req, body, nil
}

// ReadResponse reads and validates a response and its body.
func ReadResponse(r io.Reader) (Response, []byte, error) {
	var resp Response
	body, _, err := readFrame(r, &resp, func() int64 { return resp.Size }, MaxListBody)
	if err != nil {
		return Response{}, nil, err
	}
	if err := resp.Validate(); err != nil {
		return Response{}, nil, err
	}
	if resp.Listing {
		if resp.Jobs, err = decodeJobs(body); err != nil {
			return Response{}, nil, err
		}
		if err := resp.Validate(); err != nil {
			return Response{}, nil, err
		}
	}
	return resp, body, nil
}

// decodeJobs reads the body of a listing: a JSON array of views, read as strictly as a header.
func decodeJobs(body []byte) ([]JobView, error) {
	jobs := []JobView{}
	if _, err := strictKeys(body, reflect.TypeOf(jobs)); err != nil {
		return nil, protocolf("listing: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&jobs); err != nil {
		return nil, protocolf("listing: %v", err)
	}
	return jobs, nil
}

// WriteRequest validates and writes a request.
func WriteRequest(w io.Writer, req Request, body []byte) error {
	req.Size = int64(len(body))
	if err := req.Validate(); err != nil {
		return err
	}
	return WriteFrame(w, req, body)
}

// WriteResponse writes a response with body. A response with Jobs (non-nil) is
// a listing: its body is the list, and body must be empty.
func WriteResponse(w io.Writer, resp Response, body []byte) error {
	if resp.Jobs != nil {
		if len(body) > 0 || len(resp.Jobs) > MaxListedJobs {
			return protocolf("a listing of %d Jobs with a body of %d bytes", len(resp.Jobs), len(body))
		}
		list, err := json.Marshal(resp.Jobs)
		if err != nil {
			return err
		}
		if len(list) > MaxListBody {
			return protocolf("listing of %d bytes", len(list))
		}
		resp.Listing, body = true, list
	}
	resp.Size = int64(len(body))
	return WriteFrame(w, resp, body)
}
