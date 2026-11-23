package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/support/atomicfile"
)

// ClaudeSettingsFile is the settings of a launch, in its directory, that Claude Code
// reads on top of the user's through --settings: the hooks of the Job live there and
// nowhere else.
const ClaudeSettingsFile = "claude-settings.json"

type claudeAgent struct{ host harness.Host }

func (a claudeAgent) Prepare(ctx context.Context, in Input) (Plan, error) {
	id := in.SessionID
	if id == "" {
		var err error
		if id, err = NewSessionID(claude.ID); err != nil {
			return Plan{}, err
		}
	}
	adapter := claude.Adapter{}
	if err := adapter.TrustFolder(a.host, in.Worktree); err != nil {
		return Plan{}, err
	}
	settings := filepath.Join(in.LaunchDir, ClaudeSettingsFile)
	if err := writeClaudeSettings(settings, in.Waiting); err != nil {
		return Plan{}, err
	}
	run := interactive(in)
	configDir := claudeConfigDir(a.host)
	run.ConfigDir = configDir
	l, err := adapter.StartSession(ctx, a.host, run, id)
	if err != nil {
		return Plan{}, err
	}
	env := claudeEnv(a.host)
	plan := Plan{Argv: withFlags(l.Argv, "--settings", settings), Env: harnessEnv(a.host, env...), SessionID: id}
	if len(env) == 0 { // the caller has none: neither may the harness inherit one
		plan.UnsetEnv = []string{claude.ConfigHomeEnv}
	}
	return plan, nil
}

// claudeEnv is CLAUDE_CONFIG_DIR as the caller has it, if it has it: set, even to
// ~/.claude, Claude Code looks for its global state inside it and not in
// ~/.claude.json, so a caller without it must not be given one.
func claudeEnv(host harness.Host) []string {
	if dir := host.Getenv(claude.ConfigHomeEnv); dir != "" {
		return []string{claude.ConfigHomeEnv + "=" + dir}
	}
	return nil
}

// claudeConfigDir is where Claude Code keeps its sessions for this host:
// CLAUDE_CONFIG_DIR, or ~/.claude. Planning looks there, and the harness is told
// to use it, so both always agree.
func claudeConfigDir(host harness.Host) string {
	if env := host.Getenv(claude.ConfigHomeEnv); env != "" {
		return env
	}
	return filepath.Join(host.Home(), claude.ConfigDirName)
}

// Discover is never needed: the session of a Claude launch is named at the start.
func (claudeAgent) Discover(context.Context, Input, time.Time, time.Time) ([]string, error) {
	return nil, nil
}

// writeClaudeSettings installs the hooks that say that the agent waits: Stop, the
// end of a turn, writes the marker, and UserPromptSubmit, which also fires for
// text that arrives through tmux, removes it.
func writeClaudeSettings(path, marker string) error {
	hook := func(command string) []map[string]any {
		return []map[string]any{{"hooks": []map[string]any{{"type": "command", "command": command}}}}
	}
	raw, err := json.MarshalIndent(map[string]any{
		// The launch runs in bypassPermissions mode and nobody is there to accept its
		// disclaimer; this setting is what newer versions read instead of .claude.json.
		"skipDangerousModePermissionPrompt": true,
		"hooks": map[string]any{
			"Stop":             hook("touch -- " + shellQuote(marker)),
			"UserPromptSubmit": hook("rm -f -- " + shellQuote(marker)),
		},
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("runner: encoding the hooks: %w", err)
	}
	return atomicfile.Write(path, raw, 0o600)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
