// Package runner stops the agent of a Job. The wrapper that runs the harness leads its
// own process group and records its identity; stopping is asking that group to end,
// only after checking that the process with the recorded pid is still the wrapper,
// and waiting for the whole group, not just the wrapper, to be gone.
package runner

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/Autarquico/codeflock/internal/worker/execwrap"
	"github.com/Autarquico/codeflock/internal/worker/procident"
)

// Target is the identity the wrapper recorded of itself.
type Target struct {
	PID, PGID int
	// Start is the opaque token of when the wrapper started (procident).
	Start string
}

// State is what is left of the group of a launch.
type State int

// States of a group.
const (
	// Empty: nothing of the group is alive, or nothing that is alive is ours.
	Empty State = iota
	// Alive: the wrapper or something of its group is still running.
	Alive
	// Unverified: the wrapper's identity cannot be established; nothing is signalled.
	Unverified
)

// Defaults of a Stopper.
const (
	// WrapperGrace is how long the wrapper itself gives the harness after a SIGTERM before
	// it escalates; it writes the exit record and the final output when that is over.
	WrapperGrace = execwrap.DefaultGrace
	// GraceMargin is what the stopper waits beyond WrapperGrace, so that a wrapper that
	// cooperates is always done before the stopper kills the group under it.
	GraceMargin     = 10 * time.Second
	DefaultGrace    = WrapperGrace + GraceMargin
	DefaultKillWait = 10 * time.Second
	DefaultPoll     = 100 * time.Millisecond
)

// Stopper ends the group of a launch.
type Stopper struct {
	// Grace is how long the group has to end after the first signal before it is
	// killed, and KillWait how long it has after that. Zero is the default.
	Grace, KillWait time.Duration
	// Poll is how often the group is looked at. Zero is the default.
	Poll time.Duration
	// Signal sends sig to the group; nil is kill(-pgid, sig).
	Signal func(pgid int, sig syscall.Signal) error
	// Verify says who the process with the wrapper's pid is; nil is procident.Verify.
	// A test seam: the identity is re-checked right before every signal.
	Verify func(pid int, start string) procident.Verdict
}

func (s Stopper) grace() time.Duration    { return orDefault(s.Grace, DefaultGrace) }
func (s Stopper) killWait() time.Duration { return orDefault(s.KillWait, DefaultKillWait) }
func (s Stopper) poll() time.Duration     { return orDefault(s.Poll, DefaultPoll) }

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (s Stopper) signal(pgid int, sig syscall.Signal) error {
	if s.Signal != nil {
		return s.Signal(pgid, sig)
	}
	return syscall.Kill(-pgid, sig)
}

// verdict is what the process with the wrapper's pid is now. The wrapper leads its own
// group (pgid == pid), which is what lets a verdict about the pid say something about
// the group; a record where that is not so says nothing.
func (s Stopper) verdict(t Target) procident.Verdict {
	if t.PGID != t.PID {
		return procident.Unknown
	}
	if s.Verify != nil {
		return s.Verify(t.PID, t.Start)
	}
	return procident.Verify(t.PID, t.Start)
}

// Inspect says what is left of the group of t without signalling anything.
func (s Stopper) Inspect(t Target) State {
	st, _ := s.inspect(t)
	return st
}

func (s Stopper) inspect(t Target) (State, procident.Verdict) {
	v := s.verdict(t)
	switch v {
	case procident.Same:
		return Alive, v
	case procident.Gone:
		// The wrapper is gone, so a group with this ID is what it left behind.
		if !s.groupEmpty(t) {
			return Alive, v
		}
		return Empty, v
	case procident.Rebooted, procident.Different:
		// A pid is not reused while a group with its ID has members, and whatever has
		// this ID now is not ours.
		return Empty, v
	}
	return Unverified, v
}

// Outcome is how a stop ended.
type Outcome int

// Outcomes of Stop.
const (
	// Stopped: the whole group is gone.
	Stopped Outcome = iota
	// Survivors: something of the group outlived the kill; the Job stays stopping.
	Survivors
	// NotVerified: the wrapper could not be identified; nothing was signalled, or the
	// identity was lost on the way, and the stop is not confirmed.
	NotVerified
)

// Stop asks the group of t to end with SIGTERM, kills it if it is still there after
// Grace, and gives up KillWait after that. It returns Stopped only when the group is
// empty, never on the wrapper's exit alone.
//
// Grace defaults to longer than the grace of the wrapper (WrapperGrace), which manages
// its own escalation and writes its exit record; the stopper's kill is the backstop.
//
// The identity of the wrapper is verified again immediately before every signal, and a
// group whose leader fails that check is not signalled. A window remains between that
// check and the kill(2) in which the pid could be reused; closing it needs a pidfd
// (Linux), which is out of scope here.
func (s Stopper) Stop(ctx context.Context, t Target) (Outcome, error) {
	if t.PID <= 1 || t.PGID <= 1 {
		return NotVerified, fmt.Errorf("runner: refusing to stop pid %d, pgid %d: not a process this peer launched", t.PID, t.PGID)
	}
	st, v := s.inspect(t)
	switch st {
	case Unverified:
		return NotVerified, nil
	case Empty:
		return Stopped, nil
	}
	var killedAt time.Time
	if v == procident.Same {
		switch s.verdict(t) {
		case procident.Same:
			// ESRCH: the group is gone. EPERM: macOS says it for a group that holds only
			// zombies (the wrapper exited and is not reaped yet); the poll below decides
			// whether anything is really left, and gives up after KillWait if so.
			if err := s.signal(t.PGID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
				return Survivors, err
			}
		case procident.Gone:
			// It exited since the first look: from here on it is a wrapper that is gone.
			v = procident.Gone
		default:
			return NotVerified, nil
		}
	}
	if v != procident.Same {
		// A wrapper that is gone with a group that is not: the survivors are ours to end.
		if s.groupEmpty(t) {
			return Stopped, nil
		}
		if s.kill(t) {
			killedAt = time.Now()
		}
	}
	start := time.Now()
	tick := time.NewTicker(s.poll())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return Survivors, ctx.Err()
		case <-tick.C:
		}
		st, v = s.inspect(t)
		switch st {
		case Unverified:
			return NotVerified, nil
		case Empty:
			return Stopped, nil
		}
		elapsed := time.Since(start)
		if killedAt.IsZero() && (elapsed >= s.grace() || v != procident.Same) && s.kill(t) {
			killedAt = time.Now()
		}
		if !killedAt.IsZero() && time.Since(killedAt) >= s.killWait() {
			return Survivors, nil
		}
	}
}

// groupEmpty says that nothing of the group is left; not knowing is not empty.
func (s Stopper) groupEmpty(t Target) bool {
	empty, err := procident.GroupEmpty(t.PGID)
	return err == nil && empty
}

// kill sends SIGKILL to the group, after checking again who leads it: a wrapper that
// is something else now is not signalled, and a wrapper that is gone is only followed
// into its group while the group has members. It says whether it signalled.
func (s Stopper) kill(t Target) bool {
	switch s.verdict(t) {
	case procident.Same:
	case procident.Gone:
		if s.groupEmpty(t) {
			return false
		}
	default:
		return false
	}
	_ = s.signal(t.PGID, syscall.SIGKILL)
	return true
}
