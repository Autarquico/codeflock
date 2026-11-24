package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// loginShellTimeout bounds the shell asked for its PATH: a profile that hangs
// must not hold the request that triggered it.
const loginShellTimeout = 3 * time.Second

// toolDirs are where the tools of an agent usually live when no profile put them
// in the PATH (relative ones are under $HOME).
var toolDirs = []string{
	".local/bin", "/opt/homebrew/bin", "/usr/local/bin",
	".local/share/mise/shims", ".local/share/mise/command-wrappers/bin",
}

// peerPath is the PATH of a peer: the one it has, then the one of the login
// shell of the user, then the usual directories, without repeats. A non-interactive
// ssh gets none of what the profile adds (mise, Homebrew, ~/.local/bin), and
// tmux, git, claude and codex are looked up with it.
func peerPath(ctx context.Context, current, shell, home string) string {
	parts := filepath.SplitList(current)
	parts = append(parts, filepath.SplitList(loginShellPath(ctx, shell))...)
	for _, dir := range toolDirs {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(home, dir)
		}
		parts = append(parts, dir)
	}
	seen := make(map[string]bool, len(parts))
	out := parts[:0]
	for _, dir := range parts {
		if filepath.IsAbs(dir) && !seen[dir] {
			seen[dir] = true
			out = append(out, dir)
		}
	}
	return strings.Join(out, string(os.PathListSeparator))
}

// loginShellPath is the PATH that `$SHELL -lc` has, or empty if it cannot be had
// in time. Whatever the profile prints comes before the PATH, on lines of its own.
func loginShellPath(ctx context.Context, shell string) string {
	if !filepath.IsAbs(shell) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, loginShellTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-lc", `printf %s "$PATH"`) //nolint:gosec // the user's own login shell, a fixed script
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return lines[len(lines)-1]
}

// useLoginPath makes the PATH of this process the one of peerPath, so that what
// it looks up and what it launches (tmux, git, the harnesses) is found as in the
// user's terminal.
func useLoginPath(ctx context.Context) {
	if home, err := os.UserHomeDir(); err == nil {
		_ = os.Setenv("PATH", peerPath(ctx, os.Getenv("PATH"), os.Getenv("SHELL"), home))
	}
}

// useToolDirs is useLoginPath without the login shell, for the wrapper of a
// launch: it starts in a tmux server whose PATH may be older than this one.
func useToolDirs() {
	if home, err := os.UserHomeDir(); err == nil {
		_ = os.Setenv("PATH", peerPath(context.Background(), os.Getenv("PATH"), "", home))
	}
}
