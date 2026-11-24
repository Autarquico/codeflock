package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestThePushEnvironmentLosesWhatCouldMoveTheCodeElsewhere(t *testing.T) {
	env := pushEnv([]string{
		"PATH=/bin", "GIT_SSH_COMMAND=evil", "GIT_SSH=evil", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=url.x.insteadOf",
		"GIT_CONFIG_VALUE_0=y", "GIT_CONFIG_GLOBAL=/home/me/.gitconfig", "HOME=/home/me",
	})
	for _, kv := range env {
		for _, banned := range []string{"GIT_SSH_COMMAND=", "GIT_SSH=", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY", "GIT_CONFIG_VALUE", "GIT_CONFIG_GLOBAL=/home"} {
			if strings.HasPrefix(kv, banned) {
				t.Errorf("%q survives", kv)
			}
		}
	}
	for _, want := range []string{"PATH=/bin", "HOME=/home/me", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %q", want)
		}
	}
}

// fakeSSH is an ssh that records the host it was asked to reach and fails.
func fakeSSH(t *testing.T, name string) (path, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, name+".log")
	path = filepath.Join(dir, name)
	script := "#!/bin/sh\necho \"$@\" >> '" + log + "'\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // an executable stub of a test
		t.Fatal(err)
	}
	return path, log
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // a test's fixed argv
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestThePushGoesToThePeerWhateverTheGitConfigAndTheEnvironmentSay(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "first")
	// A rule of the repository, one of the user's global config, and an ssh of the environment.
	git(t, repo, "config", "url.ssh://local-evil.test/.insteadOf", "ssh://peer.test/")
	global := filepath.Join(t.TempDir(), "gitconfig")
	rules := "[url \"ssh://global-evil.test/\"]\n\tinsteadOf = ssh://peer.test/\n\tpushInsteadOf = ssh://peer.test/\n"
	if err := os.WriteFile(global, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	envSSH, envLog := fakeSSH(t, "env-ssh")
	t.Setenv("GIT_SSH_COMMAND", envSSH)
	t.Setenv("GIT_SSH", envSSH)
	ours, ourLog := fakeSSH(t, "our-ssh")

	target := PushTarget{URL: "ssh://peer.test/home/me/m.git", Config: []string{"core.sshCommand=" + ours}}
	head, err := exec.CommandContext(t.Context(), "git", "-C", repo, "rev-parse", "HEAD").Output() //nolint:gosec // a test's fixed argv
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(string(head))
	err = runGit(t.Context(), repo, PushArgv(target, commit, "refs/cflk/job-aaaaaaaaaa/g1"))
	if err == nil {
		t.Fatal("the fake ssh failed and the push did not")
	}
	got, readErr := os.ReadFile(ourLog) //nolint:gosec // a log the test made
	if readErr != nil || !strings.Contains(string(got), "peer.test") || strings.Contains(string(got), "evil") {
		t.Errorf("our ssh was asked for %q (%v)", got, readErr)
	}
	if _, statErr := os.Stat(envLog); statErr == nil {
		t.Error("the ssh of the environment ran")
	}
}
