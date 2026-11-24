package runner_test

import (
	"os"
	"testing"

	"github.com/Autarquico/codeflock/internal/peer/runner"
)

func TestScrubAgentSessionKeepsAuthAndConfig(t *testing.T) {
	drop := []string{"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID"}
	keep := []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "CODEX_API_KEY", "HTTPS_PROXY", "HOME", "PATH"}
	for _, n := range drop {
		t.Setenv(n, "x")
	}
	for _, n := range keep {
		t.Setenv(n, "x")
	}
	runner.ScrubAgentSession()
	for _, n := range drop {
		if _, ok := os.LookupEnv(n); ok {
			t.Errorf("%s survived", n)
		}
	}
	for _, n := range keep {
		if os.Getenv(n) != "x" {
			t.Errorf("%s was dropped", n)
		}
	}
}
