package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// loginShellTimeout bounds the shell asked for its PATH: a profile that hangs
// must not hold the request that triggered it, and it stays well under the
// budget the client gives a peer to answer.
const loginShellTimeout = time.Second

// loginPathTTL is how long the PATH of the login shell is reused: each `cflk
// peer` is a new process, and a profile can take seconds to run.
const loginPathTTL = time.Hour

// failedLoginPathTTL is how long a probe that failed or timed out is
// remembered: long enough that a broken profile does not cost its timeout on
// every request, short enough that a one-off failure does not take the tools
// away for an hour.
const failedLoginPathTTL = time.Minute

// pipeGrace is how long after the shell is killed its output is still waited
// for: a descendant that kept the pipe must not hold the probe past its deadline.
const pipeGrace = 200 * time.Millisecond

// cacheDirName is the directory of cflk under the user's cache; it is the
// name of the state and configuration directories.
const cacheDirName = "codeflock"

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
	// The shell leads its own group so that the deadline kills what the profile
	// started too, and WaitDelay ends the wait for a pipe some descendant holds.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = pipeGrace
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

// cachedLoginPath is loginShellPath remembered in file, per shell: for
// loginPathTTL, or for failedLoginPathTTL when the probe got nothing.
func cachedLoginPath(ctx context.Context, shell, file string) string {
	if data, err := os.ReadFile(file); err == nil { //nolint:gosec // our own cache file
		if sh, path, ok := strings.Cut(string(data), "\n"); ok && sh == shell {
			ttl := loginPathTTL
			if path == "" {
				ttl = failedLoginPathTTL
			}
			if info, err := os.Stat(file); err == nil && time.Since(info.ModTime()) < ttl {
				return path
			}
		}
	}
	path := loginShellPath(ctx, shell)
	writeAtomically(file, []byte(shell+"\n"+path))
	return path
}

// writeAtomically replaces file with content in one rename, so that a `cflk
// peer` reading it at the same time never sees half of it. It is a cache: any
// failure is ignored.
func writeAtomically(file string, content []byte) {
	dir := filepath.Dir(file)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".login-path-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(content)
	if cerr := tmp.Close(); werr != nil || cerr != nil || os.Rename(tmp.Name(), file) != nil {
		_ = os.Remove(tmp.Name())
	}
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
		login = cachedLoginPath(ctx, os.Getenv("SHELL"), filepath.Join(cache, cacheDirName, "login-path"))
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
