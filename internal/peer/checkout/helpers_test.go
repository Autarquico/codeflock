package checkout_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/peer/checkout"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, //nolint:gosec // test helper: fixed git options, arguments from the test
		"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commitFile(t *testing.T, dir, name, body, msg string) string {
	t.Helper()
	write(t, filepath.Join(dir, name), body)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", msg)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// world is the checkout of this peer (mac) and the worktree of the peer that ran the Job,
// which took the work from the mac and went on.
type world struct {
	t      *testing.T
	mac    string
	worker string
	base   string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, mac: filepath.Join(root, "mac"), worker: filepath.Join(root, "worker")}
	if err := os.MkdirAll(w.mac, 0o750); err != nil {
		t.Fatal(err)
	}
	gitIn(t, w.mac, "init", "-q", "-b", "main")
	w.base = commitFile(t, w.mac, "README", "base\n", "base")
	gitIn(t, root, "clone", "-q", w.mac, w.worker)
	return w
}

const jobRef = "refs/cflk/job-aaaaaaaaaa/g1"

// request fetches what the worker has at HEAD into the mac, under jobRef, as the transport
// does, and asks for exactly that commit.
func (w *world) request() checkout.Request {
	w.t.Helper()
	commit := gitIn(w.t, w.worker, "rev-parse", "HEAD")
	gitIn(w.t, w.mac, "fetch", "-q", w.worker, "+HEAD:"+jobRef)
	return checkout.Request{Dir: w.mac, Ref: jobRef, Commit: commit, Branch: "cflk/job-aaaaaaaaaa", Alt: "cflk/job-aaaaaaaaaa@g1"}
}

func (w *world) head() string        { return gitIn(w.t, w.mac, "rev-parse", "HEAD") }
func (w *world) branch() string      { return gitIn(w.t, w.mac, "branch", "--show-current") }
func (w *world) tip(b string) string { return gitIn(w.t, w.mac, "rev-parse", "refs/heads/"+b) }

func branchExists(t *testing.T, dir, b string) bool {
	t.Helper()
	return exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+b).Run() == nil //nolint:gosec // test helper
}
