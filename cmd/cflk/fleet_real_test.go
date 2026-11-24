package main

import (
	"testing"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
)

// Found on the real fleet: a delegated Job leaves its first worktree in the
// peer's mirror on cflk/<id>, and coming back there must not trip on it.
func TestADelegatedJobMovedAwayComesBackToItsPeer(t *testing.T) {
	n := newMoveNet(t, []fakeharness.Step{neverEnds}, "laptop", "mini")
	v := n.delegated(t, "mini")
	n.writeInto(t, "mini", v, "notes.txt", "from mini")

	mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, To: "laptop"})
	back := mustMove(t, n.fleet("laptop"), cli.MoveRequest{ID: v.ID, To: "mini"})
	if back.State != "moved" || back.MovedTo != "mini" || back.MovedGen != 3 {
		t.Fatalf("laptop view = %+v", back)
	}
	if got := n.record(t, "mini", v.ID); got.Gen != 3 || got.State != job.Running && got.State != job.Waiting {
		t.Fatalf("mini = %+v", got)
	}
}
