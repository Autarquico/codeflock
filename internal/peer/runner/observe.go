package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/support/screen"
	"github.com/Autarquico/codeflock/internal/worker/execwrap"
)

// exitBySignal is the code Observe gives an exit record that has none because a
// signal ended the harness: not a success.
const exitBySignal = -1

// targetOf is the wrapper of a launch as it recorded itself; false if it never
// did. A record that cannot be read or decoded is an error, not an absence: what
// it says is unknown, and no one may conclude from it that the group is gone.
func targetOf(launchDir string) (Target, bool, error) {
	var w execwrap.Wrapper
	ok, err := execwrap.Read(launchDir, execwrap.WrapperFile, &w)
	if err != nil || !ok {
		return Target{}, false, err
	}
	return Target{PID: w.PID, PGID: w.PGID, Start: w.Start}, true, nil
}

// Observe is what is true now of the current launch of j: the facts job.Reconcile
// decides on. A Job that never launched has no process and no session.
func (r *Runner) Observe(ctx context.Context, j job.Job) (job.Facts, error) {
	if j.Launch == nil {
		return job.Facts{GroupEmpty: true}, nil
	}
	store, dir := r.cfg.Store, r.cfg.Store.LaunchDir(j.ID, j.Launch.ID)
	status, err := r.cfg.Tmux.Status(ctx, r.cfg.Tmux.HandleFor(j.Launch.Session))
	if err != nil {
		return job.Facts{}, fmt.Errorf("runner: looking at the session of %s: %w", j.ID, err)
	}
	f := job.Facts{SessionAlive: status.Alive, GroupEmpty: true}
	var exit execwrap.Exit
	if ok, err := readRecord(store.ExitPath(j.ID, j.Launch.ID), &exit); err != nil {
		return job.Facts{}, err
	} else if ok {
		code := exitBySignal
		if exit.ExitCode != nil {
			code = *exit.ExitCode
		}
		f.Exit = &code
	}
	if t, ok, err := targetOf(dir); err != nil {
		return job.Facts{}, err
	} else if ok {
		f.GroupEmpty = r.cfg.Stopper.Inspect(t) == Empty
	}
	f.Waiting = r.waiting(j, store.WaitingPath(j.ID, j.Launch.ID))
	return f, nil
}

// waiting says whether the marker is there and nothing since has superseded it.
func (r *Runner) waiting(j job.Job, marker string) bool {
	info, err := os.Stat(marker)
	if err != nil {
		return false
	}
	if s, ok := r.agents[j.Harness].(staleWaiting); ok && s.WaitingStale(j.SessionID, info.ModTime()) {
		return false
	}
	return true
}

// Reconcile observes the Job and applies the state job.Reconcile gives, under the
// Job's lock. It is what every command that reads a Job does first.
func (r *Runner) Reconcile(ctx context.Context, id string) (job.Job, error) {
	var out job.Job
	err := r.cfg.Store.WithLock(id, func(j *job.Job) error {
		defer func() { out = *j }()
		return r.reconcile(ctx, j)
	})
	return out, err
}

// reconcile is Reconcile on a Job whose lock the caller holds.
func (r *Runner) reconcile(ctx context.Context, j *job.Job) error {
	_ = r.importSession(ctx, j) // an ambiguous session is Launch's to report
	switch j.State {
	case job.Running, job.Waiting, job.Lost:
	default:
		return nil
	}
	f, err := r.Observe(ctx, *j)
	if err != nil {
		return err
	}
	if to := job.Reconcile(j.State, f); to != j.State {
		return j.Transition(to)
	}
	return nil
}

// Screen is what the terminal of the Job's agent shows now, with the escape
// sequences that are not colours removed.
func (r *Runner) Screen(ctx context.Context, id string) ([]byte, error) {
	j, err := r.cfg.Store.Get(id)
	if err != nil {
		return nil, err
	}
	if j.Launch == nil {
		return nil, fmt.Errorf("runner: job %s has not launched", id)
	}
	raw, err := r.cfg.Tmux.Snapshot(ctx, r.cfg.Tmux.HandleFor(j.Launch.Session))
	if err != nil {
		return nil, err
	}
	return screen.Sanitize(raw), nil
}

// Send types text into the agent of a running or waiting Job and clears the
// waiting marker: the agent is working on it now.
func (r *Runner) Send(ctx context.Context, id, text string) error {
	return r.cfg.Store.WithLock(id, func(j *job.Job) error {
		if j.Launch == nil || (j.State != job.Running && j.State != job.Waiting) {
			return fmt.Errorf("runner: job %s is %s: there is no agent to send to", id, j.State)
		}
		// The marker is set aside while the text goes in: a turn that ends quickly
		// writes the next marker, which clearing it afterwards would delete. If the
		// send fails the agent may still be waiting, so the marker comes back (unless
		// a newer one was written meanwhile): a marker restored for an Enter that did
		// get through is rewritten or superseded by the next turn, one lost for good
		// is not.
		marker := r.cfg.Store.WaitingPath(j.ID, j.Launch.ID)
		aside := marker + ".sending"
		if err := os.Rename(marker, aside); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("runner: clearing the waiting marker: %w", err)
		}
		if err := r.cfg.Tmux.SendText(ctx, r.cfg.Tmux.HandleFor(j.Launch.Session), text); err != nil {
			restoreMarker(aside, marker)
			return err
		}
		_ = os.Remove(aside)
		if j.State == job.Waiting {
			return j.Transition(job.Running)
		}
		return nil
	})
}

// restoreMarker puts the marker set aside back unless one was written meanwhile:
// that one is newer and stays (a link fails where the name exists, a rename would
// replace it).
func restoreMarker(aside, marker string) {
	_ = os.Link(aside, marker)
	_ = os.Remove(aside)
}

// Stop writes stopping, ends the group of the Job's launch (see Stopper.Stop) and,
// only with the group gone, destroys its session and writes stopped. The Job is
// reconciled and made stopping under one hold of the lock, so that an agent that
// finished cleanly is done, not stopped. The lock is not held while the group
// ends, which may take a while: stopping is on disk meanwhile, so that ps and show
// see it. If something of the group survives, or the wrapper cannot be identified
// or read, the Job stays stopping and Stop says why: a later Stop repeats it. A Job
// already stopped is left alone.
func (r *Runner) Stop(ctx context.Context, id string) error {
	var launch job.Job
	var already bool
	err := r.cfg.Store.WithLockSave(id, func(j *job.Job, save func() error) error {
		if err := r.reconcile(ctx, j); err != nil {
			return err
		}
		if err := save(); err != nil { // what reconcile found stays, whatever Stop does next
			return err
		}
		if already = j.State == job.Stopped; already {
			return nil
		}
		if j.State != job.Stopping { // a retry keeps what the first Stop began from
			j.StoppingFrom = j.State
		}
		launch = *j
		return j.Transition(job.Stopping)
	})
	if err != nil || already {
		return err
	}
	if launch.Launch != nil {
		if notStopped := r.endLaunch(ctx, r.cfg.Store.LaunchDir(id, launch.Launch.ID), launch.Launch.Session); notStopped != nil {
			return fmt.Errorf("runner: job %s stays stopping: %w", id, notStopped)
		}
	}
	return r.cfg.Store.WithLock(id, func(j *job.Job) error {
		if j.State != job.Stopping || !sameLaunch(j.Launch, launch.Launch) { // another Stop, or a later launch, got here first
			return nil
		}
		from := j.StoppingFrom
		j.StoppingFrom = ""
		if from == job.Failed || launch.Launch == nil {
			return j.Transition(job.Stopped) // it had failed already: its exit is not a new outcome
		}
		// The group is empty: if the wrapper recorded that the harness ended by itself
		// while a Stop began, that is how the Job ended, not by this Stop.
		var exit execwrap.Exit
		ok, err := readRecord(r.cfg.Store.ExitPath(id, launch.Launch.ID), &exit)
		if err != nil {
			return fmt.Errorf("runner: job %s stays stopping: %w", id, err)
		}
		if ok && exit.Cause == execwrap.CauseExit && exit.Error == "" {
			if exit.ExitCode != nil && *exit.ExitCode == 0 {
				return j.Transition(job.Done)
			}
			return j.Transition(job.Failed)
		}
		return j.Transition(job.Stopped)
	})
}

func sameLaunch(a, b *job.Launch) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ID == b.ID
}

// endLaunch ends the group of a launch and its tmux session, and says why it could
// not be verified that nothing of it is left. A launch whose wrapper has not
// recorded itself may still be about to: its session is destroyed first and the
// record looked for again.
func (r *Runner) endLaunch(ctx context.Context, launchDir, session string) error {
	recorded, err := r.stopGroup(ctx, launchDir)
	if err != nil {
		return err
	}
	if err := r.cfg.Tmux.Stop(ctx, r.cfg.Tmux.HandleFor(session)); err != nil {
		return err
	}
	if recorded {
		return nil
	}
	_, err = r.stopGroup(ctx, launchDir)
	return err
}

// stopGroup stops the group the wrapper of the launch recorded, if it did.
func (r *Runner) stopGroup(ctx context.Context, launchDir string) (recorded bool, err error) {
	t, ok, err := targetOf(launchDir)
	if err != nil || !ok {
		return false, err
	}
	switch outcome, err := r.cfg.Stopper.Stop(ctx, t); {
	case err != nil:
		return true, err
	case outcome == Survivors:
		return true, errors.New("a process of the launch survived the kill")
	case outcome == NotVerified:
		return true, errors.New("the wrapper of the launch could not be identified")
	}
	return true, nil
}
