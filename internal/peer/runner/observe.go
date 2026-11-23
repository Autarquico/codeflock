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

// targetOf is the wrapper of a launch as it recorded itself; false if it never did.
func targetOf(launchDir string) (Target, bool) {
	var w execwrap.Wrapper
	ok, err := execwrap.Read(launchDir, execwrap.WrapperFile, &w)
	if err != nil || !ok {
		return Target{}, false
	}
	return Target{PID: w.PID, PGID: w.PGID, Start: w.Start}, true
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
	if ok, err := execwrap.Read(dir, execwrap.ExitFile, &exit); err != nil {
		return job.Facts{}, err
	} else if ok {
		code := exitBySignal
		if exit.ExitCode != nil {
			code = *exit.ExitCode
		}
		f.Exit = &code
	}
	if t, ok := targetOf(dir); ok {
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
	})
	return out, err
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
		if err := r.cfg.Tmux.SendText(ctx, r.cfg.Tmux.HandleFor(j.Launch.Session), text); err != nil {
			return err
		}
		if err := os.Remove(r.cfg.Store.WaitingPath(j.ID, j.Launch.ID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("runner: clearing the waiting marker: %w", err)
		}
		if j.State == job.Waiting {
			return j.Transition(job.Running)
		}
		return nil
	})
}

// Stop writes stopping, ends the group of the Job's launch (see Stopper.Stop) and,
// only with the group gone, destroys its session and writes stopped. If something
// of the group survives, or the wrapper cannot be identified, the Job stays
// stopping, which is written, and Stop says why: a later Stop repeats it. A Job
// already stopped is left alone.
func (r *Runner) Stop(ctx context.Context, id string) error {
	var notStopped error
	err := r.cfg.Store.WithLock(id, func(j *job.Job) error {
		if j.State == job.Stopped {
			return nil
		}
		if err := j.Transition(job.Stopping); err != nil {
			return err
		}
		if notStopped = r.endLaunch(ctx, *j); notStopped != nil {
			return nil // stopping is what is true, and it is kept
		}
		return j.Transition(job.Stopped)
	})
	if err != nil {
		return err
	}
	if notStopped != nil {
		return fmt.Errorf("runner: job %s stays stopping: %w", id, notStopped)
	}
	return nil
}

func (r *Runner) endLaunch(ctx context.Context, j job.Job) error {
	if j.Launch == nil {
		return nil
	}
	if t, ok := targetOf(r.cfg.Store.LaunchDir(j.ID, j.Launch.ID)); ok {
		switch outcome, err := r.cfg.Stopper.Stop(ctx, t); {
		case err != nil:
			return err
		case outcome == Survivors:
			return errors.New("a process of the launch survived the kill")
		case outcome == NotVerified:
			return errors.New("the wrapper of the launch could not be identified")
		}
	}
	return r.cfg.Tmux.Stop(ctx, r.cfg.Tmux.HandleFor(j.Launch.Session))
}
