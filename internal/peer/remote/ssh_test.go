package remote_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/remote"
)

var (
	mini     = discovery.Peer{ID: "nMINI", Name: "mac-mini", Host: "mac-mini.tail1234.ts.net", Online: true}
	ssh      = remote.SSH{ConnectTimeout: 3_000_000_000}
	sshFixed = []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=3", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}
)

func argv(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestTheCommandOfARequestIsSSHToTheHostRunningCflkPeerAndNothingElse(t *testing.T) {
	got, err := ssh.ServeArgv(mini)
	want := argv([]string{"ssh"}, sshFixed, []string{"--", "mac-mini.tail1234.ts.net", "cflk", "peer"})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q, %v\nwant   %q", got, err, want)
	}
}

func TestAttachIsSSHWithATerminalAndTheJobIdAsTheOnlyOperand(t *testing.T) {
	const id = "job-abcdefghij"
	got, err := ssh.AttachArgv(mini, id, false)
	want := argv([]string{"ssh", "-t"}, sshFixed, []string{"--", mini.Host, "cflk", "peer", "attach", id})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q, %v\nwant   %q", got, err, want)
	}
	got, err = ssh.AttachArgv(mini, id, true)
	want = argv([]string{"ssh", "-t"}, sshFixed, []string{"--", mini.Host, "cflk", "peer", "attach", "--read-only", id})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("read-only argv = %q, %v\nwant   %q", got, err, want)
	}
}

func TestNeitherAHostNorAJobIdThatCouldBeAnOptionOrACommandIsAccepted(t *testing.T) {
	for _, host := range []string{"-oProxyCommand=touch /tmp/x", "a b", "host;id", "$(id)", "", "h\nx"} {
		bad := discovery.Peer{Name: "x", Host: host}
		if _, err := ssh.ServeArgv(bad); err == nil {
			t.Errorf("host %q accepted", host)
		}
		if _, err := ssh.PushTarget(bad, "/a/b.git"); err == nil {
			t.Errorf("push to host %q accepted", host)
		}
	}
	for _, id := range []string{"job-$(id)", "--read-only", "job-abcdefghij; id", "../x", ""} {
		if _, err := ssh.AttachArgv(mini, id, false); err == nil {
			t.Errorf("id %q accepted", id)
		}
	}
	if _, err := ssh.Dial(context.Background(), mini, "show"); err == nil {
		t.Error("Dial took operands: the request goes in stdin")
	}
}

func TestTheCodeIsPushedWithGitToTheMirrorOfThePeerUnderTheRefOfTheJob(t *testing.T) {
	const repo = "example.com/o/r"
	mirror := "/home/me/.local/share/cflk/repos/" + job.RepoHash(repo) + ".git"
	target, err := ssh.PushTarget(mini, mirror)
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	got := remote.PushArgv(target, commit, "refs/cflk/job-abcdefghij/g1")
	want := []string{
		"git", "-c", "core.sshCommand=ssh " + strings.Join(sshFixed, " "),
		"push", "--quiet", "--", "ssh://" + mini.Host + mirror, commit + ":refs/cflk/job-abcdefghij/g1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q\nwant   %q", got, want)
	}
}

func TestAMirrorPathThatIsNotAPlainAbsolutePathIsRefused(t *testing.T) {
	for _, m := range []string{"relative/x.git", "/a/../b.git", "/a b.git", "/a;id.git", "/a/$(id).git", "-x", "/a//b.git", "~/x.git"} {
		if _, err := ssh.PushTarget(mini, m); err == nil {
			t.Errorf("mirror %q accepted", m)
		}
	}
}
