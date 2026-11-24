package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fakeShell(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakesh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	return path
}

func TestThePathOfAPeerPutsTheLoginShellsFirstWithoutRepeats(t *testing.T) {
	got := peerPath("/login/bin:/usr/bin:/mise/bin:rel/bin:", "/usr/bin:/bin", "/home/u")
	want := "/login/bin /usr/bin /mise/bin /home/u/.local/bin /opt/homebrew/bin /usr/local/bin /home/u/.local/share/mise/shims /home/u/.local/share/mise/command-wrappers/bin /bin"
	if strings.Join(strings.Split(got, ":"), " ") != want {
		t.Errorf("PATH = %v\nwant   %v", got, want)
	}
}

func TestTheLoginShellsPathIsTheTextBetweenTheMarkers(t *testing.T) {
	shell := fakeShell(t, `printf "motd without newline"; printf '\n__CFLK_PATH__%s__CFLK_END__\n' "/login/bin:/x"; echo "logout noise"`)
	if got := loginShellPath(t.Context(), shell); got != "/login/bin:/x" {
		t.Errorf("got %q", got)
	}
	if got := loginShellPath(t.Context(), fakeShell(t, `echo /bad/bin`)); got != "" {
		t.Errorf("without markers got %q", got)
	}
}

func TestTheLoginShellsPathSurvivesAShellThatFailsOrHangs(t *testing.T) {
	for name, shell := range map[string]string{
		"fails":    fakeShell(t, "exit 3"),
		"missing":  filepath.Join(t.TempDir(), "nope"),
		"empty":    "",
		"relative": "sh",
		"hangs":    fakeShell(t, "exec sleep 30"),
	} {
		start := time.Now()
		if got := loginShellPath(t.Context(), shell); got != "" {
			t.Errorf("%s: PATH = %q", name, got)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%s: took %v", name, d)
		}
	}
}

func TestTheLoginPathIsProbedOncePerTTL(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	shell := fakeShell(t, `echo x >> `+count+`; printf '\n__CFLK_PATH__%s__CFLK_END__\n' "/login/bin"`)
	file := filepath.Join(t.TempDir(), "sub", "login-path")
	for range 3 {
		if got := cachedLoginPath(t.Context(), shell, file); got != "/login/bin" {
			t.Fatalf("got %q", got)
		}
	}
	if data, _ := os.ReadFile(count); strings.Count(string(data), "x") != 1 { //nolint:gosec // test file
		t.Errorf("probes: %q", data)
	}
	old := time.Now().Add(-2 * loginPathTTL)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	cachedLoginPath(t.Context(), shell, file)
	if data, _ := os.ReadFile(count); strings.Count(string(data), "x") != 2 { //nolint:gosec // test file
		t.Errorf("an expired cache is not probed again: %q", data)
	}
}

func TestThePathOfAPeerIsTheOneItsHarnessesAreLaunchedWith(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // the cache lives under it on macOS
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("SHELL", fakeShell(t, `printf '\n__CFLK_PATH__%s__CFLK_END__\n' "/login/bin"`))
	useLoginPath(t.Context())
	if got := os.Getenv("PATH"); !strings.HasPrefix(got, "/login/bin:"+home+"/.local/bin") || !strings.HasSuffix(got, ":/usr/bin") {
		t.Errorf("PATH = %q", got)
	}
}

func TestTheProbeEndsAtItsDeadlineEvenIfADescendantKeepsThePipe(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	shell := fakeShell(t, `sleep 30 &
echo $! > `+pidFile+`
sleep 30`)
	start := time.Now()
	if got := loginShellPath(t.Context(), shell); got != "" {
		t.Errorf("PATH = %q", got)
	}
	if d := time.Since(start); d > loginShellTimeout+time.Second {
		t.Errorf("took %v", d)
	}
	data, _ := os.ReadFile(pidFile) //nolint:gosec // test file
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("pid %q: %v", data, err)
	}
	time.Sleep(100 * time.Millisecond)
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Error("the sleeper the profile started outlived the probe")
	}
}

func TestAFailedProbeIsRememberedBrieflyAndAGoodOneForAnHour(t *testing.T) {
	file := filepath.Join(t.TempDir(), "login-path")
	good := "#!/bin/sh\n" + `printf '\n__CFLK_PATH__%s__CFLK_END__\n' "/login/bin"` + "\n"
	shell := fakeShell(t, "exit 3")
	setShell := func(script string) {
		if err := os.WriteFile(shell, []byte(script), 0o700); err != nil { //nolint:gosec // a test script
			t.Fatal(err)
		}
	}
	age := func(d time.Duration) {
		when := time.Now().Add(-d)
		if err := os.Chtimes(file, when, when); err != nil {
			t.Fatal(err)
		}
	}

	if got := cachedLoginPath(t.Context(), shell, file); got != "" {
		t.Fatalf("got %q", got)
	}
	setShell(good)
	if got := cachedLoginPath(t.Context(), shell, file); got != "" {
		t.Errorf("a fresh failure was probed again: %q", got)
	}
	age(2 * failedLoginPathTTL)
	if got := cachedLoginPath(t.Context(), shell, file); got != "/login/bin" {
		t.Errorf("a failure older than its TTL was not probed again: %q", got)
	}
	setShell("#!/bin/sh\nexit 3\n")
	age(2 * failedLoginPathTTL)
	if got := cachedLoginPath(t.Context(), shell, file); got != "/login/bin" {
		t.Errorf("a good probe expired with the short TTL: %q", got)
	}
}

func TestTheCacheIsWrittenWholeInTheCflkCacheDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "xdg"))
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("SHELL", fakeShell(t, `printf '\n__CFLK_PATH__%s__CFLK_END__\n' "/login/bin"`))
	useLoginPath(t.Context())
	cache, _ := os.UserCacheDir()
	entries, err := os.ReadDir(filepath.Join(cache, cacheDirName))
	if err != nil || len(entries) != 1 || entries[0].Name() != "login-path" {
		t.Errorf("entries %v err %v", entries, err)
	}
}
