package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
	cfruntime "github.com/Autarquico/codeflock/internal/adapters/runtime"
	"github.com/Autarquico/codeflock/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/support/atomicfile"
	"github.com/Autarquico/codeflock/internal/worker/execwrap"
)

// Timings of a launch.
const (
	// DefaultStartTimeout is how long Launch waits for the wrapper to record that
	// the harness started: the wrapper itself waits up to execwrap.GateTimeout for
	// the terminal to be wired.
	DefaultStartTimeout = execwrap.GateTimeout + 10*time.Second
	// DefaultDiscoverTimeout is how long a harness that picks its own session id
	// has, once it started, to create its session.
	DefaultDiscoverTimeout = 30 * time.Second
	pollEvery              = 50 * time.Millisecond
)

// SessionFile and PaneReadyFile are files of a launch directory besides the
// wrapper's own records and the waiting marker.
const (
	SessionFile   = "session_id"
	PaneReadyFile = "pane.ready"
)

// Config is what a Runner runs on.
type Config struct {
	Store *job.Store
	Host  harness.Host
	// Tmux is the runtime of the launches. Its socket (-L) is the caller's: a
	// test gives it one of its own.
	Tmux *tmux.Runtime
	// Wrapper is the argv that runs execwrap.Run on a launch directory (cflk's own
	// hidden command; a test binary in a role).
	Wrapper func(launchDir string) []string
	// NotifyCmd is the argv of the program Codex runs when a turn ends; Codex appends
	// the JSON of the notification, and the launch the marker and the session file
	// before it. The program calls HandleNotify.
	NotifyCmd []string
	// Agents adds to, or replaces, the harnesses the Runner knows (claude-code and codex).
	Agents map[string]Agent
	// Stopper ends the groups of launches; its zero value is the default.
	Stopper Stopper
	// StartTimeout and DiscoverTimeout default to the constants above.
	StartTimeout, DiscoverTimeout time.Duration
}

// Runner starts, watches, talks to and stops the agent of a Job on this peer.
type Runner struct {
	cfg    Config
	agents map[string]Agent
}

// New builds a Runner.
func New(cfg Config) *Runner {
	agents := map[string]Agent{
		claude.ID: claudeAgent{host: cfg.Host},
		codex.ID:  codexAgent{host: cfg.Host},
	}
	for id, a := range cfg.Agents {
		agents[id] = a
	}
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = DefaultStartTimeout
	}
	if cfg.DiscoverTimeout == 0 {
		cfg.DiscoverTimeout = DefaultDiscoverTimeout
	}
	return &Runner{cfg: cfg, agents: agents}
}

// resumable are the states a launch may start from.
var resumable = []job.State{job.Stopped, job.Lost, job.Failed}

// Launch starts the agent of the Job as a new launch: its own id, the tmux session
// cflk-<job>-<launch>, the harness under the wrapper, in the worktree of the Job's
// generation. A Job without a session names one (Claude) or has it discovered
// (Codex); one with a session resumes it, and prompt may then be empty. The Job
// must be in a state that may become running (stopped, failed or lost). It returns
// the Job with its new launch, running.
//
// The launch is written to the Job, running, before anything starts, so that
// whatever the wrapper starts is always referenced by job.json. If the agent does
// not start and its group is verified gone, the Job is as it was. If it cannot be
// verified gone, the Job is left stopping with the launch recorded, and the error
// says so: `stop` ends it.
func (r *Runner) Launch(ctx context.Context, id, prompt string) (job.Job, error) {
	var out job.Job
	err := r.cfg.Store.WithLockSave(id, func(j *job.Job, save func() error) error {
		if !slices.Contains(resumable, j.State) {
			return fmt.Errorf("job %s is %s, it cannot be launched: only a stopped, lost or failed Job is: %w", j.ID, j.State, job.ErrInvalidTransition)
		}
		agent, ok := r.agents[j.Harness]
		if !ok {
			return fmt.Errorf("runner: job %s: unknown harness %q", j.ID, j.Harness)
		}
		if err := r.importSession(ctx, j); err != nil && prompt == "" {
			return fmt.Errorf("runner: job %s cannot be resumed: %w", j.ID, err)
		}
		before := *j
		l := job.NewLaunch(j.ID)
		if err := j.Transition(job.Running); err != nil {
			return err
		}
		j.Launch = &l
		if err := save(); err != nil {
			return err
		}
		in := Input{
			Job:       before,
			Worktree:  r.cfg.Store.WorktreeDir(j.ID, j.Gen),
			LaunchDir: r.cfg.Store.LaunchDir(j.ID, l.ID),
			Waiting:   r.cfg.Store.WaitingPath(j.ID, l.ID),
			SessionID: j.SessionID,
			Prompt:    prompt,
			NotifyCmd: r.cfg.NotifyCmd,
		}
		in.SessionFile = filepath.Join(in.LaunchDir, SessionFile)
		// A session known before the start (Claude names it) is on disk with the
		// launch, so that a launch cut short can still be resumed.
		known := func(session string) error {
			if j.SessionID == session {
				return nil
			}
			j.SessionID = session
			return save()
		}
		session, err := r.start(ctx, agent, in, l, known)
		if err != nil {
			return r.failLaunch(j, before, in, save, err)
		}
		j.SessionID = session
		out = *j
		return nil
	})
	return out, err
}

// failLaunch is what becomes of a Job whose launch did not come to be: the Job
// as it was if the launch left nothing running, stopping with the launch recorded
// (and the session it ran, if known) if that cannot be verified.
func (r *Runner) failLaunch(j *job.Job, before job.Job, in Input, save func() error, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*DefaultGrace)
	defer cancel()
	left := r.endLaunch(ctx, in.LaunchDir, j.Launch.Session)
	if left == nil {
		*j = before
		if err := save(); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	if raw, err := os.ReadFile(in.SessionFile); err == nil && j.SessionID == "" { //nolint:gosec // a file of the launch
		j.SessionID = string(raw)
	}
	if err := j.Transition(job.Stopping); err != nil {
		return errors.Join(cause, left, err)
	}
	if err := save(); err != nil {
		return errors.Join(cause, left, err)
	}
	return fmt.Errorf("%w; its agent may still run (%w), so job %s is stopping: stop it before launching again", cause, left, j.ID)
}

func (r *Runner) start(ctx context.Context, agent Agent, in Input, l job.Launch, known func(session string) error) (string, error) {
	if info, err := os.Stat(in.Worktree); err != nil || !info.IsDir() {
		return "", fmt.Errorf("runner: job %s has no worktree at %s", in.Job.ID, in.Worktree)
	}
	if err := os.MkdirAll(in.LaunchDir, 0o700); err != nil {
		return "", fmt.Errorf("runner: creating the launch directory: %w", err)
	}
	plan, err := agent.Prepare(ctx, in)
	if err != nil {
		return "", err
	}
	if plan.SessionID != "" {
		if err := writeSession(in.SessionFile, plan.SessionID); err != nil {
			return "", err
		}
		if err := known(plan.SessionID); err != nil {
			return "", err
		}
	}
	ready := filepath.Join(in.LaunchDir, PaneReadyFile)
	launch := execwrap.Launch{Argv: plan.Argv, Env: plan.Env, UnsetEnv: plan.UnsetEnv, Dir: in.Worktree, Mode: execwrap.ModeInteractive, Grace: execwrap.DefaultGrace, Gate: ready}
	if err := execwrap.Write(in.LaunchDir, execwrap.LaunchFile, launch); err != nil {
		return "", err
	}
	spec := cfruntime.Spec{
		Name: l.Session, Dir: in.Worktree, Command: r.cfg.Wrapper(in.LaunchDir),
		Capture: filepath.Join(in.LaunchDir, execwrap.OutputLog), Ready: ready,
	}
	if _, err := r.cfg.Tmux.Start(ctx, spec); err != nil {
		return "", fmt.Errorf("runner: starting the session of the launch: %w", err)
	}
	return r.settle(ctx, agent, in, l, plan.SessionID)
}

// settle waits for the harness to have started and, if its session is not known,
// for it to exist.
func (r *Runner) settle(ctx context.Context, agent Agent, in Input, l job.Launch, session string) (string, error) {
	var started execwrap.Started
	err := poll(ctx, r.cfg.StartTimeout, func() (bool, error) {
		if ok, err := readRecord(r.cfg.Store.StartedPath(in.Job.ID, l.ID), &started); err != nil || ok {
			return ok, err
		}
		var exit execwrap.Exit
		if ok, err := readRecord(r.cfg.Store.ExitPath(in.Job.ID, l.ID), &exit); err != nil {
			return false, err
		} else if ok {
			return false, fmt.Errorf("runner: the agent did not start: %s", orUnknown(exit.Error))
		}
		return false, nil
	})
	if err != nil {
		return "", fmt.Errorf("runner: waiting for the agent to start: %w", err)
	}
	if session != "" {
		return session, nil
	}
	err = poll(ctx, r.cfg.DiscoverTimeout, func() (bool, error) {
		var err error
		session, err = r.discover(ctx, agent, in, r.cfg.Store.ExitPath(in.Job.ID, l.ID), started.StartedAt)
		return session != "", err
	})
	if err != nil {
		return "", fmt.Errorf("runner: finding the session of the launch: %w", err)
	}
	return session, recordSession(in, session)
}

// discover is the session the harness created for the launch, between its start
// and its end (now, if it has not ended); empty if there is none yet. Several are
// an error.
func (r *Runner) discover(ctx context.Context, agent Agent, in Input, exitPath string, started time.Time) (string, error) {
	before := time.Now()
	var exit execwrap.Exit
	if ok, err := readRecord(exitPath, &exit); err != nil {
		return "", err
	} else if ok {
		before = exit.ExitedAt
	}
	ids, err := agent.Discover(ctx, in, started, before)
	switch {
	case err != nil:
		return "", err
	case len(ids) > 1:
		return "", fmt.Errorf("%d sessions of the harness were created in %s during the launch: %v", len(ids), in.Worktree, ids)
	case len(ids) == 1:
		return ids[0], nil
	}
	return "", nil
}

// recordSession is what a launch does once it knows its session, however it came
// to know it: write it down, then see whether its first turn had already ended.
func recordSession(in Input, session string) error {
	if err := writeSession(in.SessionFile, session); err != nil {
		return err
	}
	return markTurnsEndedBefore(in.Waiting, in.SessionFile, session)
}

// importSession gives a Job that lacks a session the one its current launch
// discovered and wrote to its session file or, if the launch was cut short before
// writing it, the one the harness created during the launch (found and recorded
// as the launch would). Several candidates are an error and leave the Job without
// a session.
func (r *Runner) importSession(ctx context.Context, j *job.Job) error {
	if j.SessionID != "" || j.Launch == nil {
		return nil
	}
	launchDir := r.cfg.Store.LaunchDir(j.ID, j.Launch.ID)
	file := filepath.Join(launchDir, SessionFile)
	if raw, err := os.ReadFile(file); err == nil { //nolint:gosec // a file of the launch
		j.SessionID = strings.TrimSpace(string(raw))
		return nil
	}
	agent, ok := r.agents[j.Harness]
	if !ok {
		return nil
	}
	var started execwrap.Started
	if ok, err := readRecord(r.cfg.Store.StartedPath(j.ID, j.Launch.ID), &started); err != nil || !ok {
		return err
	}
	in := Input{
		Job: *j, Worktree: r.cfg.Store.WorktreeDir(j.ID, j.Gen), LaunchDir: launchDir,
		Waiting: r.cfg.Store.WaitingPath(j.ID, j.Launch.ID), SessionFile: file,
	}
	session, err := r.discover(ctx, agent, in, r.cfg.Store.ExitPath(j.ID, j.Launch.ID), started.StartedAt)
	if err != nil || session == "" {
		return err
	}
	j.SessionID = session
	return recordSession(in, session)
}

func writeSession(path, id string) error {
	if err := atomicfile.Write(path, []byte(id), 0o600); err != nil {
		return fmt.Errorf("runner: recording the session: %w", err)
	}
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "no reason recorded"
	}
	return s
}

// poll runs check every pollEvery until it says it is done, fails, or timeout or ctx ends.
func poll(ctx context.Context, timeout time.Duration, check func() (bool, error)) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		if done, err := check(); err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("timed out")
		case <-tick.C:
		}
	}
}

// readRecord loads a record the wrapper wrote at path; ok is false if it is not there yet.
func readRecord(path string, v any) (bool, error) {
	return execwrap.Read(filepath.Dir(path), filepath.Base(path), v)
}
