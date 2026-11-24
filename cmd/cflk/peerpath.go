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
// must not hold the request that triggered it, and it stays well under the
// budget the client gives a peer to answer.
const loginShellTimeout = time.Second

// loginPathTTL is how long the PATH of the login shell is reused: each `cflk
// peer` is a new process, and a profile can take seconds to run.
const loginPathTTL = time.Hour

// toolDirs are where the tools of an agent usually live when no profile put them
// in the PATH (relative ones are under $HOME).
var toolDirs = []string{
	".local/bin", "/opt/homebrew/bin", "/usr/local/bin",
	".local/share/mise/shims", ".local/share/mise/command-wrappers/bin",
}

// peerPath is the PATH of a peer: the one of the login shell of the user first,
// so that mise or Homebrew win over the system binaries, then the usual
// directories, then the one it has, without repeats. A non-interactive ssh gets
// none of what the profile adds (mise, Homebrew, ~/.local/bin), and tmux, git,
// claude and codex are looked up with it.
func peerPath(login, current, home string) string {
	parts := filepath.SplitList(login)
	for _, dir := range toolDirs {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(home, dir)
		}
		parts = append(parts, dir)
	}
	parts = append(parts, filepath.SplitList(current)...)
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

const (
	pathBegin = "__CFLK_PATH__"
	pathEnd   = "__CFLK_END__"
)

// loginShellPath is the PATH that `$SHELL -lc` has, or empty if it cannot be had
// in time. What the profile prints around it is ignored: the value is the text
// between two markers.
func loginShellPath(ctx context.Context, shell string) string {
	if !filepath.IsAbs(shell) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, loginShellTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-lc", `printf '\n`+pathBegin+`%s`+pathEnd+`\n' "$PATH"`) //nolint:gosec // the user's own login shell, a fixed script
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	_, rest, ok := strings.Cut(string(out), "\n"+pathBegin)
	if !ok {
		return ""
	}
	value, _, ok := strings.Cut(rest, pathEnd)
	if !ok {
		return ""
	}
	return value
}

// cachedLoginPath is loginShellPath remembered in file for loginPathTTL, per
// shell. A failed probe is remembered too, so a broken profile costs its
// timeout once an hour and not on every request.
func cachedLoginPath(ctx context.Context, shell, file string) string {
	if info, err := os.Stat(file); err == nil && time.Since(info.ModTime()) < loginPathTTL {
		if data, err := os.ReadFile(file); err == nil { //nolint:gosec // our own cache file
			if sh, path, ok := strings.Cut(string(data), "\n"); ok && sh == shell {
				return path
			}
		}
	}
	path := loginShellPath(ctx, shell)
	if os.MkdirAll(filepath.Dir(file), 0o700) == nil {
		_ = os.WriteFile(file, []byte(shell+"\n"+path), 0o600) //nolint:gosec // our own cache file
	}
	return path
}

// useLoginPath makes the PATH of this process the one of peerPath, so that what
// it looks up and what it launches (tmux, git, the harnesses) is found as in the
// user's terminal.
func useLoginPath(ctx context.Context) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	login := ""
	if cache, err := os.UserCacheDir(); err == nil {
		login = cachedLoginPath(ctx, os.Getenv("SHELL"), filepath.Join(cache, "codefabric", "login-path"))
	}
	_ = os.Setenv("PATH", peerPath(login, os.Getenv("PATH"), home))
}

// useToolDirs is useLoginPath without the login shell, for the wrapper of a
// launch: it starts in a tmux server whose PATH may be older than this one.
func useToolDirs() {
	if home, err := os.UserHomeDir(); err == nil {
		_ = os.Setenv("PATH", peerPath("", os.Getenv("PATH"), home))
	}
}
