package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/support/atomicfile"
	"github.com/Autarquico/codeflock/internal/support/flock"
)

// StartSession builds the interactive process of a Job whose session the caller
// names: --session-id the first time, --resume once Claude Code has written it.
// Unlike Start it neither isolates the configuration nor asks for a secret: the
// process runs with the login and the configuration of the host, so the Launch
// has no Env and no SecretEnv. run.ConfigDir is where the host keeps its sessions
// (~/.claude) and the prompt may be empty (a resume).
func (a Adapter) StartSession(_ context.Context, host harness.Host, run harness.RunSpec, id string) (harness.Launch, error) {
	if err := a.validate(run); err != nil {
		return harness.Launch{}, err
	}
	plan, err := planFor(run, id)
	if err != nil {
		return harness.Launch{}, err
	}
	l, err := a.launch(host, run, plan)
	if err != nil {
		return harness.Launch{}, err
	}
	l.Env, l.SecretEnv = nil, nil
	return l, nil
}

// TrustFolder marks the worktree as trusted in ~/.claude.json (or in the .claude.json
// of CLAUDE_CONFIG_DIR when the host sets it), so that the first
// start there does not stop at "Is this a project you trust?". Only that project's
// entry is set; the rest of the file is kept. Claude Code keys the trust by the
// real path of the folder.
func (Adapter) TrustFolder(host harness.Host, worktree string) error {
	return editProject(host, worktree, func(projects object, key string) bool {
		entry, _ := projects[key].(object)
		if entry == nil {
			entry = object{}
		}
		entry[trustField] = true
		projects[key] = entry
		return true
	})
}

// UntrustFolder removes the trust entry TrustFolder wrote for the worktree, and
// only that project's entry. A folder with no entry is left as it is.
func (Adapter) UntrustFolder(host harness.Host, worktree string) error {
	return editProject(host, worktree, func(projects object, key string) bool {
		_, present := projects[key]
		delete(projects, key)
		return present
	})
}

// stateFilePath is where Claude Code keeps its global state for this host: inside
// CLAUDE_CONFIG_DIR when the host sets it, ~/.claude.json otherwise.
func stateFilePath(host harness.Host) string {
	if dir := host.Getenv(ConfigHomeEnv); dir != "" {
		return filepath.Join(dir, StateFile)
	}
	return filepath.Join(host.Home(), StateFile)
}

// stateLockPath is the lock every cflk write of the state file holds, whichever
// Job, state root or HOME it works for: it sits next to the file, so whoever
// shares the file shares the lock.
func stateLockPath(host harness.Host) string {
	return stateFilePath(host) + ".cflk.lock"
}

// editProject applies change to the projects of ~/.claude.json under the key of the
// worktree, keeping the rest of the file, and writes it back atomically if change
// says it changed something. The read-modify-write is serialized with the other
// cflk processes by a lock and reads the file after taking it. Claude Code itself
// writes the file without that lock: a change of its own landing between our read
// and our rename can still be lost (residual, narrow window).
func editProject(host harness.Host, worktree string, change func(projects object, key string) bool) error {
	resolved, err := trustedPath(worktree)
	if err != nil {
		return err
	}
	lock := stateLockPath(host)
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return fmt.Errorf("claude-code: trusting the folder: %w", err)
	}
	release, err := flock.Lock(lock)
	if err != nil {
		return fmt.Errorf("claude-code: trusting the folder: %w", err)
	}
	defer release()
	path := stateFilePath(host)
	state := object{}
	raw, err := os.ReadFile(path) //nolint:gosec // the state file of the host
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("claude-code: trusting the folder: %w", err)
	default:
		if state, err = parseObject(raw); err != nil {
			return fmt.Errorf("claude-code: trusting the folder: %s: %w", path, err)
		}
	}
	projects, _ := state[projectsField].(object)
	if projects == nil {
		projects = object{}
	}
	if !change(projects, resolved) {
		return nil
	}
	state[projectsField] = projects
	out, err := encodeObject(state)
	if err != nil {
		return fmt.Errorf("claude-code: trusting the folder: %w", err)
	}
	if err := atomicfile.Write(path, out, 0o600); err != nil {
		return fmt.Errorf("claude-code: trusting the folder: %w", err)
	}
	return nil
}
