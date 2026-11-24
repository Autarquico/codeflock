package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

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
	name string
	// mirrors holds the bare repositories other peers push the code of their
	// delegations to: <share>/repos/<sha-repo>.git.
	mirrors string
	store   *job.Store
	tmux    *tmux.Runtime
	runner  *runner.Runner
}

// overrides are what a test of this package swaps in the composition: the
// agents that stand for the harnesses, and the program a launch runs as its
// wrapper. They are not flags of the binary.
type overrides struct {
	// name, root and share place a peer apart from the machine's own: a test
	// that runs two peers on one machine gives each its own.
	name, root, share string
	agents            map[string]runner.Agent
	wrapper           func(launchDir string) []string
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
	root := cmp.Or(over.root, os.Getenv(stateDirVar))
	if root == "" {
		if root, err = job.DefaultRoot(); err != nil {
			return nil, err
		}
	}
	share, err := shareDir(home, over.share)
	if err != nil {
		return nil, err
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
		name:    cmp.Or(over.name, config.MachineName(hostname, "this machine")),
		mirrors: filepath.Join(share, "repos"),
		store:   store,
		tmux:    rt,
		runner: runner.New(runner.Config{
			Store: store, Host: host, Tmux: rt,
			Wrapper:   wrapper,
			NotifyCmd: []string{exe, "internal", "notify"},
			Agents:    over.agents,
		}),
	}, nil
}

// shareDirVar overrides where this peer keeps the mirrors (~/.local/share/cflk).
const shareDirVar = "CFLK_SHARE_DIR"

func shareDir(home, override string) (string, error) {
	dir := cmp.Or(override, os.Getenv(shareDirVar), filepath.Join(home, ".local", "share", "cflk"))
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s must be an absolute path, got %q", shareDirVar, dir)
	}
	return dir, nil
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
		return cli.JobView{}, invalid("--on %q: this runs Jobs on this peer (%s); other peers go through the fleet", req.On, p.name)
	}
	if err := p.tmux.Check(ctx); err != nil {
		return cli.JobView{}, err
	}
	src, err := sourceOf(ctx, req.Branch)
	if err != nil {
		return cli.JobView{}, err
	}
	return p.start(ctx, startSpec{repoDir: src.dir, repo: src.repo, harness: harnessID, commit: src.commit, prompt: req.Prompt, createdBy: p.name})
}

// source is the code a Job starts from: a commit of the repository the person is in.
type source struct{ dir, repo, commit string }

// sourceOf resolves base (a branch or commit; empty is HEAD) in the repository of the current directory.
func sourceOf(ctx context.Context, base string) (source, error) {
	top, err := git(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return source{}, invalid("cflk run starts from a git repository: %v", err)
	}
	origin, err := git(ctx, top, "remote", "get-url", "origin")
	if err != nil {
		return source{}, invalid("the repository has no origin remote: %v", err)
	}
	repo, err := job.NormalizeRepo(origin)
	if err != nil {
		return source{}, invalid("origin %q: %v", origin, err)
	}
	if base == "" {
		base = "HEAD"
	}
	commit, err := git(ctx, top, "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
	if err != nil {
		return source{}, invalid("%q is not a commit of this repository: %v", base, err)
	}
	return source{dir: top, repo: repo, commit: commit}, nil
}

// startSpec is a Job to create and launch from a commit of a repository on this
// machine: the one the person is in, or the mirror a delegation was pushed to.
type startSpec struct {
	repoDir, repo, harness, commit, prompt, createdBy string
	// id is the id another peer drew; empty draws one.
	id string
}

// start creates the Job, its worktree at the commit and its first launch. When
// nothing was started it leaves nothing behind.
func (p *peer) start(ctx context.Context, sp startSpec) (cli.JobView, error) {
	top := sp.repoDir
	proto := job.Job{
		Repo: sp.repo, Harness: sp.harness, Gen: 1, Peer: p.name, State: job.Stopped, CreatedBy: sp.createdBy,
		History: []job.Entry{{Gen: 1, Peer: p.name, Commit: sp.commit}},
	}
	create := p.store.Create
	if sp.id != "" {
		create = func(j job.Job) (job.Job, error) { return p.store.CreateAs(sp.id, j) }
	}
	j, err := create(proto)
	if errors.Is(err, job.ErrExists) {
		return cli.JobView{}, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("job %s already exists on %s", sp.id, p.name), nil)
	}
	if err != nil {
		return cli.JobView{}, err
	}
	if err := p.addWorktree(ctx, top, j, sp.commit); err != nil {
		_ = os.RemoveAll(p.store.Dir(j.ID))
		_, _ = git(context.WithoutCancel(ctx), top, "worktree", "prune")
		return cli.JobView{}, err
	}
	launched, err := p.runner.Launch(ctx, j.ID, sp.prompt)
	if err != nil {
		// A launch is recorded before anything starts, so a Job that is stopped
		// with none recorded ran nothing and was told nothing. That is decided
		// and acted on under the Job's lock: a `resume` that got in between has
		// recorded a launch, and its Job is not ours to delete.
		removed := false
		_ = p.store.WithLock(j.ID, func(left *job.Job) error {
			if left.State == job.Stopped && left.Launch == nil {
				removed = p.discard(ctx, top, *left)
			}
			return nil
		})
		if !removed {
			// The launch is recorded: its agent may still run, and the Job and
			// its worktree are what `cflk stop` needs to end it.
			return cli.JobView{}, fmt.Errorf("%w (job %s is kept)", err, j.ID)
		}
		return cli.JobView{}, err
	}
	return view(launched, ""), nil
}

// addWorktree creates the worktree of j at commit on its new branch. A failure
// (a post-checkout hook, a full disk) can leave the branch and git's registration
// of the worktree behind: both are taken back, the branch only if it did not exist
// before and is at the commit, which is where `worktree add -b` put it.
func (p *peer) addWorktree(ctx context.Context, top string, j job.Job, commit string) error {
	worktree := p.store.WorktreeDir(j.ID, j.Gen)
	// Only a lookup that answered "no such branch" lets cleanup delete it; a
	// lookup that failed, or found it, leaves the branch alone.
	found, lookErr := git(context.WithoutCancel(ctx), top, "for-each-ref", "--format=%(refname)", "refs/heads/"+j.Branch)
	existed := lookErr != nil || found != ""
	_, err := git(ctx, top, "worktree", "add", "-b", j.Branch, worktree, commit)
	if err == nil {
		return nil
	}
	bg := context.WithoutCancel(ctx)
	_, _ = git(bg, top, "worktree", "remove", "--force", worktree)
	_ = os.RemoveAll(worktree)
	_, _ = git(bg, top, "worktree", "prune")
	if tip, tipErr := git(bg, top, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/heads/"+j.Branch); !existed && tipErr == nil && tip == commit {
		_, _ = git(bg, top, "branch", "-D", "--", j.Branch)
	}
	return err
}

// discard takes back a Job that launched nothing: its worktree and registration,
// its branch and its directory. It reports whether the directory is gone. The
// caller holds the Job's lock.
func (p *peer) discard(ctx context.Context, top string, j job.Job) bool {
	bg := context.WithoutCancel(ctx)
	_, _ = git(bg, top, "worktree", "remove", "--force", p.store.WorktreeDir(j.ID, j.Gen))
	_, _ = git(bg, top, "branch", "-D", "--", j.Branch)
	removed := os.RemoveAll(p.store.Dir(j.ID)) == nil
	_, _ = git(bg, top, "worktree", "prune")
	return removed
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

// Stored is the Jobs as their records say, without observing their agents or
// waiting for any lock: what a listing shows when a command has a Job busy.
// problem is shown with each.
func (l *localJobs) Stored(problem string) ([]cli.JobView, error) {
	p, err := l.open()
	if err != nil {
		return nil, err
	}
	stored, err := p.store.List()
	if err != nil {
		return nil, err
	}
	views := make([]cli.JobView, len(stored))
	for i, s := range stored {
		views[i] = view(s, problem)
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
	j, attach, err := l.attachCommand(ctx, id, readOnly)
	if err != nil {
		return err
	}
	return enter(ctx, attachwindow.ExecRunner{}, attach, j.ID)
}

// enter runs the command that enters a terminal: here when this one is a
// terminal, else in a new window of the one the person works in.
func enter(ctx context.Context, windows attachwindow.Runner, attach cfruntime.Command, id string) error {
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		return runAttached(ctx, attach)
	}
	launcher, ok := attachwindow.Detect(os.Getenv, runtime.GOOS)
	if !ok {
		return domain.NewError(domain.CodeRunNotAttachable, "there is no terminal here to attach in; run `cflk attach` from a terminal", nil)
	}
	if err := launcher.Open(ctx, windows, windowArgv(attach), "cflk "+id); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "opened a %s window attached to %s\n", launcher.Label(), id)
	return nil
}

// AttachHere is Attach for a process whose terminal is the person's, as under
// `ssh -t <peer> cflk peer attach`: it never opens a window.
func (l *localJobs) AttachHere(ctx context.Context, id string, readOnly bool) error {
	_, attach, err := l.attachCommand(ctx, id, readOnly)
	if err != nil {
		return err
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return domain.NewError(domain.CodeRunNotAttachable, "attach needs a terminal (ssh -t)", nil)
	}
	return runAttached(ctx, attach)
}

// attachCommand is the tmux client that enters the terminal of a running Job.
func (l *localJobs) attachCommand(ctx context.Context, id string, readOnly bool) (job.Job, cfruntime.Command, error) {
	j, p, err := l.read(ctx, id)
	if err != nil {
		return j, cfruntime.Command{}, err
	}
	if err := hasTerminal(j, domain.CodeRunNotAttachable); err != nil {
		return j, cfruntime.Command{}, err
	}
	if err := p.tmux.Check(ctx); err != nil {
		return j, cfruntime.Command{}, err
	}
	attach, err := p.tmux.AttachCommand(p.tmux.HandleFor(j.Launch.Session), readOnly)
	return j, attach, err
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
	if j.Launch == nil || !runsAgent(j.State) {
		return notRunning(j.ID, j.State, code)
	}
	return nil
}

// runsAgent says whether a Job in this state has an agent running in a terminal.
func runsAgent(state job.State) bool { return state == job.Running || state == job.Waiting }

func notRunning(id string, state job.State, code domain.Code) error {
	return domain.NewError(code, fmt.Sprintf("job %s is %s: its agent is not running", id, state), nil)
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

// isTerminal says whether f is a terminal: it answers the window-size request
// only a terminal does, which /dev/null, a character device, does not.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ) //nolint:gosec // a descriptor fits an int
	return err == nil
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
	useToolDirs()
	return runner.RunWrapper(dir)
}
