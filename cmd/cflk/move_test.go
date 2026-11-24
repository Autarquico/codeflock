//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/remote/remotetest"
	"github.com/Autarquico/codeflock/internal/peer/runner"
	"github.com/Autarquico/codeflock/internal/peer/server"
	"github.com/Autarquico/codeflock/internal/peer/sessions"
	"github.com/Autarquico/codeflock/internal/peer/wire"
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
)

const secretPlan = "the plan only this conversation knows"

// moveAgent is a Claude whose conversation is real to the sessions package: its
// first launch writes a transcript under the home of its peer, and every launch
// is logged with the session it resumed.
type moveAgent struct {
	script, home, peer, log string
	seq                     *int
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

const uuidFmt = "%08x-1111-4222-8333-%012x"

func (a moveAgent) Prepare(_ context.Context, in runner.Input) (runner.Plan, error) {
	id := in.SessionID
	if id == "" {
		*a.seq++
		id = fmt.Sprintf(uuidFmt, *a.seq, *a.seq)
		cwd, err := filepath.EvalSymlinks(in.Worktree)
		if err != nil {
			return runner.Plan{}, err
		}
		line, _ := json.Marshal(map[string]any{
			"type": "user", "sessionId": id, "cwd": cwd, "version": "2.1.319",
			"message": map[string]any{"role": "user", "content": secretPlan},
		})
		dir := filepath.Join(a.home, "projects", nonAlnum.ReplaceAllString(cwd, "-"))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return runner.Plan{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), append(line, '\n'), 0o600); err != nil {
			return runner.Plan{}, err
		}
	}
	f, err := os.OpenFile(a.log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // a test's log
	if err != nil {
		return runner.Plan{}, err
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", a.peer, id)
	return runner.Plan{
		Argv: []string{os.Args[0], "--script", a.script}, Env: []string{fakeharness.AsFakeharnessEnv + "=1"}, SessionID: id,
	}, nil
}

func (moveAgent) Discover(context.Context, runner.Input, time.Time, time.Time) ([]string, error) {
	return nil, nil
}

// errCrash is the end of a process at a crash point.
var errCrash = errors.New("crashed")

// crashFaults crashes a peer the next time it reaches an armed point.
type crashFaults struct {
	mu    sync.Mutex
	armed map[string]bool
	fired bool
	seen  []string
}

func (c *crashFaults) arm(point string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = map[string]bool{point: true}
}

func (c *crashFaults) At(point string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, point)
	if c.armed[point] {
		delete(c.armed, point)
		c.fired = true
		return errCrash
	}
	return nil
}

func (c *crashFaults) crashed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	fired := c.fired
	c.fired = false
	return fired
}

type movePeer struct {
	*localJobs
	name   string
	home   string
	faults *crashFaults
	have   bool // the harness is installed
}

// cut is a dial that fails: before the peer hears the order, or after it carried it out.
type cut struct {
	peer  string
	op    wire.Op
	after bool
}

type moveNet struct {
	t      *testing.T
	peers  map[string]*movePeer
	dialer *remotetest.Dialer
	found  []discovery.Peer
	log    string
	mu     sync.Mutex
	cuts   []cut
}

var errCut = errors.New("the connection was cut")

func newMoveNet(t *testing.T, steps []fakeharness.Step, names ...string) *moveNet {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("HOME", t.TempDir())
	repoWithOrigin(t)
	n := &moveNet{t: t, peers: map[string]*movePeer{}, dialer: &remotetest.Dialer{Peers: map[string]remotetest.ServeFunc{}}, log: filepath.Join(t.TempDir(), "launches")}
	raw, err := json.Marshal(fakeharness.Script{Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(script, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	seq := new(int)
	for _, name := range names {
		mp := &movePeer{name: name, home: t.TempDir(), faults: &crashFaults{}, have: true}
		ss := sessions.New()
		ss.ConfigDirs = map[string]string{claude.ID: mp.home}
		mp.localJobs = newLocalJobs("/cflk", overrides{
			name: name, root: shortDir(t), share: t.TempDir(), sessions: ss, faults: mp.faults,
			installed: func(string) bool { return mp.have },
			agents:    map[string]runner.Agent{claude.ID: moveAgent{script: script, home: mp.home, peer: name, log: n.log, seq: seq}},
			wrapper:   func(dir string) []string { return []string{"/usr/bin/env", asWrapper + "=" + dir, os.Args[0]} },
			stopper:   runner.Stopper{Grace: 200 * time.Millisecond, KillWait: 2 * time.Second, Poll: 10 * time.Millisecond},
		})
		p, err := mp.open()
		if err != nil {
			t.Fatal(err)
		}
		if err := p.tmux.Check(t.Context()); err != nil {
			t.Skipf("tmux: %v", err)
		}
		t.Cleanup(func() {
			ctx := context.WithoutCancel(t.Context())
			jobs, _ := p.store.List()
			for _, j := range jobs {
				_ = p.runner.Stop(ctx, j.ID)
			}
			_ = p.tmux.KillServer(ctx)
		})
		n.peers[name] = mp
		n.found = append(n.found, discovery.Peer{ID: "n" + name, Name: name, Host: name + ".ts.net", Online: true})
		n.dialer.Peers[name] = n.serve(mp)
	}
	for _, mp := range n.peers {
		mp.net = &network{transport: n.dialer, discover: n.discover}
	}
	return n
}

func (n *moveNet) discover(context.Context) (discovery.Result, error) {
	return discovery.Result{Peers: n.found}, nil
}

// serve is the peer behind the Dialer: it can be cut, and it can crash, which for
// the caller is an answer that never comes.
func (n *moveNet) serve(mp *movePeer) remotetest.ServeFunc {
	return func(ctx context.Context, in io.Reader, out io.Writer) error {
		req, body, err := wire.ReadRequest(in)
		if err != nil {
			return err
		}
		var replay bytes.Buffer
		if err := wire.WriteRequest(&replay, req, body); err != nil {
			return err
		}
		c, hit := n.takeCut(mp.name, req.Op)
		if hit && !c.after {
			return errCut
		}
		var answer bytes.Buffer
		serveErr := mp.servePeer(ctx, &replay, &answer)
		if mp.faults.crashed() || (hit && c.after) {
			return errCut
		}
		_, _ = out.Write(answer.Bytes())
		return serveErr
	}
}

func (n *moveNet) cutNext(peer string, op wire.Op, after bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cuts = append(n.cuts, cut{peer, op, after})
}

func (n *moveNet) takeCut(peer string, op wire.Op) (cut, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i, c := range n.cuts {
		if c.peer == peer && c.op == op {
			n.cuts = slices.Delete(n.cuts, i, i+1)
			return c, true
		}
	}
	return cut{}, false
}

// fleet is the CLI as typed on the named peer.
func (n *moveNet) fleet(name string) *fleet {
	f := &fleet{local: n.peers[name].localJobs, transport: n.dialer, discover: n.discover, timeout: 2 * time.Second}
	f.local.net = &network{transport: f.transport, discover: f.discover}
	return f
}

// launches is the agents started so far, as "peer session".
func (n *moveNet) launches() []string {
	raw, _ := os.ReadFile(n.log) //nolint:gosec // a test's log
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(raw)), "\n", "\n"))
}

func (n *moveNet) launchesOn(peer string) int {
	raw, _ := os.ReadFile(n.log) //nolint:gosec // a test's log
	count := 0
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.HasPrefix(line, peer+" ") {
			count++
		}
	}
	return count
}

func (n *moveNet) record(t *testing.T, peer, id string) job.Job {
	t.Helper()
	p, err := n.peers[peer].open()
	if err != nil {
		t.Fatal(err)
	}
	j, err := p.store.Get(id)
	if err != nil {
		t.Fatalf("%s: %v", peer, err)
	}
	return j
}

// started runs a Job on the first peer, with a conversation to move.
func (n *moveNet) started(t *testing.T, on string) cli.JobView {
	t.Helper()
	v, err := n.fleet(on).Run(t.Context(), cli.RunRequest{Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the Job runs", func() bool { return n.record(t, on, v.ID).SessionID != "" })
	return v
}

func mustMove(t *testing.T, f *fleet, req cli.MoveRequest) cli.JobView {
	t.Helper()
	v, err := f.Move(t.Context(), req)
	if err != nil {
		t.Fatalf("move %+v: %v", req, err)
	}
	return v
}

func requireCode(t *testing.T, err error, code domain.Code) {
	t.Helper()
	if codeOf(err) != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func TestAMovedJobKeepsItsWorkAndItsConversationAndOwnsOneAgent(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	was := n.record(t, "laptop", v.ID)
	untracked := filepath.Join(n.peers["laptop"].store().WorktreeDir(v.ID, 1), "notes.txt")
	if err := os.WriteFile(untracked, []byte("half done"), 0o600); err != nil {
		t.Fatal(err)
	}

	moved := mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, To: "mini"})
	if moved.State != "moved" || moved.MovedTo != "mini" || moved.MovedGen != 2 {
		t.Fatalf("source view = %+v", moved)
	}
	there := n.record(t, "mini", v.ID)
	if there.Gen != 2 || there.Peer != "mini" || there.State != job.Running && there.State != job.Waiting {
		t.Fatalf("destination = %+v", there)
	}
	if there.SessionID == was.SessionID || there.SessionID == "" {
		t.Fatalf("session %q must be new, not %q", there.SessionID, was.SessionID)
	}
	if got, _ := os.ReadFile(filepath.Join(n.peers["mini"].store().WorktreeDir(v.ID, 2), "notes.txt")); string(got) != "half done" {
		t.Fatalf("the untracked file did not travel: %q", got)
	}
	if n.launchesOn("mini") != 1 || !slices.Contains(n.launches(), there.SessionID) {
		t.Fatalf("launches = %v", n.launches())
	}
	transcript, err := exec.CommandContext(t.Context(), "grep", "-rl", secretPlan, filepath.Join(n.peers["mini"].home, "projects")).Output() //nolint:gosec // a test
	if err != nil || !strings.Contains(string(transcript), there.SessionID) {
		t.Fatalf("the conversation is not under the new session: %q %v", transcript, err)
	}
	for _, repo := range []string{n.peers["mini"].mirrorOf(t), "."} {
		out, err := exec.CommandContext(t.Context(), "git", "-C", repo, "log", "--all", "--oneline", "-S"+secretPlan).CombinedOutput() //nolint:gosec // a test
		if err != nil || len(out) != 0 {
			t.Fatalf("the conversation is in git, or git could not look: %s %v", out, err)
		}
	}

	// Both peers answer for the Job, and the owner is the one that has it.
	for _, from := range []string{"laptop", "mini"} {
		shown, err := n.fleet(from).Show(t.Context(), v.ID)
		if err != nil || shown.State == "moved" || shown.Gen != 2 {
			t.Fatalf("show from %s = %+v, %v", from, shown, err)
		}
	}
	list, err := n.fleet("laptop").List(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if rows := slices.DeleteFunc(slices.Clone(list.Jobs), func(r cli.JobView) bool { return r.ID != v.ID }); len(rows) != 1 || rows[0].Peer != "mini" {
		t.Fatalf("ps rows = %+v", rows)
	}
}

func (l *localJobs) store() *job.Store {
	p, _ := l.open()
	return p.store
}

func (l *localJobs) mirrorOf(t *testing.T) string {
	t.Helper()
	p, err := l.open()
	if err != nil {
		t.Fatal(err)
	}
	return p.mirrorDir("example.com/o/r")
}

func TestADestinationWithoutTheHarnessRefusesAndTheJobStaysStoppedAtTheSource(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	n.peers["mini"].have = false
	v := n.started(t, "laptop")

	_, err := n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"})
	requireCode(t, err, wire.CodeTransferAborted)
	if got := n.record(t, "laptop", v.ID); got.State != job.Stopped || got.Move != nil {
		t.Fatalf("source = %+v", got)
	}
	if _, getErr := n.peers["mini"].store().Get(v.ID); getErr == nil {
		t.Fatal("the destination has a Job it refused")
	}
	if n.launchesOn("mini") != 0 {
		t.Fatalf("an agent started at the destination: %v", n.launches())
	}
	n.peers["mini"].have = true
	mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, To: "mini"}) // and the same Job can go again
}

func TestALostAcknowledgementIsRepeatedWithRetryAndStartsOneAgent(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	n.cutNext("mini", wire.OpActivate, true)

	_, err := n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"})
	if err == nil || !strings.Contains(err.Error(), "--retry") {
		t.Fatalf("err = %v", err)
	}
	if got := n.record(t, "laptop", v.ID); got.State != job.Moving {
		t.Fatalf("source = %s", got.State)
	}
	if n.launchesOn("mini") != 1 {
		t.Fatalf("the destination did not start: %v", n.launches())
	}
	moved := mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, Retry: true})
	if moved.State != "moved" || n.launchesOn("mini") != 1 {
		t.Fatalf("after retry: %+v, launches %v", moved, n.launches())
	}
}

func TestAbortGivesTheMoveUpAndALateActivateStartsNothing(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	n.cutNext("mini", wire.OpActivate, false)

	if _, err := n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"}); err == nil {
		t.Fatal("a cut activate must leave the move unresolved")
	}
	transfer := n.record(t, "laptop", v.ID).Move.Transfer
	stopped := mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, Abort: true})
	if stopped.State != "stopped" {
		t.Fatalf("abort left %s", stopped.State)
	}
	_, err := n.peers["mini"].activate(t.Context(), server.TransferInput{Job: v.ID, Gen: 2, Transfer: transfer})
	requireCode(t, err, wire.CodeTransferAborted)
	if n.launchesOn("mini") != 0 {
		t.Fatalf("a late activate started an agent: %v", n.launches())
	}
	// A new move to the same peer works with a new transfer.
	mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, To: "mini"})
	if got := n.record(t, "mini", v.ID); got.Gen != 2 || n.launchesOn("mini") != 1 {
		t.Fatalf("destination = %+v launches %v", got, n.launches())
	}
}

func TestAbortAfterTheDestinationStartedLeavesTheJobMovedThere(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	n.cutNext("mini", wire.OpActivate, true)
	_, _ = n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"})

	_, err := n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, Abort: true})
	requireCode(t, err, domain.CodeInvalidTransition)
	if got := n.record(t, "laptop", v.ID); got.State != job.Moved || got.MovedTo == nil || got.MovedTo.Peer != "mini" {
		t.Fatalf("source = %+v", got)
	}
}

func TestAnAbortThatTheDestinationDoesNotConfirmChangesNothing(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	n.cutNext("mini", wire.OpPrepare, false)
	_, _ = n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"})
	n.cutNext("mini", wire.OpAbortTransfer, false)

	_, err := n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, Abort: true})
	if err == nil || !strings.Contains(err.Error(), "stays moving") {
		t.Fatalf("err = %v", err)
	}
	if got := n.record(t, "laptop", v.ID); got.State != job.Moving {
		t.Fatalf("source = %s", got.State)
	}
}

func TestTwoMovesAtOnceLeaveOneAgent(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini", "nas")
	v := n.started(t, "laptop")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, to := range []string{"mini", "nas"} {
		wg.Go(func() {
			_, errs[i] = n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: to})
		})
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("exactly one move wins: %v / %v", errs[0], errs[1])
	}
	if n.launchesOn("mini")+n.launchesOn("nas") != 1 {
		t.Fatalf("launches = %v", n.launches())
	}
}

func TestOldOrdersAfterAToBToAToCAreStale(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "a", "b", "c")
	v := n.started(t, "a")
	mustMove(t, n.fleet("a"), cli.MoveRequest{ID: v.ID, To: "b"})
	eventually(t, "b runs it", func() bool { return n.record(t, "b", v.ID).SessionID != "" })
	mustMove(t, n.fleet("b"), cli.MoveRequest{ID: v.ID, To: "a"})
	if got := n.record(t, "a", v.ID); got.Gen != 3 {
		t.Fatalf("a = %+v", got)
	}
	mustMove(t, n.fleet("a"), cli.MoveRequest{ID: v.ID, To: "c"})

	for _, order := range []func() error{
		func() error { _, err := n.peers["a"].stop(t.Context(), v.ID, 3); return err },
		func() error { _, err := n.peers["a"].resume(t.Context(), v.ID, 1, false); return err },
		func() error { _, err := n.peers["b"].send(t.Context(), v.ID, 2, "x"); return err },
		func() error {
			_, err := n.peers["b"].move(t.Context(), server.MoveInput{Job: v.ID, Gen: 2, To: "c"})
			return err
		},
	} {
		requireCode(t, order(), domain.CodeStaleLease)
	}
	// The one owner is found from anywhere, through the chain of `moved`.
	for _, from := range []string{"a", "b", "c"} {
		if shown, err := n.fleet(from).Show(t.Context(), v.ID); err != nil || shown.Peer != "c" || shown.Gen != 4 {
			t.Fatalf("show from %s = %+v, %v", from, shown, err)
		}
	}
	// A Job that is not at a peer anymore cannot be started there by a late prepare.
	if _, err := n.peers["b"].prepare(t.Context(), server.PrepareInput{Job: v.ID, Gen: 2, Transfer: job.NewTransferID(), Harness: "claude", Repo: was(n, "a", v.ID).Repo, Commit: strings.Repeat("a", 40)}, nil); err == nil {
		t.Fatal("a late prepare must fail")
	}
}

func was(n *moveNet, peer, id string) job.Job {
	p, _ := n.peers[peer].open()
	j, _ := p.store.Get(id)
	return j
}

func TestAMovementThatCrashesAnywhereIsFinishedByRetryWithOneAgent(t *testing.T) {
	sourcePoints := []string{pointReserved, pointPushed, pointPrepared, pointActivated}
	destinationPoints := []string{pointWorktree, pointStaged, pointPrepared2, pointImported, pointWritten, pointLaunched}
	for _, point := range append(slices.Clone(sourcePoints), destinationPoints...) {
		t.Run(point, func(t *testing.T) {
			n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
			v := n.started(t, "laptop")
			crasher := n.peers["mini"].faults
			if slices.Contains(sourcePoints, point) {
				crasher = n.peers["laptop"].faults
			}
			crasher.arm(point)

			if _, err := n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"}); err == nil {
				t.Fatal("the crash must be seen")
			}
			if got := n.record(t, "laptop", v.ID); got.State != job.Moving {
				t.Fatalf("source after the crash = %s", got.State)
			}
			moved := mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, Retry: true})
			if moved.State != "moved" {
				t.Fatalf("after retry: %+v", moved)
			}
			if got := n.record(t, "mini", v.ID); got.Gen != 2 || got.Launch == nil {
				t.Fatalf("destination = %+v", got)
			}
			if n.launchesOn("mini") != 1 {
				t.Fatalf("launches = %v", n.launches())
			}
		})
	}
}

func TestACrashAfterMovedIsWrittenLeavesTheJobMoved(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	n.peers["laptop"].faults.arm(pointMoved)
	if _, err := n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"}); err == nil {
		t.Fatal("the crash must be seen")
	}
	if got := n.record(t, "laptop", v.ID); got.State != job.Moved {
		t.Fatalf("source = %s", got.State)
	}
}

func TestADuplicateOrLateActivateAnswersTheJobAndStartsNothingMore(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	transfer := ""
	n.peers["laptop"].faults.arm(pointActivated)
	_, _ = n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"})
	transfer = n.record(t, "laptop", v.ID).Move.Transfer

	for range 2 {
		got, err := n.peers["mini"].activate(t.Context(), server.TransferInput{Job: v.ID, Gen: 2, Transfer: transfer})
		if err != nil || got.Gen != 2 || got.State == "stopped" {
			t.Fatalf("activate = %+v, %v", got, err)
		}
	}
	if n.launchesOn("mini") != 1 {
		t.Fatalf("launches = %v", n.launches())
	}
}

func TestAJobWhoseAgentSurvivesTheStopIsNotMoved(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	p, err := n.peers["laptop"].open()
	if err != nil {
		t.Fatal(err)
	}
	p.runner = runner.New(runner.Config{
		Store: p.store, Host: p.host, Tmux: p.tmux, Wrapper: n.peers["laptop"].over.wrapper, Agents: n.peers["laptop"].over.agents,
		Stopper: runner.Stopper{Grace: 20 * time.Millisecond, KillWait: 20 * time.Millisecond, Poll: 5 * time.Millisecond, Signal: func(int, syscall.Signal) error { return nil }},
	})

	if _, err := n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"}); err == nil {
		t.Fatal("a Job whose group survived must not move")
	}
	if got := n.record(t, "laptop", v.ID); got.State != job.Stopping || got.Move != nil {
		t.Fatalf("source = %+v", got)
	}
	if _, err := n.peers["mini"].store().Get(v.ID); err == nil || len(n.dialer.Dialed) != 0 {
		t.Fatalf("something was sent: %v", n.dialer.Dialed)
	}
}

func TestAHalfPreparedDestinationIsFinishedByAnotherPrepare(t *testing.T) {
	for _, point := range []string{pointWorktree, pointStaged} {
		n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
		v := n.started(t, "laptop")
		n.peers["mini"].faults.arm(point)
		_, _ = n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"})
		if _, err := n.peers["mini"].store().Get(v.ID); err == nil {
			t.Fatalf("%s: a Job exists before activate", point)
		}
		mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, Retry: true})
	}
}

func TestAForcedResumeOfAMovingJobRunsItAndTheOldTransferNeverStarts(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	n.cutNext("mini", wire.OpActivate, false)
	_, _ = n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "mini"})
	old := n.record(t, "laptop", v.ID).Move.Transfer

	if _, err := n.fleet("laptop").Resume(t.Context(), v.ID); err == nil {
		t.Fatal("a plain resume of a moving Job must be refused")
	}
	if _, err := n.fleet("laptop").ResumeForce(t.Context(), v.ID); err != nil {
		t.Fatal(err)
	}
	if got := n.record(t, "laptop", v.ID); got.Move != nil || got.State == job.Moving {
		t.Fatalf("source = %+v", got)
	}
	mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, To: "mini"})
	_, err := n.peers["mini"].activate(t.Context(), server.TransferInput{Job: v.ID, Gen: 2, Transfer: old})
	requireCode(t, err, wire.CodeTransferAborted)
	if n.launchesOn("mini") != 1 {
		t.Fatalf("mini launched %d times", n.launchesOn("mini"))
	}
}

func TestCleanRemovesTheWorkOfAMovedJobAndKeepsThePointer(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	if _, err := n.fleet("laptop").Clean(t.Context(), v.ID, false); err == nil {
		t.Fatal("a running Job cannot be cleaned")
	}
	mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, To: "mini"})
	wt := n.peers["laptop"].store().WorktreeDir(v.ID, 1)
	repo := n.peers["laptop"].mirrorOf(t)

	cleaned, err := n.fleet("laptop").Clean(t.Context(), v.ID, false)
	if err != nil || cleaned.State != "moved" || cleaned.MovedTo != "mini" {
		t.Fatalf("clean = %+v, %v", cleaned, err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("the worktree is still there: %v", err)
	}
	if out := runGit(t, ".", "for-each-ref", "refs/cflk/"+v.ID+"/"); out != "" {
		t.Fatalf("refs left in the user's repo: %s", out)
	}
	if _, err := os.Stat(repo); err == nil {
		if out := runGit(t, repo, "for-each-ref", "refs/cflk/"+v.ID+"/"); out != "" {
			t.Fatalf("refs left in the mirror: %s", out)
		}
	}
	if runGit(t, ".", "rev-parse", "--verify", "cflk/"+v.ID) == "" {
		t.Fatal("the user's branch is gone")
	}
	if _, err := n.fleet("laptop").Clean(t.Context(), v.ID, false); err != nil {
		t.Fatalf("clean twice: %v", err)
	}
	if _, err := n.fleet("mini").Clean(t.Context(), v.ID, false); err == nil {
		t.Fatal("the Job that runs cannot be cleaned")
	}
}

func TestCleanForgetsADoneJobWhole(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop")
	v := n.started(t, "laptop")
	p, _ := n.peers["laptop"].open()
	if err := p.runner.Stop(t.Context(), v.ID); err != nil {
		t.Fatal(err)
	}
	if err := p.store.WithLock(v.ID, func(j *job.Job) error { j.State = job.Done; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := n.fleet("laptop").Clean(t.Context(), v.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.store.Dir(v.ID)); !os.IsNotExist(err) {
		t.Fatalf("the Job directory is still there: %v", err)
	}
}

func TestAJobWithoutAnotherPeerOrInAnotherStateIsRefusedWithAReason(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.started(t, "laptop")
	_, err := n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "laptop"})
	requireCode(t, err, domain.CodeInvalidRequest)
	_, err = n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, To: "nowhere"})
	requireCode(t, err, domain.CodeInvalidRequest)
	_, err = n.fleet("laptop").Move(t.Context(), cli.MoveRequest{ID: v.ID, Retry: true})
	requireCode(t, err, domain.CodeInvalidTransition)
}

// shortDir is a temporary directory with a short path: Claude names the directory
// of a conversation after its cwd, and a long one is hashed.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
