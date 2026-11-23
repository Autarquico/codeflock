package checkout

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...) //nolint:gosec // test helper
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The branch that place validated is the one move updates: if it moved in between, the
// update fails instead of overwriting what was put there.
func TestMove_ABranchThatMovedAfterPlaceIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "a")
	git(t, dir, "branch", "cflk/t")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "b")
	commit := git(t, dir, "rev-parse", "HEAD")
	l := &lander{repo: repo{dir: dir}, req: Request{Branch: "cflk/t", Alt: "cflk/t@g1"}}

	pl, err := l.place(t.Context(), commit)
	if err != nil || pl.branch != "cflk/t" || pl.outcome != Advanced {
		t.Fatalf("place = %+v, %v", pl, err)
	}
	git(t, dir, "switch", "-q", "-c", "side", "main~1")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "elsewhere")
	moved := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "branch", "-f", "cflk/t", moved)

	if err := l.move(t.Context(), pl, commit); err == nil {
		t.Fatal("move overwrote a branch that was no longer where place saw it")
	}
	if got := git(t, dir, "rev-parse", "cflk/t"); got != moved {
		t.Fatalf("cflk/t = %s, want %s", got, moved)
	}
}
