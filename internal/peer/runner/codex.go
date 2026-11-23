package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
	"unicode"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
)

// codexStaleAfter is how long after the marker the rollout may still be written
// to by the turn that wrote it; growth beyond it is the agent working again.
const codexStaleAfter = 2 * time.Second

type codexAgent struct{ host harness.Host }

// Prepare trusts the worktree and installs the notify program with -c flags, so that
// neither touches config.toml: they live and die with this process.
func (a codexAgent) Prepare(ctx context.Context, in Input) (Plan, error) {
	if len(in.NotifyCmd) == 0 {
		return Plan{}, errors.New("runner: codex needs a notify program to tell when it waits")
	}
	trusted, err := filepath.EvalSymlinks(in.Worktree)
	if err != nil {
		return Plan{}, fmt.Errorf("runner: resolving the worktree: %w", err)
	}
	notify, err := tomlArray(append(append([]string{}, in.NotifyCmd...), in.Waiting, in.SessionFile))
	if err != nil {
		return Plan{}, err
	}
	trust, err := tomlString(trusted)
	if err != nil {
		return Plan{}, err
	}
	run := interactive(in)
	configDir := codex.ConfigDir(a.host)
	run.ConfigDir = configDir
	l, err := codex.Adapter{}.StartSession(ctx, a.host, run, in.SessionID)
	if err != nil {
		return Plan{}, err
	}
	argv := withFlags(l.Argv,
		"-c", "projects."+trust+`.trust_level="trusted"`,
		"-c", "notify="+notify,
	)
	return Plan{Argv: argv, Env: harnessEnv(a.host, codex.ConfigHomeEnv+"="+configDir), SessionID: in.SessionID}, nil
}

func (a codexAgent) Discover(ctx context.Context, in Input, after time.Time) ([]string, error) {
	return codex.DiscoverSessions(ctx, codex.ConfigDir(a.host), in.Worktree, after)
}

// WaitingStale: the notification comes at the end of a turn, so a rollout that
// grew after it is a turn that went on.
func (a codexAgent) WaitingStale(sessionID string, marker time.Time) bool {
	path, err := codex.Rollout(codex.ConfigDir(a.host), sessionID)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.ModTime().After(marker.Add(codexStaleAfter))
}

// tomlString is s as a TOML basic string.
func tomlString(s string) (string, error) {
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("runner: %q has a control character", s)
		}
	}
	return strconv.Quote(s), nil
}

func tomlArray(items []string) (string, error) {
	out := "["
	for i, s := range items {
		q, err := tomlString(s)
		if err != nil {
			return "", err
		}
		if i > 0 {
			out += ","
		}
		out += q
	}
	return out + "]", nil
}
