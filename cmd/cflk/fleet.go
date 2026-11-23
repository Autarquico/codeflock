package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
	"github.com/Autarquico/codeflock/internal/adapters/runtime"
	"github.com/Autarquico/codeflock/internal/attachwindow"
	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/remote"
	"github.com/Autarquico/codeflock/internal/peer/wire"
)

// queryTimeout is how long a peer has to answer a question (ps, show, hello)
// before it counts as unreachable: a fleet listing must not wait for a dead peer.
const queryTimeout = 4 * time.Second

// listBudget is how long `cflk ps` may take in all: discovery and the peers
// share it, and the local Jobs are read meanwhile.
const listBudget = 4500 * time.Millisecond

// fleet is cli.Jobs over this peer and the others: every command works on a Job
// wherever it is. The peers are the discovered ones, and a peer is only ever
// reached through the Transport.
type fleet struct {
	local     *localJobs
	transport remote.Transport
	discover  func(context.Context) (discovery.Result, error)
	windows   attachwindow.Runner
	// timeout is how long a peer has to answer a question.
	timeout time.Duration
	// budget is how long a listing may take in all; zero is listBudget.
	budget time.Duration
	// orderTimeout is how long a peer has to carry out an order (stop, send,
	// screen, resume); zero is remote.DefaultOrderTimeout.
	orderTimeout time.Duration
}

var _ cli.Jobs = (*fleet)(nil)

// discoverPeers is discovery over the real Tailscale and the config file of the
// user. Tailscale answering slowly must not hold a command.
func discoverPeers(ctx context.Context) (discovery.Result, error) {
	path, err := discovery.DefaultConfigPath()
	if err != nil {
		return discovery.Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	return discovery.Discover(ctx, discovery.Options{ConfigPath: path})
}

type reply struct {
	peer discovery.Peer
	resp wire.Response
	body []byte
	err  error
}

// ask sends req to every peer at once, each with its own timeout, and returns
// the answers in the order of the peers. A peer that Tailscale says is offline
// is not dialed.
func (f *fleet) ask(ctx context.Context, peers []discovery.Peer, req wire.Request) []reply {
	replies := make([]reply, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		replies[i].peer = p
		if !p.Online {
			replies[i].err = domain.NewError(domain.CodePeerUnreachable, "offline (Tailscale)", nil)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, f.timeout)
			defer cancel()
			replies[i].resp, replies[i].body, replies[i].err = remote.Call(ctx, f.transport, p, req, nil)
		}()
	}
	wg.Wait()
	return replies
}

func (f *fleet) selfName() (string, error) {
	p, err := f.local.open()
	if err != nil {
		return "", err
	}
	return p.name, nil
}

func (f *fleet) isThis(name string) (bool, error) {
	self, err := f.selfName()
	return name == "local" || name == self, err
}

// List is the Jobs of this peer and the others. Discovery and the peers share
// one budget; this peer's Jobs are read while the others answer.
func (f *fleet) List(ctx context.Context, only string) (cli.JobList, error) {
	budget, cancel := context.WithTimeout(ctx, cmp.Or(f.budget, listBudget))
	defer cancel()
	res, err := f.discover(budget)
	if err != nil {
		return cli.JobList{}, err
	}
	wantLocal, peers := true, res.Peers
	if only != "" {
		this, err := f.isThis(only)
		if err != nil {
			return cli.JobList{}, err
		}
		wantLocal, peers = this, nil
		if !this {
			p, err := findPeer(res, only)
			if err != nil {
				return cli.JobList{}, err
			}
			peers = []discovery.Peer{p}
		}
	}
	var local []cli.JobView
	var localErr error
	localDone := make(chan struct{})
	go func() {
		defer close(localDone)
		if wantLocal {
			local, localErr = f.local.List(ctx)
		}
	}()
	replies := f.ask(budget, peers, wire.Request{Op: wire.OpPS})
	<-localDone
	if localErr != nil {
		return cli.JobList{}, localErr
	}
	list := cli.JobList{Jobs: local}
	for _, r := range replies {
		if r.err != nil {
			list.Unreachable = append(list.Unreachable, cli.UnreachablePeer{Peer: r.peer.Name, Problem: r.err.Error()})
			continue
		}
		for _, v := range r.resp.Jobs {
			list.Jobs = append(list.Jobs, cli.JobView(v))
		}
	}
	return list, nil
}

func (f *fleet) Peers(ctx context.Context) ([]cli.PeerView, error) {
	res, err := f.discover(ctx)
	if err != nil {
		return nil, err
	}
	self, err := f.selfName()
	if err != nil {
		return nil, err
	}
	hello, err := peerService{f.local}.Hello(ctx)
	if err != nil {
		return nil, err
	}
	views := []cli.PeerView{{Name: self, This: true, Reachable: true, Harnesses: hello.Harnesses, Jobs: hello.Jobs}}
	for _, r := range f.ask(ctx, res.Peers, wire.Request{Op: wire.OpHello}) {
		v := cli.PeerView{Name: r.peer.Name}
		if r.err != nil {
			v.Problem = r.err.Error()
		} else if r.resp.Hello != nil {
			v.Reachable, v.Harnesses, v.Jobs = true, r.resp.Hello.Harnesses, r.resp.Hello.Jobs
		}
		views = append(views, v)
	}
	return views, nil
}

// findPeer is the discovered peer a person named; the name never becomes a host
// unless discovery lists it.
func findPeer(res discovery.Result, name string) (discovery.Peer, error) {
	if p, ok := res.Find(name); ok {
		return p, nil
	}
	known := make([]string, len(res.Peers))
	for i, p := range res.Peers {
		known[i] = p.Name
	}
	msg := fmt.Sprintf("no peer named %q (peers: %s)", name, strings.Join(known, ", "))
	if len(res.Warnings) > 0 {
		msg += "; " + strings.Join(res.Warnings, "; ")
	}
	return discovery.Peer{}, domain.NewError(domain.CodeInvalidRequest, msg, nil)
}

// codeOf is the code of a domain error, or empty.
func codeOf(err error) domain.Code {
	var coded *domain.Error
	if errors.As(err, &coded) {
		return coded.Code
	}
	return ""
}

// owner is where a Job lives: this peer (peer is nil) or another.
type owner struct {
	peer *discovery.Peer
	view cli.JobView
}

// locate finds the peer that owns the Job. This peer is asked first and, if it
// has the Job and does not say it moved, it is the owner: no other peer is
// asked. Otherwise all the others are, at once, and the owner is the one that
// has it with the highest generation and not moved.
func (f *fleet) locate(ctx context.Context, id string) (owner, error) {
	if !job.ValidID(id) {
		return owner{}, invalid("%q is not a Job id (job-xxxxxxxxxx)", id)
	}
	v, err := f.local.Show(ctx, id)
	switch {
	case err == nil && v.State != string(job.Moved):
		return owner{view: v}, nil
	case err != nil && codeOf(err) != domain.CodeNotFound:
		return owner{}, err
	}
	res, err := f.discover(ctx)
	if err != nil {
		return owner{}, err
	}
	var found *owner
	var silent []string
	for _, r := range f.ask(ctx, res.Peers, wire.Request{Op: wire.OpShow, Job: id}) {
		switch {
		case codeOf(r.err) == domain.CodeNotFound:
		case r.err != nil || r.resp.Job == nil || r.resp.Job.ID != id:
			silent = append(silent, r.peer.Name)
		case r.resp.Job.State == string(job.Moved):
		case found == nil || r.resp.Job.Gen > found.view.Gen:
			found = &owner{peer: &r.peer, view: cli.JobView(*r.resp.Job)}
		}
	}
	if found != nil {
		return *found, nil
	}
	if len(silent) > 0 {
		return owner{}, domain.NewError(domain.CodePeerNoAnswer,
			fmt.Sprintf("job %s is not on a peer that answered (no answer from: %s)", id, strings.Join(silent, ", ")), nil)
	}
	return owner{}, domain.NewError(domain.CodeNotFound, fmt.Sprintf("no peer has job %s", id), nil)
}

// call is op on the Job at its owner, expecting the generation that was found.
func (f *fleet) call(ctx context.Context, o owner, op wire.Op, body []byte) (wire.Response, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(f.orderTimeout, remote.DefaultOrderTimeout))
	defer cancel()
	return remote.Call(ctx, f.transport, *o.peer, wire.Request{Op: op, Job: o.view.ID, Gen: o.view.Gen}, body)
}

// jobOp runs a command that answers a Job: here, or at its owner.
func (f *fleet) jobOp(ctx context.Context, id string, op wire.Op, body []byte, here func() (cli.JobView, error)) (cli.JobView, error) {
	o, err := f.locate(ctx, id)
	if err != nil {
		return cli.JobView{}, err
	}
	if o.peer == nil {
		return here()
	}
	resp, _, err := f.call(ctx, o, op, body)
	if err != nil {
		return cli.JobView{}, err
	}
	if resp.Job == nil {
		return cli.JobView{}, fmt.Errorf("peer %s answered %s without a Job", o.peer.Name, op)
	}
	return cli.JobView(*resp.Job), nil
}

func (f *fleet) Show(ctx context.Context, id string) (cli.JobView, error) {
	o, err := f.locate(ctx, id)
	return o.view, err
}

func (f *fleet) Stop(ctx context.Context, id string) (cli.JobView, error) {
	return f.jobOp(ctx, id, wire.OpStop, nil, func() (cli.JobView, error) { return f.local.Stop(ctx, id) })
}

func (f *fleet) Resume(ctx context.Context, id string) (cli.JobView, error) {
	return f.jobOp(ctx, id, wire.OpResume, nil, func() (cli.JobView, error) { return f.local.Resume(ctx, id) })
}

func (f *fleet) Send(ctx context.Context, id, text string) (cli.JobView, error) {
	return f.jobOp(ctx, id, wire.OpSend, []byte(text), func() (cli.JobView, error) { return f.local.Send(ctx, id, text) })
}

func (f *fleet) Screen(ctx context.Context, id string) (string, error) {
	o, err := f.locate(ctx, id)
	if err != nil {
		return "", err
	}
	if o.peer == nil {
		return f.local.Screen(ctx, id)
	}
	_, body, err := f.call(ctx, o, wire.OpScreen, nil)
	return string(body), err
}

func (f *fleet) Attach(ctx context.Context, id string, readOnly bool) error {
	o, err := f.locate(ctx, id)
	if err != nil {
		return err
	}
	if o.peer == nil {
		return f.local.Attach(ctx, id, readOnly)
	}
	if state := job.State(o.view.State); !runsAgent(state) {
		return notRunning(id, state, domain.CodeRunNotAttachable)
	}
	argv, err := f.transport.AttachArgv(*o.peer, id, readOnly)
	if err != nil {
		return err
	}
	return enter(ctx, f.windows, runtime.Command{Path: argv[0], Args: argv[1:]}, id)
}

// wireHarness is the name of a harness in a request to a peer.
func wireHarness(name string) (string, error) {
	switch harnessIDs[name] {
	case claude.ID:
		return "claude", nil
	case codex.ID:
		return "codex", nil
	}
	return "", invalid("unknown harness %q: use claude or codex", name)
}

func (f *fleet) Run(ctx context.Context, req cli.RunRequest) (cli.JobView, error) {
	if req.Prompt == "" {
		return cli.JobView{}, invalid("the prompt is empty: say what the agent should do")
	}
	this, err := f.isThis(req.On)
	if err != nil {
		return cli.JobView{}, err
	}
	if req.On == "" || this {
		return f.local.Run(ctx, req)
	}
	harness, err := wireHarness(req.Harness)
	if err != nil {
		return cli.JobView{}, err
	}
	if len(req.Prompt) > wire.MaxBody {
		return cli.JobView{}, invalid("the prompt is %d bytes: a Job sent to a peer takes at most %d", len(req.Prompt), wire.MaxBody)
	}
	createdBy, err := f.selfName()
	if err != nil {
		return cli.JobView{}, err
	}
	if !wire.ValidName(createdBy) {
		return cli.JobView{}, invalid("the name of this machine, %q, cannot travel to a peer: use letters, digits, '.', '_' and '-' (hostname or CFLK machine name)", createdBy)
	}
	res, err := f.discover(ctx)
	if err != nil {
		return cli.JobView{}, err
	}
	target, err := f.target(ctx, res, req.On, harness)
	if err != nil {
		return cli.JobView{}, err
	}
	if target == nil {
		req.On = ""
		return f.local.Run(ctx, req)
	}
	src, err := sourceOf(ctx, req.Branch)
	if err != nil {
		return cli.JobView{}, err
	}
	v, err := remote.Delegate(ctx, f.transport, *target, remote.Delegation{
		RepoDir: src.dir, Repo: src.repo, Commit: src.commit, Harness: harness, CreatedBy: createdBy, Prompt: req.Prompt,
		OrderTimeout: f.orderTimeout,
	})
	return cli.JobView(v), err
}

// target is the peer a run goes to: the one named, or for "any" the reachable
// peer with the fewest Jobs that has the harness (this peer counts, and is nil).
func (f *fleet) target(ctx context.Context, res discovery.Result, on, harness string) (*discovery.Peer, error) {
	if on != "any" {
		p, err := findPeer(res, on)
		if err != nil {
			return nil, err
		}
		if !p.Online {
			return nil, domain.NewError(domain.CodePeerUnreachable, fmt.Sprintf("peer %s is offline (Tailscale)", p.Name), nil)
		}
		return &p, nil
	}
	var best *discovery.Peer
	fewest := -1
	if hello, err := (peerService{f.local}).Hello(ctx); err == nil && slices.Contains(hello.Harnesses, harness) {
		fewest = hello.Jobs
	}
	for _, r := range f.ask(ctx, res.Peers, wire.Request{Op: wire.OpHello}) {
		h := r.resp.Hello
		if r.err == nil && h != nil && slices.Contains(h.Harnesses, harness) && (fewest < 0 || h.Jobs < fewest) {
			best, fewest = &r.peer, h.Jobs
		}
	}
	if fewest < 0 {
		return nil, domain.NewError(domain.CodeNoCandidatePeer, "no reachable peer has "+harness, nil)
	}
	return best, nil
}

// connectTimeout is how long ssh has to reach a peer.
const connectTimeout = 3 * time.Second

// newFleet is the fleet of this binary: the peers of Tailscale and the config
// file, reached over ssh.
func newFleet(local *localJobs) *fleet {
	return &fleet{
		local:     local,
		transport: remote.SSH{ConnectTimeout: connectTimeout},
		discover:  discoverPeers,
		windows:   attachwindow.ExecRunner{},
		timeout:   queryTimeout,
	}
}
