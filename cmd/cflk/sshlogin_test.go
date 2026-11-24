package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestClaudeStartedOverSSHOnAMacNeedsALoginOutsideTheKeychain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("SSH_CONNECTION", "")
	if p := loginProblem("claude"); p != "" {
		t.Fatalf("outside ssh: %q", p)
	}
	t.Setenv("SSH_CONNECTION", "10.0.0.1 1 10.0.0.2 22")
	if p := loginProblem("codex"); p != "" {
		t.Fatalf("codex: %q", p)
	}
	if got := loginProblem("claude") != ""; got != (runtime.GOOS == "darwin") {
		t.Fatalf("over ssh on %s, problem = %v", runtime.GOOS, got)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "x")
	if p := loginProblem("claude"); p != "" {
		t.Fatalf("with a token: %q", p)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "k")
	if p := loginProblem("claude"); p != "" {
		t.Fatalf("with an API key: %q", p)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	custom := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", custom)
	if err := os.WriteFile(filepath.Join(custom, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := loginProblem("claude"); p != "" {
		t.Fatalf("with a credentials file in CLAUDE_CONFIG_DIR: %q", p)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := loginProblem("claude"); p != "" {
		t.Fatalf("with a credentials file: %q", p)
	}
}
