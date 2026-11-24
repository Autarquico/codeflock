//go:build realssh

package remote_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/remote"
	"github.com/Autarquico/codeflock/internal/peer/wire"
)

// realPeer is this machine reached through the ssh client, as a peer: it needs
// key-based ssh to $CFLK_REALSSH_HOST (default localhost) and `cflk` on the PATH
// of its non-interactive shell. Without them the tests skip, saying why.
func realPeer(t *testing.T) discovery.Peer {
	t.Helper()
	host := os.Getenv("CFLK_REALSSH_HOST")
	if host == "" {
		host = "localhost"
	}
	p := discovery.Peer{ID: "config:" + host, Name: host, Host: host, Online: true}
	argv, err := ssh.ServeArgv(p)
	if err != nil {
		t.Fatal(err)
	}
	// A host is a peer for this test when `cflk peer` answers a hello over ssh.
	probe := exec.CommandContext(t.Context(), argv[0], argv[1:]...) //nolint:gosec // the argv of the code under test
	probe.Stdin = strings.NewReader(`{"op":"hello"}` + "\n")
	if out, err := probe.Output(); err != nil || !strings.HasPrefix(string(out), "{") {
		t.Skipf("%s is not reachable by ssh with cflk installed: %v", host, err)
	}
	return p
}

func TestHelloAndErrorsOverRealSSH(t *testing.T) {
	p := realPeer(t)
	resp, _, err := remote.Call(t.Context(), ssh, p, wire.Request{Op: wire.OpHello}, nil)
	if err != nil || resp.Hello == nil || resp.Hello.Name == "" {
		t.Fatalf("hello = %+v, %v", resp.Hello, err)
	}
	_, _, err = remote.Call(t.Context(), ssh, p, wire.Request{Op: wire.OpShow, Job: "job-aaaaaaaaaa"}, nil)
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != domain.CodeNotFound {
		t.Errorf("show of an unknown Job: %v", err)
	}
}

func TestNothingInTheStreamIsInterpretedByTheRemoteShell(t *testing.T) {
	p := realPeer(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	text := "x; touch " + marker + "; $(touch " + marker + ") `touch " + marker + "`\n\"'"
	_, _, err := remote.Call(t.Context(), ssh, p, wire.Request{Op: wire.OpSend, Job: "job-aaaaaaaaaa"}, []byte(text))
	if err == nil {
		t.Error("a send to an unknown Job worked")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the text reached a shell")
	}
}
