//go:build realcli && (linux || darwin)

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/remote/remotetest"
)

// These tests move a Job of the real CLI between two state directories of this
// machine and ask it, on the other side, for what only the conversation knows. They
// call a model (a few tokens), with the login of the machine.
//
//	go test -tags realcli -run Real ./cmd/cflk
func realPeers(t *testing.T, binary string) *fleet {
	t.Helper()
	if _, err := exec.LookPath(binary); err != nil {
		t.Skipf("%s is not installed", binary)
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	repoWithOrigin(t)
	// Codex runs this as its notify program: marker, session file and payload follow.
	notify := filepath.Join(shortDir(t), "cflk")
	script := fmt.Sprintf("#!/bin/sh\nshift 2\nexec env %s=1 %q \"$@\"\n", asNotify, os.Args[0])
	if err := os.WriteFile(notify, []byte(script), 0o700); err != nil { //nolint:gosec // a test's stand-in executable
		t.Fatal(err)
	}
	dialer := &remotetest.Dialer{Peers: map[string]remotetest.ServeFunc{}}
	var found []discovery.Peer
	peers := map[string]*localJobs{}
	for _, name := range []string{"laptop", "mini"} {
		l := newLocalJobs(notify, overrides{
			name: name, root: shortDir(t), share: t.TempDir(),
			wrapper: func(dir string) []string { return []string{"/usr/bin/env", asWrapper + "=" + dir, os.Args[0]} },
		})
		p, err := l.open()
		if err != nil {
			t.Fatal(err)
		}
		if err := p.tmux.Check(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx := context.WithoutCancel(t.Context())
			jobs, _ := p.store.List()
			for _, j := range jobs {
				_ = p.runner.Stop(ctx, j.ID)
				for _, wt := range mustGlob(p.store.Dir(j.ID)) {
					_ = claude.Adapter{}.UntrustFolder(p.host, wt)
				}
			}
			_ = p.tmux.KillServer(ctx)
		})
		peers[name] = l
		dialer.Peers[name] = l.servePeer
		found = append(found, discovery.Peer{ID: "n" + name, Name: name, Host: name + ".ts.net", Online: true})
	}
	discover := func(context.Context) (discovery.Result, error) { return discovery.Result{Peers: found}, nil }
	for _, l := range peers {
		l.net = &network{transport: dialer, discover: discover}
	}
	return &fleet{local: peers["laptop"], transport: dialer, discover: discover, timeout: 5 * time.Second, orderTimeout: 5 * time.Minute}
}

func mustGlob(dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, "g*"))
	return m
}

func realMoveKeepsTheConversation(t *testing.T, binary, harness string) {
	f := realPeers(t, binary)
	ctx := t.Context()
	v, err := f.Run(ctx, cli.RunRequest{Harness: harness, Prompt: "The secret word is KUMQUAT. Remember it. Reply with the single word READY and nothing else."})
	if err != nil && harness == "codex" && strings.Contains(err.Error(), "finding the session of the launch") {
		// Codex asks to trust the git root of a worktree it was not told to trust, and waits.
		t.Skipf("codex does not start without a trust decision for the repository: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	waitWaiting := func(id string, within time.Duration) cli.JobView {
		t.Helper()
		var last cli.JobView
		for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(time.Second) {
			if last, err = f.Show(ctx, id); err != nil {
				t.Fatal(err)
			}
			if last.State == string(job.Waiting) {
				return last
			}
		}
		screen, _ := f.Screen(ctx, id)
		t.Fatalf("job %s is %s, not waiting, after %s; the screen:\n%s", id, last.State, within, screen)
		return last
	}
	was := waitWaiting(v.ID, 90*time.Second)

	moved, err := f.Move(ctx, cli.MoveRequest{ID: v.ID, To: "mini"})
	if err != nil || moved.State != "moved" {
		t.Fatalf("move = %+v, %v", moved, err)
	}
	// A resumed conversation has not ended a turn, so it need not be waiting: it is
	// there when its agent runs and its screen is up.
	var there cli.JobView
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		there, err = f.Show(ctx, v.ID)
		if screen, _ := f.Screen(ctx, v.ID); err == nil && there.Peer == "mini" && (there.State == string(job.Running) || there.State == string(job.Waiting)) && strings.TrimSpace(screen) != "" {
			break
		}
	}
	if there.Peer != "mini" || there.Gen != 2 || there.SessionID == was.SessionID {
		t.Fatalf("destination = %+v (was %+v)", there, was)
	}
	if _, err := f.Send(ctx, v.ID, "Reply with the secret word spelled backwards, in capitals, and nothing else."); err != nil {
		t.Fatal(err)
	}
	eventually := func() bool {
		screen, err := f.Screen(ctx, v.ID)
		return err == nil && strings.Contains(screen, "TAUQMUK")
	}
	remembered := false
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline) && !remembered; time.Sleep(2 * time.Second) {
		remembered = eventually()
	}
	if !remembered {
		screen, _ := f.Screen(ctx, v.ID)
		t.Fatalf("the agent at the destination does not remember the word:\n%s", screen)
	}
}

func TestRealClaudeJobMovedBetweenTwoStateDirsRemembersItsConversation(t *testing.T) {
	realMoveKeepsTheConversation(t, "claude", "claude")
}

func TestRealCodexJobMovedBetweenTwoStateDirsRemembersItsConversation(t *testing.T) {
	realMoveKeepsTheConversation(t, "codex", "codex")
}
