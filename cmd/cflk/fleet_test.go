//go:build linux || darwin

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/remote"
	"github.com/Autarquico/codeflock/internal/peer/remote/remotetest"
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
)

// sshAttach is the test Dialer with the real ssh command for attach, so that
// what a window would run is the argv production builds.
type sshAttach struct{ *remotetest.Dialer }

func (sshAttach) AttachArgv(p discovery.Peer, id string, readOnly bool) ([]string, error) {
	return remote.SSH{ConnectTimeout: 3 * time.Second}.AttachArgv(p, id, readOnly)
}

type windowLog struct{ calls [][]string }

func (w *windowLog) Run(_ context.Context, name string, args ...string) error {
	w.calls = append(w.calls, append([]string{name}, args...))
	return nil
}

type testFleet struct {
	*fleet
	dialer  *remotetest.Dialer
	windows *windowLog
	peers   map[string]*localJobs
}

var neverEnds = fakeharness.Step{WaitFile: &fakeharness.WaitFile{Path: "/nonexistent/never", TimeoutMS: 120000}}

// newTestFleet is the peer "laptop" (this machine) and the peers named, each
// with a state directory of its own and reached through the test Dialer. A peer
// named "hung" accepts the connection and never answers; "off" is offline.
func newTestFleet(t *testing.T, steps []fakeharness.Step, names ...string) testFleet {
	t.Helper()
	stubHarnessesOnPath(t)
	repoWithOrigin(t)
	tf := testFleet{dialer: &remotetest.Dialer{Peers: map[string]remotetest.ServeFunc{}}, windows: &windowLog{}, peers: map[string]*localJobs{}}
	laptop := peerNamed(t, "laptop", fake, steps...)
	tf.peers["laptop"] = laptop
	var found []discovery.Peer
	for _, name := range names {
		p := discovery.Peer{ID: "n" + name, Name: name, Host: name + ".ts.net", Online: name != "off"}
		found = append(found, p)
		switch name {
		case "off":
		case "hung":
			tf.dialer.Peers[name] = func(ctx context.Context, _ io.Reader, _ io.Writer) error {
				<-ctx.Done()
				return ctx.Err()
			}
		default:
			l := peerNamed(t, name, fake, steps...)
			tf.peers[name] = l
			tf.dialer.Peers[name] = l.servePeer
		}
	}
	tf.fleet = &fleet{
		local: laptop, transport: sshAttach{tf.dialer}, windows: tf.windows, timeout: 400 * time.Millisecond,
		discover: func(context.Context) (discovery.Result, error) {
			return discovery.Result{Peers: found, Warnings: []string{"tailscale: not running"}}, nil
		},
	}
	return tf
}

// stubHarnessesOnPath puts a claude and a codex on the PATH, which is what a
// peer's hello reports as its harnesses.
func stubHarnessesOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"claude", "codex"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // an executable stub of a test
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPsShowsTheJobsOfThePeersThatAnswerAndMarksTheRestUnreachableInTime(t *testing.T) {
	tf := newTestFleet(t, []fakeharness.Step{neverEnds}, "mac-mini", "hung", "off")
	here, err := tf.Run(t.Context(), cli.RunRequest{Prompt: "here"})
	if err != nil {
		t.Fatal(err)
	}
	there, err := tf.Run(t.Context(), cli.RunRequest{On: "mac-mini", Prompt: "there"})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	views, err := tf.List(t.Context(), "")
	if err != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("ps took %v: %v", time.Since(start), err)
	}
	got := map[string]cli.JobView{}
	for _, v := range views {
		got[v.Peer+"/"+v.State] = v
	}
	if len(views) != 4 || got["laptop/running"].ID != here.ID || got["mac-mini/running"].ID != there.ID {
		t.Fatalf("views = %+v", views)
	}
	for _, name := range []string{"hung", "off"} {
		if v := got[name+"/unreachable"]; v.ID != "" || v.Problem == "" {
			t.Errorf("%s: %+v", name, v)
		}
	}
	if v := got["mac-mini/running"]; v.Repo != "example.com/o/r" || v.Branch != "cflk/"+v.ID || v.Harness == "" {
		t.Errorf("a remote row lacks its fields: %+v", v)
	}

	only, err := tf.List(t.Context(), "mac-mini")
	if err != nil || len(only) != 1 || only[0].ID != there.ID {
		t.Errorf("--peer mac-mini = %+v, %v", only, err)
	}
	if only, err := tf.List(t.Context(), "laptop"); err != nil || len(only) != 1 || only[0].ID != here.ID {
		t.Errorf("--peer laptop = %+v, %v", only, err)
	}
	if _, err := tf.List(t.Context(), "nope"); codeOf(err) != domain.CodeInvalidRequest {
		t.Errorf("--peer nope: %v", err)
	}
}

func TestPeersSaysWhoAnswersWithHarnessesAndJobs(t *testing.T) {
	tf := newTestFleet(t, []fakeharness.Step{neverEnds}, "mac-mini", "hung", "off")
	if _, err := tf.Run(t.Context(), cli.RunRequest{On: "mac-mini", Prompt: "there"}); err != nil {
		t.Fatal(err)
	}
	peers, err := tf.Peers(t.Context())
	if err != nil || len(peers) != 4 {
		t.Fatalf("peers = %+v, %v", peers, err)
	}
	if p := peers[0]; p.Name != "laptop" || !p.This || !p.Reachable || !reflect.DeepEqual(p.Harnesses, []string{"claude", "codex"}) {
		t.Errorf("this: %+v", p)
	}
	if p := peers[1]; p.Name != "mac-mini" || !p.Reachable || p.Jobs != 1 {
		t.Errorf("mac-mini: %+v", p)
	}
	for _, p := range peers[2:] {
		if p.Reachable || p.Problem == "" {
			t.Errorf("%s should be unreachable: %+v", p.Name, p)
		}
	}
}

func TestRunOnAPeerDelegatesTheUnpushedCommitAndEveryCommandFollowsTheJob(t *testing.T) {
	received := filepath.Join(t.TempDir(), "received")
	tf := newTestFleet(t, askAndKeepWaiting(received), "mac-mini")
	repo, _ := os.Getwd()
	// A branch whose name looks like shell, with a commit origin does not have.
	pwned := filepath.Join(t.TempDir(), "PWNED")
	branch := "fix;$(touch${IFS}" + pwned + ")"
	runGit(t, repo, "switch", "-q", "-c", branch)
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "unpushed")
	head := runGit(t, repo, "rev-parse", "HEAD")

	v, err := tf.Run(t.Context(), cli.RunRequest{On: "mac-mini", Branch: branch, Prompt: "fix \"the\" tests; $(id)\n`x`"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Peer != "mac-mini" || v.State != "running" || v.Branch != "cflk/"+v.ID {
		t.Fatalf("run = %+v", v)
	}
	p, _ := tf.peers["mac-mini"].open()
	if got := runGit(t, p.store.WorktreeDir(v.ID, 1), "rev-parse", "HEAD"); got != head {
		t.Errorf("the peer's worktree is at %s, want %s", got, head)
	}
	if mustGet(t, p, v.ID).CreatedBy != "laptop" {
		t.Error("created_by is not this peer")
	}
	if local, _ := tf.peers["laptop"].List(t.Context()); len(local) != 0 {
		t.Errorf("the Job also exists here: %+v", local)
	}
	for _, d := range tf.dialer.Dialed {
		if len(d.Args) != 0 {
			t.Errorf("a dial carried arguments: %+v", d)
		}
	}

	if shown, err := tf.Show(t.Context(), v.ID); err != nil || shown.Peer != "mac-mini" {
		t.Errorf("show = %+v, %v", shown, err)
	}
	eventually(t, "the screen of the remote Job", func() bool {
		s, err := tf.Screen(t.Context(), v.ID)
		return err == nil && strings.Contains(s, "Which branch?")
	})
	if _, err := tf.Send(t.Context(), v.ID, "use main; $(id)"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the answer to arrive verbatim", func() bool {
		b, _ := os.ReadFile(received) //nolint:gosec // a file of the test
		return strings.TrimSpace(string(b)) == "use main; $(id)"
	})
	if s, err := tf.Stop(t.Context(), v.ID); err != nil || s.State != "stopped" {
		t.Errorf("stop = %+v, %v", s, err)
	}
	if s, err := tf.Resume(t.Context(), v.ID); err != nil || s.State != "running" {
		t.Errorf("resume = %+v, %v", s, err)
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Error("the branch name was interpreted")
	}
}

func TestRunOnAnyTakesTheReachablePeerWithFewestJobs(t *testing.T) {
	tf := newTestFleet(t, []fakeharness.Step{neverEnds}, "mac-mini", "studio", "hung", "off")
	// laptop: 1 job, mac-mini: 1 job, studio: none.
	if _, err := tf.Run(t.Context(), cli.RunRequest{Prompt: "here"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tf.Run(t.Context(), cli.RunRequest{On: "mac-mini", Prompt: "there"}); err != nil {
		t.Fatal(err)
	}
	v, err := tf.Run(t.Context(), cli.RunRequest{On: "any", Prompt: "anywhere"})
	if err != nil || v.Peer != "studio" {
		t.Fatalf("any = %+v, %v", v, err)
	}
	// Now every reachable peer has one: the tie goes to this machine.
	if v, err = tf.Run(t.Context(), cli.RunRequest{On: "any", Prompt: "again"}); err != nil || v.Peer != "laptop" {
		t.Fatalf("any on a tie = %+v, %v", v, err)
	}
	if _, err := tf.Run(t.Context(), cli.RunRequest{On: "off", Prompt: "x"}); codeOf(err) != domain.CodeWorkerOffline {
		t.Errorf("an offline peer: %v", err)
	}
	if _, err := tf.Run(t.Context(), cli.RunRequest{On: "nobody", Prompt: "x"}); codeOf(err) != domain.CodeInvalidRequest {
		t.Errorf("an unknown peer: %v", err)
	}
	if _, err := tf.Run(t.Context(), cli.RunRequest{On: "-oProxyCommand=x", Prompt: "x"}); codeOf(err) != domain.CodeInvalidRequest {
		t.Errorf("a host typed as an option: %v", err)
	}
}

func TestAttachToARemoteJobOpensAWindowWithTheSSHCommand(t *testing.T) {
	tf := newTestFleet(t, []fakeharness.Step{neverEnds}, "mac-mini")
	v, err := tf.Run(t.Context(), cli.RunRequest{On: "mac-mini", Prompt: "there"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", "/tmp/tmux-1/default,1,0")

	for _, readOnly := range []bool{false, true} {
		tf.windows.calls = nil
		if err := tf.Attach(t.Context(), v.ID, readOnly); err != nil {
			t.Fatal(err)
		}
		if len(tf.windows.calls) != 1 || tf.windows.calls[0][0] != "tmux" {
			t.Fatalf("window calls = %q", tf.windows.calls)
		}
		call := tf.windows.calls[0]
		script, err := os.ReadFile(call[len(call)-1]) //nolint:gosec // the script the launcher was asked to run
		if err != nil {
			t.Fatal(err)
		}
		want := "'ssh' '-t' '-o' 'BatchMode=yes' '-o' 'ConnectTimeout=3' '-o' 'ServerAliveInterval=15' '-o' 'ServerAliveCountMax=3' '--' 'mac-mini.ts.net' 'cflk' 'peer' 'attach' "
		if readOnly {
			want += "'--read-only' "
		}
		want += "'" + v.ID + "'"
		if !strings.Contains(string(script), want) {
			t.Errorf("read-only=%v: the window runs\n%s\nwant it to run %s", readOnly, script, want)
		}
	}

	if _, err := tf.Stop(t.Context(), v.ID); err != nil {
		t.Fatal(err)
	}
	if err := tf.Attach(t.Context(), v.ID, false); codeOf(err) != domain.CodeRunNotAttachable {
		t.Errorf("attach to a stopped Job: %v", err)
	}
}

func TestAJobIsFoundOnThePeerThatHasItAndAMissingOneSaysWhoDidNotAnswer(t *testing.T) {
	tf := newTestFleet(t, []fakeharness.Step{neverEnds}, "mac-mini", "hung")
	if _, err := tf.Show(t.Context(), "job-$(id)"); codeOf(err) != domain.CodeInvalidRequest {
		t.Errorf("a bad id: %v", err)
	}
	_, err := tf.Show(t.Context(), "job-aaaaaaaaaa")
	if codeOf(err) != domain.CodeNotFound || !strings.Contains(err.Error(), "hung") || strings.Contains(err.Error(), "mac-mini") {
		t.Errorf("a missing Job: %v", err)
	}
}
