package runner

import (
	"context"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/job"
)

// Input is what an Agent is told to build the process of a launch.
type Input struct {
	Job      job.Job
	Worktree string
	// LaunchDir holds the records of the launch.
	LaunchDir string
	// Waiting is the marker the agent's hooks write and remove.
	Waiting string
	// SessionFile is where the launch writes the Job's session id for the notify program.
	SessionFile string
	// NotifyCmd is the program a harness without hooks runs when a turn ends.
	NotifyCmd []string
	// SessionID is the session to resume; empty on the first launch.
	SessionID string
	Prompt    string
}

// Plan is the process an Agent built.
type Plan struct {
	Argv []string
	Env  []string
	// UnsetEnv are variables the harness must not inherit from the tmux server.
	UnsetEnv []string
	// SessionID is the session the process runs; empty if the harness picks its
	// own, which Discover then finds.
	SessionID string
}

// Agent is what the launch needs to know of one harness.
type Agent interface {
	// Prepare readies what must exist before the harness starts (trust of the folder,
	// the hooks of the launch) and says what to run.
	Prepare(ctx context.Context, in Input) (Plan, error)
	// Discover lists the sessions the launch created, for a Plan without one: those
	// created from after until before (the end of the launch, or now).
	Discover(ctx context.Context, in Input, after, before time.Time) ([]string, error)
}

// staleWaiting is implemented by an Agent whose waiting marker the activity of the
// agent supersedes (Codex: its rollout grows after the marker).
type staleWaiting interface {
	WaitingStale(sessionID string, marker time.Time) bool
}

func interactive(in Input) harness.RunSpec {
	return harness.RunSpec{Mode: domain.ModeInteractive, Prompt: in.Prompt, Worktree: in.Worktree}
}

// withFlags puts flags right after the binary, before any subcommand or prompt.
func withFlags(argv []string, flags ...string) []string {
	out := make([]string, 0, len(argv)+len(flags))
	out = append(out, argv[0])
	out = append(out, flags...)
	return append(out, argv[1:]...)
}

// harnessEnv is the environment a launch gives its harness explicitly: HOME, PATH
// and the extra variables (the configuration directory discovery and planning look
// at). Without it the harness would inherit the tmux server's environment, which
// is that of whoever started the server, not of the caller.
func harnessEnv(host harness.Host, extra ...string) []string {
	env := append([]string{"HOME=" + host.Home()}, extra...)
	if path := host.Getenv("PATH"); path != "" {
		env = append(env, "PATH="+path)
	}
	return env
}
