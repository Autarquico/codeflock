//go:build linux || darwin

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/remote"
	"github.com/Autarquico/codeflock/internal/peer/remote/remotetest"
	"github.com/Autarquico/codeflock/internal/peer/server"
	"github.com/Autarquico/codeflock/internal/peer/wire"
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
)

// twoPeers is this machine's repo and a peer named mac-mini, reached through a
// Dialer that answers in this process with a state directory of its own.
type twoPeers struct {
	remote *localJobs
	dialer *remotetest.Dialer
	peer   discovery.Peer
}

func newTwoPeers(t *testing.T, steps ...fakeharness.Step) twoPeers {
	t.Helper()
	l := peerNamed(t, "mac-mini", fake, steps...)
	d := &remotetest.Dialer{Peers: map[string]remotetest.ServeFunc{"mac-mini": l.servePeer}}
	return twoPeers{remote: l, dialer: d, peer: discovery.Peer{ID: "nMAC", Name: "mac-mini", Host: "mac-mini.ts.net", Online: true}}
}

func (tp twoPeers) call(t *testing.T, req wire.Request, body string) (wire.Response, string, error) {
	t.Helper()
	resp, out, err := remote.Call(t.Context(), tp.dialer, tp.peer, req, []byte(body))
	return resp, string(out), err
}

// delegate does what the client of `cflk run --on` does: ask for the mirror, push
// HEAD under refs/cflk/<id>/g1 and ask for the run.
func (tp twoPeers) delegate(t *testing.T, repo, prompt string) (wire.JobView, error) {
	t.Helper()
	repoID, err := job.NormalizeRepo(runGit(t, repo, "remote", "get-url", "origin"))
	if err != nil {
		t.Fatal(err)
	}
	mirror, _, err := tp.call(t, wire.Request{Op: wire.OpMirror, Repo: repoID}, "")
	if err != nil {
		t.Fatal(err)
	}
	id := job.NewID()
	runGit(t, repo, "push", "-q", mirror.Mirror, "HEAD:refs/cflk/"+id+"/g1")
	resp, _, err := tp.call(t, wire.Request{
		Op: wire.OpRun, Job: id, Harness: "claude", Repo: repoID, Commit: runGit(t, repo, "rev-parse", "HEAD"), CreatedBy: "laptop",
	}, prompt)
	if err != nil {
		return wire.JobView{}, err
	}
	return *resp.Job, nil
}

func TestADelegatedJobRunsOnTheOtherPeerFromTheCommitThatWasPushed(t *testing.T) {
	repo := repoWithOrigin(t)
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "unpushed to origin")
	head := runGit(t, repo, "rev-parse", "HEAD")
	received := filepath.Join(t.TempDir(), "received")
	tp := newTwoPeers(t, askAndKeepWaiting(received)...)

	v, err := tp.delegate(t, repo, "fix \"the\" tests; $(touch /tmp/pwned)\nand `more`")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "running" || v.Peer != "mac-mini" || v.Repo != "example.com/o/r" || v.Branch != "cflk/"+v.ID || v.Gen != 1 {
		t.Fatalf("run = %+v", v)
	}
	p, _ := tp.remote.open()
	if got := runGit(t, p.store.WorktreeDir(v.ID, 1), "rev-parse", "HEAD"); got != head {
		t.Errorf("the worktree is at %s, want the pushed commit %s", got, head)
	}
	if j := mustGet(t, p, v.ID); j.CreatedBy != "laptop" {
		t.Errorf("created_by = %q", j.CreatedBy)
	}

	ps, _, err := tp.call(t, wire.Request{Op: wire.OpPS}, "")
	if err != nil || len(ps.Jobs) != 1 || ps.Jobs[0].ID != v.ID || ps.Jobs[0].State != "running" {
		t.Fatalf("ps = %+v, %v", ps, err)
	}

	eventually(t, "the screen to show the question", func() bool {
		_, screen, err := tp.call(t, wire.Request{Op: wire.OpScreen, Job: v.ID}, "")
		return err == nil && strings.Contains(screen, "Which branch?")
	})
	text := "use main; $(touch /tmp/pwned2) \"quoted\""
	if _, _, err := tp.call(t, wire.Request{Op: wire.OpSend, Job: v.ID, Gen: 1}, text); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the agent to receive the answer verbatim", func() bool {
		b, _ := os.ReadFile(received) //nolint:gosec // a file of the test
		return strings.TrimSpace(string(b)) == text
	})
	for _, path := range []string{"/tmp/pwned", "/tmp/pwned2"} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s exists: something was interpreted", path)
		}
	}

	if stopped, _, err := tp.call(t, wire.Request{Op: wire.OpStop, Job: v.ID, Gen: 1}, ""); err != nil || stopped.Job.State != "stopped" {
		t.Fatalf("stop = %+v, %v", stopped, err)
	}
	if resumed, _, err := tp.call(t, wire.Request{Op: wire.OpResume, Job: v.ID}, ""); err != nil || resumed.Job.State != "running" {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}
	if shown, _, err := tp.call(t, wire.Request{Op: wire.OpShow, Job: v.ID}, ""); err != nil || shown.Job.State != "running" {
		t.Fatalf("show = %+v, %v", shown, err)
	}
	if _, _, err := tp.call(t, wire.Request{Op: wire.OpStop, Job: v.ID, Gen: 7}, ""); codeOf(err) != domain.CodeStaleLease {
		t.Errorf("an order for another generation: %v", err)
	}
}

func TestARunNeedsTheCodeToBeInTheMirrorAtTheDeclaredCommit(t *testing.T) {
	repo := repoWithOrigin(t)
	tp := newTwoPeers(t, fakeharness.Step{WaitFile: &fakeharness.WaitFile{Path: filepath.Join(t.TempDir(), "never"), TimeoutMS: 120000}})
	head := runGit(t, repo, "rev-parse", "HEAD")
	run := func(id, commit string) error {
		_, _, err := tp.call(t, wire.Request{Op: wire.OpRun, Job: id, Harness: "claude", Repo: "example.com/o/r", Commit: commit, CreatedBy: "laptop"}, "work")
		return err
	}

	if err := run(job.NewID(), head); codeOf(err) != domain.CodeInvalidRequest {
		t.Errorf("a run with nothing pushed: %v", err)
	}
	mirror, _, err := tp.call(t, wire.Request{Op: wire.OpMirror, Repo: "example.com/o/r"}, "")
	if err != nil {
		t.Fatal(err)
	}
	id := job.NewID()
	runGit(t, repo, "push", "-q", mirror.Mirror, "HEAD:refs/cflk/"+id+"/g1")
	if err := run(id, strings.Repeat("a", 40)); codeOf(err) != domain.CodeInvalidRequest {
		t.Errorf("a run that declares another commit: %v", err)
	}
	if jobs, _ := tp.remote.List(t.Context()); len(jobs) != 0 {
		t.Errorf("refused runs left Jobs: %v", jobs)
	}
	if err := run(id, head); err != nil {
		t.Fatalf("the run that matches: %v", err)
	}
	if err := run(id, head); codeOf(err) != domain.CodeInvalidRequest {
		t.Errorf("the same id twice: %v", err)
	}
	if jobs, _ := tp.remote.List(t.Context()); len(jobs) != 1 {
		t.Errorf("jobs = %v", jobs)
	}
}

func TestMirrorIsTheSamePlaceEveryTimeAndKeyedByTheRepo(t *testing.T) {
	tp := newTwoPeers(t)
	a, _, err := tp.call(t, wire.Request{Op: wire.OpMirror, Repo: "example.com/o/r"}, "")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := tp.call(t, wire.Request{Op: wire.OpMirror, Repo: "example.com/o/r"}, "")
	other, _, err2 := tp.call(t, wire.Request{Op: wire.OpMirror, Repo: "example.com/o/other"}, "")
	if err != nil || err2 != nil || a.Mirror != b.Mirror || a.Mirror == other.Mirror {
		t.Fatalf("mirrors %q %q %q (%v, %v)", a.Mirror, b.Mirror, other.Mirror, err, err2)
	}
	p, _ := tp.remote.open()
	if want := filepath.Join(p.mirrors, job.RepoHash("example.com/o/r")+".git"); a.Mirror != want {
		t.Errorf("mirror = %s, want %s", a.Mirror, want)
	}
}

func TestHelloSaysWhoAnswers(t *testing.T) {
	tp := newTwoPeers(t)
	resp, _, err := tp.call(t, wire.Request{Op: wire.OpHello}, "")
	if err != nil || resp.Hello == nil || resp.Hello.Name != "mac-mini" || resp.Hello.Jobs != 0 {
		t.Fatalf("hello = %+v, %v", resp.Hello, err)
	}
}

func TestAnErrorOfThePeerArrivesWithItsCode(t *testing.T) {
	tp := newTwoPeers(t)
	_, _, err := tp.call(t, wire.Request{Op: wire.OpShow, Job: "job-aaaaaaaaaa"}, "")
	if codeOf(err) != domain.CodeNotFound {
		t.Errorf("show of an unknown Job: %v", err)
	}
}

// serveCommand runs `cflk peer` as the binary does: the arguments are the argv,
// the stream is stdin and stdout.
func serveCommand(t *testing.T, l *localJobs, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cli.RunContextWith(t.Context(), cli.Options{PeerServe: l.servePeer, PeerAttach: l.attachPeer}, args, strings.NewReader(stdin), &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestCflkPeerAnswersOnStdoutAndRefusesWhatIsNotTheProtocol(t *testing.T) {
	l := isolated(t)
	out, _, code := serveCommand(t, l, `{"op":"ps"}`+"\n", "peer")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if resp, _, err := wire.ReadResponse(strings.NewReader(out)); err != nil || resp.Error != nil {
		t.Errorf("ps over stdin: %q, %v", out, err)
	}

	for name, stdin := range map[string]string{
		"unknown op":      `{"op":"exec"}` + "\n",
		"unknown field":   `{"op":"ps","argv":["sh"]}` + "\n",
		"a shell in a id": `{"op":"show","job":"job-$(id)"}` + "\n",
		"truncated body":  `{"op":"send","job":"job-abcdefghij","size":9}` + "\nabc",
	} {
		out, _, code := serveCommand(t, l, stdin, "peer")
		resp, _, err := wire.ReadResponse(strings.NewReader(out))
		if code == 0 || err != nil || resp.Error == nil || resp.Error.Code != string(domain.CodeInvalidRequest) {
			t.Errorf("%s: exit %d, %q, %v", name, code, out, err)
		}
	}
}

func TestCflkPeerAttachValidatesTheJobAndNeedsATerminal(t *testing.T) {
	l := isolated(t)
	if _, _, code := serveCommand(t, l, "", "peer", "attach", "job-$(id)"); code != domain.ExitCode(domain.CodeInvalidRequest) {
		t.Errorf("a bad id: exit %d", code)
	}
	if _, _, code := serveCommand(t, l, "", "peer", "attach", "--read-only", "job-aaaaaaaaaa"); code != domain.ExitCode(domain.CodeNotFound) {
		t.Errorf("an unknown Job: exit %d", code)
	}
}

func TestAttachHereWithoutATerminalDoesNotOpenAWindow(t *testing.T) {
	repoWithOrigin(t)
	l := peerWith(t, fake, fakeharness.Step{WaitFile: &fakeharness.WaitFile{Path: filepath.Join(t.TempDir(), "never"), TimeoutMS: 120000}})
	v, err := l.Run(t.Context(), cli.RunRequest{Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	t.Setenv("TMUX", "/tmp/tmux-1/default,1,0")
	err = l.AttachHere(t.Context(), v.ID, false)
	if codeOf(err) != domain.CodeRunNotAttachable {
		t.Errorf("attach here without a terminal: %v", err)
	}
}

var _ server.Local = peerService{}
