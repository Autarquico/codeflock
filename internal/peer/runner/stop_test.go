//go:build linux || darwin

package runner_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/peer/runner"
	"github.com/Autarquico/codeflock/internal/worker/procident"
)

type helper struct {
	cmd   *exec.Cmd
	ended chan struct{}
	t     runner.Target
}

func (h *helper) alive() bool {
	select {
	case <-h.ended:
		return false
	default:
		return true
	}
}

// newWrapperProcess starts a shell as the leader of its own group, as the wrapper of a launch is.
func newWrapperProcess(t *testing.T, script string) *helper {
	t.Helper()
	// The script waits on its input until the identity is taken, so that one that
	// ends at once cannot be gone before it is registered.
	input, hold, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "read _go; "+script) //nolint:gosec // the script of a test
	cmd.Stdin = input
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = input.Close()
	start, err := procident.StartOf(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.WriteString("go\n"); err != nil {
		t.Fatal(err)
	}
	_ = hold.Close()
	h := &helper{cmd: cmd, ended: make(chan struct{}), t: runner.Target{PID: cmd.Process.Pid, PGID: cmd.Process.Pid, Start: start}}
	go func() {
		_ = cmd.Wait()
		close(h.ended)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-h.t.PGID, syscall.SIGKILL)
		<-h.ended
	})
	return h
}

// newTrappingWrapperProcess returns once the trap is in place, so a signal sent at once is
// not taken before the shell installed it.
func newTrappingWrapperProcess(t *testing.T, trap, body string) *helper {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	h := newWrapperProcess(t, trap+"; : > "+ready+"; "+body)
	waitFor(t, "the trap", func() bool { _, err := os.Stat(ready); return err == nil })
	return h
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func groupSize(pgid int) int {
	members, _ := procident.GroupMembers(pgid)
	return len(members)
}

// signals records what a Stopper sends, and forwards it unless it is told to swallow it.
type signals struct {
	mu      sync.Mutex
	sent    []syscall.Signal
	pgids   []int
	swallow bool
}

func (s *signals) send(pgid int, sig syscall.Signal) error {
	s.mu.Lock()
	s.sent, s.pgids = append(s.sent, sig), append(s.pgids, pgid)
	swallow := s.swallow
	s.mu.Unlock()
	if swallow {
		return nil
	}
	return syscall.Kill(-pgid, sig)
}

func (s *signals) got() []syscall.Signal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]syscall.Signal(nil), s.sent...)
}

func stopper(sg *signals, grace, killWait time.Duration) runner.Stopper {
	return runner.Stopper{Grace: grace, KillWait: killWait, Poll: 5 * time.Millisecond, Signal: sg.send}
}

func TestStopAsksTheWrapperGroupAndNobodyElse(t *testing.T) {
	w := newTrappingWrapperProcess(t, "trap 'exit 0' TERM", "while :; do sleep 0.05; done")
	bystander := newWrapperProcess(t, "sleep 60")
	sg := &signals{}

	out, err := stopper(sg, 10*time.Second, time.Second).Stop(t.Context(), w.t)
	if err != nil || out != runner.Stopped {
		t.Fatalf("stop = %v, %v", out, err)
	}
	<-w.ended
	if got := sg.got(); len(got) != 1 || got[0] != syscall.SIGTERM || sg.pgids[0] != w.t.PGID {
		t.Errorf("signals = %v to %v: the wrapper's group is asked once, politely", got, sg.pgids)
	}
	if !bystander.alive() {
		t.Error("a process of another group was signalled")
	}
}

func TestAStopThatTheWrapperDoesNotFinishIsKilledOnlyAfterTheGrace(t *testing.T) {
	w := newTrappingWrapperProcess(t, "trap '' TERM", "while :; do sleep 0.05; done")
	sg := &signals{}
	grace := 600 * time.Millisecond
	done := make(chan runner.Outcome, 1)
	go func() {
		out, _ := stopper(sg, grace, 5*time.Second).Stop(context.Background(), w.t)
		done <- out
	}()
	time.Sleep(grace / 3)
	if !w.alive() || len(sg.got()) != 1 {
		t.Fatalf("killed before the grace: signals %v", sg.got())
	}
	select {
	case out := <-done:
		if out != runner.Stopped {
			t.Fatalf("outcome = %v", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the group was not killed")
	}
	if got := sg.got(); len(got) != 2 || got[1] != syscall.SIGKILL {
		t.Errorf("signals = %v, want TERM then KILL", got)
	}
}

func TestAStopThatCannotVerifyTheWrapperSignalsNothing(t *testing.T) {
	w := newTrappingWrapperProcess(t, "trap 'exit 0' TERM", "while :; do sleep 0.05; done")
	sg := &signals{}
	unknown := w.t
	unknown.Start = "" // no usable token
	out, err := stopper(sg, time.Second, time.Second).Stop(t.Context(), unknown)
	if err != nil || out != runner.NotVerified || len(sg.got()) != 0 || !w.alive() {
		t.Fatalf("outcome %v, %v, signals %v, alive %v", out, err, sg.got(), w.alive())
	}
	if st := stopper(sg, 0, 0).Inspect(unknown); st != runner.Unverified {
		t.Errorf("inspect = %v", st)
	}
}

func TestARecordWhosePidDoesNotLeadItsGroupSignalsNothing(t *testing.T) {
	w := newWrapperProcess(t, "sleep 60")
	odd := w.t // even the right process: a record whose pgid is not its pid says nothing of a group
	odd.PGID = odd.PID + 1
	sg := &signals{}
	out, _ := stopper(sg, time.Second, time.Second).Stop(t.Context(), odd)
	if out != runner.NotVerified || len(sg.got()) != 0 || !w.alive() {
		t.Errorf("outcome %v, signals %v", out, sg.got())
	}
}

func TestAfterARebootNothingIsSignalledWhateverLivesInThatGroupNow(t *testing.T) {
	// The record is of another boot, and the group it names is alive: it is this
	// test's own, standing for whatever got that pid after the reboot.
	own := runner.Target{PID: syscall.Getpgrp(), PGID: syscall.Getpgrp(), Start: "another-boot|1"}
	sg := &signals{}
	out, err := stopper(sg, time.Second, time.Second).Stop(t.Context(), own) // a signal to the group would end this test
	if err != nil || out != runner.Stopped || len(sg.got()) != 0 {
		t.Fatalf("outcome %v, %v, signals %v", out, err, sg.got())
	}
}

// The group outlives its leader: what a wrapper that died left behind is ended too, and the
// stop is not confirmed until it is gone.
func TestTheStopKillsWhatSurvivesAWrapperThatIsGone(t *testing.T) {
	w := newWrapperProcess(t, "sleep 300 & wait")
	waitFor(t, "the straggler", func() bool { return groupSize(w.t.PGID) == 2 })
	_ = syscall.Kill(w.t.PID, syscall.SIGKILL)
	<-w.ended
	sg := &signals{}
	if st := stopper(sg, 0, 0).Inspect(w.t); st != runner.Alive {
		t.Fatalf("inspect = %v: a group that outlives its wrapper is not empty", st)
	}
	out, err := stopper(sg, 10*time.Second, 5*time.Second).Stop(t.Context(), w.t)
	if err != nil || out != runner.Stopped {
		t.Fatalf("stop = %v, %v", out, err)
	}
	if groupSize(w.t.PGID) != 0 {
		t.Error("a process of the group is left")
	}
	if got := sg.got(); len(got) == 0 || got[0] != syscall.SIGKILL {
		t.Errorf("signals = %v: a dead wrapper is not asked, its group is killed", got)
	}
}

// A group that does not end after the kill is never reported stopped.
func TestAGroupThatSurvivesTheKillIsNotStopped(t *testing.T) {
	w := newTrappingWrapperProcess(t, "trap '' TERM", "while :; do sleep 0.05; done")
	sg := &signals{swallow: true}
	out, err := stopper(sg, 50*time.Millisecond, 100*time.Millisecond).Stop(t.Context(), w.t)
	if err != nil || out != runner.Survivors || !w.alive() {
		t.Fatalf("outcome %v, %v, alive %v", out, err, w.alive())
	}
}

func TestAPidThatIsNotAProcessOfOursIsRefused(t *testing.T) {
	for _, id := range []int{-1, 0, 1} {
		sg := &signals{}
		out, err := stopper(sg, time.Second, time.Second).Stop(t.Context(), runner.Target{PID: id, PGID: id, Start: "x"})
		if err == nil || out != runner.NotVerified || len(sg.got()) != 0 {
			t.Errorf("pid %d: outcome %v, err %v, signals %v", id, out, err, sg.got())
		}
	}
}
