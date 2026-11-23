// Package runner stops the agent of a Job. The wrapper that runs the harness leads its
// own process group and records its identity; stopping is asking that group to end,
// only after checking that the process with the recorded pid is still the wrapper,
// and waiting for the whole group, not just the wrapper, to be gone.
package runner

import (
	"context"
	"errors"
	"syscall"
	"time"
)

// Verdict is what comparing a recorded identity with the process that has the pid now says.
type Verdict int

// Verdicts, with the meaning of the identity check of the wrapper.
const (
	// Gone: no process has the pid.
	Gone Verdict = iota
	// Same: a process has the pid and started when it was recorded.
	Same
	// Different: the pid was reused, so the process recorded is gone.
	Different
	// Unknown: the platform or the record would not say; nothing may be signalled.
	Unknown
	// Rebooted: the record is of another boot: nothing of the group is left, and nothing
	// may be signalled on its account.
	Rebooted
)

// Target is the identity the wrapper recorded of itself.
type Target struct {
	PID, PGID int
	// Start is the opaque token of when the wrapper started (worker/procident).
	Start string
}

// Prober looks at the machine. It is given to the Stopper so that this package does
// not know how a platform tells when a process started.
type Prober interface {
	// Verify compares the process that has pid now with the token recorded for it.
	Verify(pid int, start string) Verdict
	// GroupEmpty says whether the group has no live process left.
	GroupEmpty(pgid int) (bool, error)
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
	DefaultGrace    = 30 * time.Second
	DefaultKillWait = 10 * time.Second
	DefaultPoll     = 100 * time.Millisecond
)

// Stopper ends the group of a launch.
type Stopper struct {
	Prober Prober
	// Grace is how long the group has to end after the first signal before it is
	// killed, and KillWait how long it has after that. Zero is the default.
	Grace, KillWait time.Duration
	// Poll is how often the group is looked at. Zero is the default.
	Poll time.Duration
	// Signal sends sig to the group; nil is kill(-pgid, sig).
	Signal func(pgid int, sig syscall.Signal) error
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
func (s Stopper) verdict(t Target) Verdict {
	if t.PGID != t.PID {
		return Unknown
	}
	return s.Prober.Verify(t.PID, t.Start)
}

// Inspect says what is left of the group of t without signalling anything.
func (s Stopper) Inspect(t Target) State {
	st, _ := s.inspect(t)
	return st
}

func (s Stopper) inspect(t Target) (State, Verdict) {
	v := s.verdict(t)
	switch v {
	case Same:
		return Alive, v
	case Gone:
		// The wrapper is gone, so a group with this ID is what it left behind.
		if empty, err := s.Prober.GroupEmpty(t.PGID); err != nil || !empty {
			return Alive, v // not knowing is not empty
		}
		return Empty, v
	case Rebooted, Different:
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
func (s Stopper) Stop(ctx context.Context, t Target) (Outcome, error) {
	st, v := s.inspect(t)
	switch st {
	case Unverified:
		return NotVerified, nil
	case Empty:
		return Stopped, nil
	}
	var killedAt time.Time
	if v == Same {
		if err := s.signal(t.PGID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return Survivors, err
		}
	} else {
		// A wrapper that is gone with a group that is not: the survivors are ours to end.
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
		if killedAt.IsZero() && (elapsed >= s.grace() || v != Same) && s.kill(t) {
			killedAt = time.Now()
		}
		if !killedAt.IsZero() && time.Since(killedAt) >= s.killWait() {
			return Survivors, nil
		}
	}
}

// kill sends SIGKILL to the group, after checking again who leads it: a wrapper that
// is something else now is not signalled. It says whether it signalled.
func (s Stopper) kill(t Target) bool {
	if v := s.verdict(t); v != Same && v != Gone {
		return false
	}
	_ = s.signal(t.PGID, syscall.SIGKILL)
	return true
}
