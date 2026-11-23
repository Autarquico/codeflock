//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/runner"
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
)

// asWrapper makes the test binary the wrapper of a launch, as `cflk` is in production.
const asWrapper = "CF_TEST_AS_WRAPPER"

func TestMain(m *testing.M) {
	fakeharness.RunIfRequested()
	if dir := os.Getenv(asWrapper); dir != "" {
		_ = os.Unsetenv(asWrapper)
		if err := runner.RunWrapper(dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeAgent stands for Claude: the fake harness with a script, or a program that does not exist.
type fakeAgent struct {
	script  string
	missing bool
}

func (a fakeAgent) Prepare(context.Context, runner.Input) (runner.Plan, error) {
	if a.missing {
		return runner.Plan{Argv: []string{"/nonexistent/agent"}, SessionID: "fake-session"}, nil
	}
	return runner.Plan{
		Argv:      []string{os.Args[0], "--script", a.script},
		Env:       []string{fakeharness.AsFakeharnessEnv + "=1"},
		SessionID: "fake-session",
	}, nil
}

func (fakeAgent) Discover(context.Context, runner.Input, time.Time) ([]string, error) {
	return nil, nil
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // a test's fixed git argv
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repoWithOrigin is the working directory of the test: a repository with an origin and one commit.
func repoWithOrigin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "remote", "add", "origin", "https://example.com/o/r.git")
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	t.Chdir(dir)
	return dir
}

// peerWith is a peer on a state root and a tmux socket of its own, whose Claude
// agent is the fake harness running steps.
func peerWith(t *testing.T, agent func(script string) fakeAgent, steps ...fakeharness.Step) *localJobs {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv(stateDirVar, t.TempDir())
	script := filepath.Join(t.TempDir(), "script.json")
	raw, err := json.Marshal(fakeharness.Script{Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	l := newLocalJobs("/cflk", overrides{
		agents:  map[string]runner.Agent{claude.ID: agent(script)},
		wrapper: func(dir string) []string { return []string{"/usr/bin/env", asWrapper + "=" + dir, os.Args[0]} },
	})
	p, err := l.open()
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
	return l
}

func askAndKeepWaiting(received string) []fakeharness.Step {
	return []fakeharness.Step{
		{Emit: &fakeharness.Line{Type: harness.EventInputRequired, Message: "Which branch?"}},
		{ReadLine: &fakeharness.ReadLine{TimeoutMS: 20000, To: received}},
		{WaitFile: &fakeharness.WaitFile{Path: filepath.Join(received + ".never"), TimeoutMS: 120000}},
	}
}

func fake(script string) fakeAgent { return fakeAgent{script: script} }

func stateOf(t *testing.T, l *localJobs, id string) string {
	t.Helper()
	v, err := l.Show(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v.State
}

func TestRunPsScreenSendStopAndResumeOnAFakeHarness(t *testing.T) {
	repo := repoWithOrigin(t)
	received := filepath.Join(t.TempDir(), "received")
	l := peerWith(t, fake, askAndKeepWaiting(received)...)
	ctx := t.Context()

	v, err := l.Run(ctx, cli.RunRequest{Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if v.State != "running" || v.Repo != "example.com/o/r" || v.Terminal == "" || v.SessionID != "fake-session" {
		t.Fatalf("run = %+v", v)
	}
	if !strings.Contains(runGit(t, repo, "worktree", "list"), v.ID) || runGit(t, repo, "branch", "--list", v.Branch) == "" {
		t.Errorf("run left no worktree or branch for %s", v.ID)
	}

	list, err := l.List(ctx)
	if err != nil || len(list) != 1 || list[0].ID != v.ID || list[0].State != "running" {
		t.Fatalf("ps = %+v, %v", list, err)
	}

	eventually(t, "the screen to show the question", func() bool {
		s, err := l.Screen(ctx, v.ID)
		return err == nil && strings.Contains(s, "Which branch?")
	})

	// The agent asks: the hooks of its harness leave the marker.
	p, _ := l.open()
	if err := os.WriteFile(p.store.WaitingPath(v.ID, mustGet(t, p, v.ID).Launch.ID), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, l, v.ID); got != "waiting" {
		t.Fatalf("state after the marker = %s, want waiting", got)
	}
	if after, err := l.Send(ctx, v.ID, "use main"); err != nil || after.State != "running" {
		t.Fatalf("send = %+v, %v", after, err)
	}
	eventually(t, "the agent to receive the answer", func() bool {
		b, _ := os.ReadFile(received) //nolint:gosec // a file of the test
		return strings.TrimSpace(string(b)) == "use main"
	})

	if stopped, err := l.Stop(ctx, v.ID); err != nil || stopped.State != "stopped" {
		t.Fatalf("stop = %+v, %v", stopped, err)
	}
	if _, err := l.Screen(ctx, v.ID); codeOf(err) != domain.CodeRunNotAttachable {
		t.Errorf("screen of a stopped Job: %v", err)
	}

	resumed, err := l.Resume(ctx, v.ID)
	if err != nil || resumed.State != "running" || resumed.SessionID != "fake-session" {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}
}

func mustGet(t *testing.T, p *peer, id string) job.Job {
	t.Helper()
	j, err := p.store.Get(id)
	if err != nil || j.Launch == nil {
		t.Fatalf("job %s: %+v, %v", id, j, err)
	}
	return j
}

func TestResumeRefusesAJobThatIsNotStoppedLostOrFailed(t *testing.T) {
	repoWithOrigin(t)
	received := filepath.Join(t.TempDir(), "received")
	l := peerWith(t, fake, askAndKeepWaiting(received)...)
	ctx := t.Context()
	p, _ := l.open()

	v, err := l.Run(ctx, cli.RunRequest{Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Resume(ctx, v.ID); codeOf(err) != domain.CodeInvalidTransition {
		t.Errorf("resume of a running Job: %v", err)
	}
	if err := os.WriteFile(p.store.WaitingPath(v.ID, mustGet(t, p, v.ID).Launch.ID), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, l, v.ID); got != "waiting" {
		t.Fatalf("state = %s, want waiting", got)
	}
	before := mustGet(t, p, v.ID).Launch.ID
	if _, err := l.Resume(ctx, v.ID); codeOf(err) != domain.CodeInvalidTransition {
		t.Errorf("resume of a waiting Job: %v", err)
	}
	if after := mustGet(t, p, v.ID).Launch.ID; after != before {
		t.Errorf("a refused resume replaced the launch: %s -> %s", before, after)
	}

	err = p.store.WithLock(v.ID, func(j *job.Job) error { j.State = job.Stopping; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Resume(ctx, v.ID); codeOf(err) != domain.CodeInvalidTransition {
		t.Errorf("resume of a stopping Job: %v", err)
	}
}

func TestStopAndResumeOfADoneJobAreRefusedWithTheSameCode(t *testing.T) {
	repoWithOrigin(t)
	exit0 := 0
	l := peerWith(t, fake, fakeharness.Step{Exit: &exit0})
	v, err := l.Run(t.Context(), cli.RunRequest{Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the agent to end", func() bool { return stateOf(t, l, v.ID) == "done" })
	if _, err := l.Resume(t.Context(), v.ID); codeOf(err) != domain.CodeInvalidTransition {
		t.Errorf("resume of a done Job: %v", err)
	}
	if _, err := l.Stop(t.Context(), v.ID); codeOf(err) != domain.CodeInvalidTransition {
		t.Errorf("stop of a done Job: %v", err)
	}
}

func TestRunFromABranchStartsTheWorktreeThere(t *testing.T) {
	repo := repoWithOrigin(t)
	runGit(t, repo, "switch", "-q", "-c", "feature")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "on feature")
	feature := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "switch", "-q", "main")
	l := peerWith(t, fake, fakeharness.Step{WaitFile: &fakeharness.WaitFile{Path: filepath.Join(t.TempDir(), "never"), TimeoutMS: 120000}})

	v, err := l.Run(t.Context(), cli.RunRequest{Branch: "feature", Prompt: "work"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := l.open()
	if got := runGit(t, p.store.WorktreeDir(v.ID, 1), "rev-parse", "HEAD"); got != feature {
		t.Errorf("the worktree starts at %s, want %s", got, feature)
	}
	if _, err := l.Run(t.Context(), cli.RunRequest{Branch: "no-such-branch", Prompt: "work"}); codeOf(err) != domain.CodeInvalidRequest {
		t.Errorf("an unknown branch: %v", err)
	}
}

func TestARunWhoseLaunchFailsLeavesNoJobWorktreeOrBranch(t *testing.T) {
	repo := repoWithOrigin(t)
	l := peerWith(t, func(string) fakeAgent { return fakeAgent{missing: true} })

	if _, err := l.Run(t.Context(), cli.RunRequest{Prompt: "work"}); err == nil {
		t.Fatal("a launch whose agent does not start succeeded")
	}

	if got := runGit(t, repo, "worktree", "list"); strings.Count(got, "\n") != 0 {
		t.Errorf("worktrees left behind:\n%s", got)
	}
	if got := runGit(t, repo, "branch", "--list", "cflk/*"); got != "" {
		t.Errorf("branches left behind: %s", got)
	}
	if jobs, err := l.List(t.Context()); err != nil || len(jobs) != 0 {
		t.Errorf("jobs left behind: %v, %v", jobs, err)
	}
	p, _ := l.open()
	if entries, _ := os.ReadDir(filepath.Join(p.store.Dir(""), "")); len(entries) != 0 {
		t.Errorf("job directories left behind: %v", entries)
	}
}
