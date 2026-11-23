//go:build linux || darwin

package runner_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
	"github.com/Autarquico/codeflock/internal/adapters/harness/localhost"
	"github.com/Autarquico/codeflock/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/runner"
)

// A Codex Job is waiting from the notification at the end of a turn until its
// rollout grows past it (the person answered in the terminal, not through send).
func TestGivenACodexJobWaitingWhenItsRolloutGrowsThenItIsNoLongerWaiting(t *testing.T) {
	const session = "0196a7b2-0000-7000-8000-0000000000aa"
	marker := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name        string
		rolloutAt   *time.Time // nil: the session has no rollout
		wantWaiting bool
	}{
		{"the rollout was last written with the turn that ended", at(marker.Add(-time.Second)), true},
		{"the turn was still writing within the grace period", at(marker.Add(time.Second)), true},
		{"the rollout grew after the grace period", at(marker.Add(10 * time.Second)), false},
		{"the rollout of the session cannot be found", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := exec.LookPath("tmux"); err != nil {
				t.Skip("tmux is not installed")
			}
			codexHome := t.TempDir()
			if tt.rolloutAt != nil {
				writeRollout(t, codexHome, session, *tt.rolloutAt)
			}
			host, err := localhost.New(localhost.Options{Home: t.TempDir(), Env: append(os.Environ(), codex.ConfigHomeEnv+"="+codexHome)})
			if err != nil {
				t.Fatal(err)
			}
			rt, err := tmux.New(tmux.Options{Socket: tmux.SocketName(t.TempDir())})
			if err != nil {
				t.Fatal(err)
			}
			store := job.NewStore(t.TempDir())
			r := runner.New(runner.Config{Store: store, Host: host, Tmux: rt})

			j := job.Job{ID: "job-aaaaaaaaaa", Harness: codex.ID, SessionID: session, State: job.Waiting}
			l := job.NewLaunch(j.ID)
			j.Launch = &l
			markerPath := store.WaitingPath(j.ID, l.ID)
			if err := os.MkdirAll(filepath.Dir(markerPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(markerPath, marker, marker); err != nil {
				t.Fatal(err)
			}

			facts, err := r.Observe(t.Context(), j)
			if err != nil {
				t.Fatal(err)
			}
			if facts.Waiting != tt.wantWaiting {
				t.Errorf("waiting = %v, want %v", facts.Waiting, tt.wantWaiting)
			}
		})
	}
}

func at(t time.Time) *time.Time { return &t }

func writeRollout(t *testing.T, codexHome, session string, mtime time.Time) {
	t.Helper()
	dir := filepath.Join(codexHome, "sessions", "2026", "11", "12")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-11-12T09-29-00-"+session+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}
