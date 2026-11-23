package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/support/atomicfile"
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

// TrustFolder marks the worktree as trusted in ~/.claude.json, so that the first
// start there does not stop at "Is this a project you trust?". Only that project's
// entry is set; the rest of the file is kept. Claude Code keys the trust by the
// real path of the folder.
func (Adapter) TrustFolder(host harness.Host, worktree string) error {
	resolved, err := trustedPath(worktree)
	if err != nil {
		return err
	}
	path := filepath.Join(host.Home(), StateFile)
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
	entry, _ := projects[resolved].(object)
	if entry == nil {
		entry = object{}
	}
	entry[trustField] = true
	projects[resolved] = entry
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
