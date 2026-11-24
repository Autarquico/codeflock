package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeShell(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakesh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	return path
}

func TestThePathOfAPeerAddsTheLoginShellsAndTheUsualDirsWithoutRepeats(t *testing.T) {
	shell := fakeShell(t, `echo "motd of the profile"; printf %s "/login/bin:/usr/bin:/mise/bin"`)
	got := peerPath(t.Context(), "/usr/bin:/bin", shell, "/home/u")
	dirs := strings.Split(got, ":")
	want := []string{"/usr/bin", "/bin", "/login/bin", "/mise/bin", "/home/u/.local/bin", "/opt/homebrew/bin", "/usr/local/bin", "/home/u/.local/share/mise/shims", "/home/u/.local/share/mise/command-wrappers/bin"}
	if strings.Join(dirs, " ") != strings.Join(want, " ") {
		t.Errorf("PATH = %v\nwant   %v", dirs, want)
	}
}

func TestThePathOfAPeerSurvivesALoginShellThatFailsOrHangs(t *testing.T) {
	for name, shell := range map[string]string{
		"fails":    fakeShell(t, "exit 3"),
		"missing":  filepath.Join(t.TempDir(), "nope"),
		"empty":    "",
		"relative": "sh",
	} {
		got := peerPath(t.Context(), "/usr/bin", shell, "/home/u")
		if !strings.HasPrefix(got, "/usr/bin:/home/u/.local/bin") {
			t.Errorf("%s: PATH = %q", name, got)
		}
	}
}

func TestThePathOfAPeerIsTheOneItsHarnessesAreLaunchedWith(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("SHELL", fakeShell(t, `printf %s "/login/bin"`))
	useLoginPath(t.Context())
	if got := os.Getenv("PATH"); !strings.HasPrefix(got, "/usr/bin:/login/bin:/home/u/.local/bin") {
		t.Errorf("PATH = %q", got)
	}
}
