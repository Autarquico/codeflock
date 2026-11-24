package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Autarquico/codeflock/internal/domain"
)

// loginProblem says why a harness cannot authenticate in this process, or "".
//
// On macOS Claude Code keeps its login in the keychain, which a process started
// from an ssh session cannot read: a Job a peer starts here would open "Not
// logged in". `claude auth login` run over ssh stores the login in
// ~/.claude/.credentials.json instead, and CLAUDE_CODE_OAUTH_TOKEN also works.
func loginProblem(harness string) string {
	if harness != "claude" || !overSSH() || runtime.GOOS != "darwin" || os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json")); err == nil {
		return ""
	}
	return "claude: its login is in the macOS keychain, which ssh sessions cannot read; run `claude auth login` once over ssh on this Mac"
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
