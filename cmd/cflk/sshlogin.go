package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/domain"
)

// loginProblem says why a harness (by name or id) cannot authenticate in this
// process, or "".
//
// On macOS Claude Code keeps its login in the keychain, which a process started
// from an ssh session cannot read: a Job a peer starts here would open "Not
// logged in". CLAUDE_CODE_OAUTH_TOKEN (from `claude setup-token`) in an
// environment ssh sessions read works, as do ANTHROPIC_API_KEY and a
// .credentials.json in ~/.claude or CLAUDE_CONFIG_DIR. `claude auth login` run
// over ssh is not enough: it still writes to the keychain when it can.
func loginProblem(harness string) string {
	if harnessIDs[harness] != claude.ID || !overSSH() || runtime.GOOS != "darwin" ||
		os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" || os.Getenv("ANTHROPIC_API_KEY") != "" {
		return ""
	}
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir != "" && !filepath.IsAbs(dir) {
		return "claude: CLAUDE_CONFIG_DIR is relative, and a Job starts in its worktree, not where cflk runs: make it absolute"
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".claude")
	}
	if _, err := os.Stat(filepath.Join(dir, ".credentials.json")); err == nil { //nolint:gosec // the user's own Claude config directory
		return ""
	}
	return "claude: its login is in the macOS keychain, which ssh sessions cannot read; on this Mac, export CLAUDE_CODE_OAUTH_TOKEN (from `claude setup-token`) or ANTHROPIC_API_KEY in ~/.zshenv"
}

func overSSH() bool { return os.Getenv("SSH_CONNECTION") != "" }

// refuseWithoutLogin is the error for starting a harness here that cannot log in.
func refuseWithoutLogin(harness string) error {
	if problem := loginProblem(harness); problem != "" {
		return domain.NewError(domain.CodeHarnessUnavailable, fmt.Sprintf("peer %s: %s", hostname(), problem), nil)
	}
	return nil
}

func hostname() string {
	name, _ := os.Hostname()
	return name
}
