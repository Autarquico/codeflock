package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
	"github.com/Autarquico/codeflock/internal/adapters/harness/localhost"
	cfruntime "github.com/Autarquico/codeflock/internal/adapters/runtime"
	"github.com/Autarquico/codeflock/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codeflock/internal/attachwindow"
	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/runner"
)

// stateDirVar overrides where this peer keeps its Jobs (~/.local/state/cflk).
const stateDirVar = "CFLK_STATE_DIR"

// localJobs is cli.Jobs over the Jobs of this machine. What needs the home
// directory, tmux or the harnesses is built on first use, so that `cflk version`
// and `cflk doctor` never depend on it.
type localJobs struct {
	exe  string
	over overrides
	once sync.Once
	peer *peer
	err  error
}

// peer is this machine as a place where Jobs run.
type peer struct {
	name   string
	store  *job.Store
	tmux   *tmux.Runtime
	runner *runner.Runner
}

// overrides are what a test of this package swaps in the composition: the
// agents that stand for the harnesses, and the program a launch runs as its
// wrapper. They are not flags of the binary.
type overrides struct {
	agents  map[string]runner.Agent
	wrapper func(launchDir string) []string
}

func newLocalJobs(exe string, over ...overrides) *localJobs {
	l := &localJobs{exe: exe}
	if len(over) > 0 {
		l.over = over[0]
	}
	return l
}

func (l *localJobs) open() (*peer, error) {
	l.once.Do(func() { l.peer, l.err = openPeer(l.exe, l.over) })
	return l.peer, l.err
}

func openPeer(exe string, over overrides) (*peer, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root := os.Getenv(stateDirVar)
	if root == "" {
		if root, err = job.DefaultRoot(); err != nil {
			return nil, err
		}
	}
	host, err := localhost.New(localhost.Options{Home: home, Env: os.Environ()})
	if err != nil {
		return nil, err
	}
	// One tmux server per state root: a root of its own (a test, a second user
	// state) never meets the launches of another.
	rt, err := tmux.New(tmux.Options{Socket: tmux.SocketName(root)})
	if err != nil {
		return nil, err
	}
	store := job.NewStore(root)
	hostname, _ := os.Hostname()
	wrapper := over.wrapper
	if wrapper == nil {
		wrapper = func(dir string) []string { return []string{exe, "internal", "wrapper", dir} }
	}
	return &peer{
		name:  config.MachineName(hostname, "this machine"),
		store: store,
		tmux:  rt,
		runner: runner.New(runner.Config{
			Store: store, Host: host, Tmux: rt,
			Wrapper:   wrapper,
			NotifyCmd: []string{exe, "internal", "notify"},
			Agents:    over.agents,
		}),
	}, nil
}

// harnessIDs are the names a person gives a harness.
var harnessIDs = map[string]string{"": claude.ID, "claude": claude.ID, claude.ID: claude.ID, "codex": codex.ID}

func (l *localJobs) Run(ctx context.Context, req cli.RunRequest) (cli.JobView, error) {
	p, err := l.open()
	if err != nil {
		return cli.JobView{}, err
	}
	harnessID, ok := harnessIDs[req.Harness]
	if !ok {
		return cli.JobView{}, invalid("unknown harness %q: use claude or codex", req.Harness)
	}
	if req.On != "" && req.On != "local" && req.On != p.name {
		return cli.JobView{}, invalid("--on %q: other peers are not available yet; a Job runs on this peer (%s)", req.On, p.name)
	}
	if err := p.tmux.Check(ctx); err != nil {
		return cli.JobView{}, err
	}
	top, err := git(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return cli.JobView{}, invalid("cflk run starts from a git repository: %v", err)
	}
	origin, err := git(ctx, top, "remote", "get-url", "origin")
	if err != nil {
		return cli.JobView{}, invalid("the repository has no origin remote: %v", err)
	}
	repo, err := job.NormalizeRepo(origin)
	if err != nil {
		return cli.JobView{}, invalid("origin %q: %v", origin, err)
	}
	base := req.Branch
	if base == "" {
		base = "HEAD"
	}
	commit, err := git(ctx, top, "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
	if err != nil {
		return cli.JobView{}, invalid("%q is not a commit of this repository: %v", base, err)
	}
	j, err := p.store.Create(job.Job{Repo: repo, Harness: harnessID, Gen: 1, Peer: p.name, State: job.Stopped, CreatedBy: p.name})
	if err != nil {
		return cli.JobView{}, err
	}
	worktree := p.store.WorktreeDir(j.ID, j.Gen)
	if _, err := git(ctx, top, "worktree", "add", "-b", j.Branch, worktree, commit); err != nil {
		_ = os.RemoveAll(p.store.Dir(j.ID))
		return cli.JobView{}, err
	}
	launched, err := p.runner.Launch(ctx, j.ID, req.Prompt)
	if err != nil {
		// Nothing runs and nothing was said to the agent: leave no Job behind.
		bg := context.WithoutCancel(ctx)
		_, _ = git(bg, top, "worktree", "remove", "--force", worktree)
		_, _ = git(bg, top, "branch", "-D", j.Branch)
		_ = os.RemoveAll(p.store.Dir(j.ID))
		return cli.JobView{}, err
	}
	return view(launched, ""), nil
}

func (l *localJobs) List(ctx context.Context) ([]cli.JobView, error) {
	p, err := l.open()
	if err != nil {
		return nil, err
	}
	stored, err := p.store.List()
	if err != nil {
		return nil, err
	}
	views := make([]cli.JobView, 0, len(stored))
	for _, s := range stored {
		j, err := p.runner.Reconcile(ctx, s.ID)
		if err != nil {
			views = append(views, view(s, err.Error()))
			continue
		}
		views = append(views, view(j, ""))
	}
	return views, nil
}

func (l *localJobs) Show(ctx context.Context, id string) (cli.JobView, error) {
	j, _, err := l.read(ctx, id)
	return view(j, ""), err
}

func (l *localJobs) Stop(ctx context.Context, id string) (cli.JobView, error) {
	j, p, err := l.read(ctx, id)
	if err != nil {
		return cli.JobView{}, err
	}
	if err := p.runner.Stop(ctx, j.ID); err != nil {
		return cli.JobView{}, mapErr(err)
	}
	return l.Show(ctx, id)
}

func (l *localJobs) Resume(ctx context.Context, id string) (cli.JobView, error) {
	j, p, err := l.read(ctx, id)
	if err != nil {
		return cli.JobView{}, err
	}
	if err := p.tmux.Check(ctx); err != nil {
		return cli.JobView{}, err
	}
	launched, err := p.runner.Launch(ctx, j.ID, "")
	if err != nil {
		return cli.JobView{}, mapErr(err)
	}
	return view(launched, ""), nil
}

func (l *localJobs) Screen(ctx context.Context, id string) (string, error) {
	j, p, err := l.read(ctx, id)
	if err != nil {
		return "", err
	}
	if err := hasTerminal(j, domain.CodeRunNotAttachable); err != nil {
		return "", err
	}
	screen, err := p.runner.Screen(ctx, j.ID)
	return string(screen), mapErr(err)
}

func (l *localJobs) Send(ctx context.Context, id, text string) (cli.JobView, error) {
	j, p, err := l.read(ctx, id)
	if err != nil {
		return cli.JobView{}, err
	}
	if err := hasTerminal(j, domain.CodeRunNotRespondable); err != nil {
		return cli.JobView{}, err
	}
	if err := p.runner.Send(ctx, j.ID, text); err != nil {
		return cli.JobView{}, mapErr(err)
	}
	return l.Show(ctx, id)
}

func (l *localJobs) Attach(ctx context.Context, id string, readOnly bool) error {
	j, p, err := l.read(ctx, id)
	if err != nil {
		return err
	}
	if err := hasTerminal(j, domain.CodeRunNotAttachable); err != nil {
		return err
	}
	if err := p.tmux.Check(ctx); err != nil {
		return err
	}
	attach, err := p.tmux.AttachCommand(p.tmux.HandleFor(j.Launch.Session), readOnly)
	if err != nil {
		return err
	}
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		return runAttached(ctx, attach)
	}
	launcher, ok := attachwindow.Detect(os.Getenv, runtime.GOOS)
	if !ok {
		return domain.NewError(domain.CodeRunNotAttachable, "there is no terminal here to attach in; run `cflk attach` from a terminal", nil)
	}
	if err := launcher.Open(ctx, attachwindow.ExecRunner{}, windowArgv(attach), "cflk "+j.ID); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "opened a %s window attached to %s\n", launcher.Label(), j.ID)
	return nil
}

// read is the Job as it is now: reconciled under its lock.
func (l *localJobs) read(ctx context.Context, id string) (job.Job, *peer, error) {
	p, err := l.open()
	if err != nil {
		return job.Job{}, nil, err
	}
	if !job.ValidID(id) {
		return job.Job{}, nil, invalid("%q is not a Job id (job-xxxxxxxxxx)", id)
	}
	j, err := p.runner.Reconcile(ctx, id)
	return j, p, mapErr(err)
}

func hasTerminal(j job.Job, code domain.Code) error {
	if j.Launch == nil || (j.State != job.Running && j.State != job.Waiting) {
		return domain.NewError(code, fmt.Sprintf("job %s is %s: its agent is not running", j.ID, j.State), nil)
	}
	return nil
}

func view(j job.Job, problem string) cli.JobView {
	v := cli.JobView{ID: j.ID, Peer: j.Peer, Repo: j.Repo, Branch: j.Branch, Harness: j.Harness, State: string(j.State), Gen: j.Gen, SessionID: j.SessionID, Problem: problem}
	if j.Launch != nil {
		v.Terminal = j.Launch.Session
	}
	return v
}

func invalid(format string, args ...any) error {
	return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf(format, args...), nil)
}

func mapErr(err error) error {
	if errors.Is(err, job.ErrNotFound) {
		return domain.NewError(domain.CodeNotFound, err.Error(), nil)
	}
	if errors.Is(err, job.ErrInvalidTransition) {
		return domain.NewError(domain.CodeInvalidTransition, err.Error(), nil)
	}
	return err
}

// git runs git with a fixed argv in dir (the current directory if empty) and returns its trimmed stdout.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // a fixed git argv; the variable parts are single arguments, never a shell
	cmd.Dir = dir
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errOut.String()); msg != "" {
			return "", fmt.Errorf("git %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(out.String()), nil
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// runAttached runs the tmux client on this terminal until the person leaves it.
func runAttached(ctx context.Context, c cfruntime.Command) error {
	// The person leaves with the tmux detach key; an interrupt is theirs to type in the pane.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), c.Path, c.Args...) //nolint:gosec // the argv is the tmux client of the Job's session, built by the tmux runtime
	cmd.Env = c.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// windowArgv is the attach command for a window that starts from the environment
// of another terminal: if that one is inside tmux its variables are dropped, so the
// client does not nest, and the tmux directory of this process is kept.
func windowArgv(c cfruntime.Command) []string {
	argv := []string{"/usr/bin/env", "-u", "TMUX", "-u", "TMUX_PANE"}
	if dir := os.Getenv("TMUX_TMPDIR"); dir != "" {
		argv = append(argv, "TMUX_TMPDIR="+dir)
	}
	return append(append(argv, c.Path), c.Args...)
}

// runWrapper is `cflk internal wrapper`: execwrap on a launch directory, in the
// terminal of the launch.
func runWrapper(dir string) error {
	return runner.RunWrapper(dir)
}
