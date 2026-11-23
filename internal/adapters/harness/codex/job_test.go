package codex_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
)

func writeMetaRollout(t *testing.T, configDir, id, cwd string, at time.Time) {
	t.Helper()
	dir := filepath.Join(configDir, "sessions", at.Format("2006"), at.Format("01"), at.Format("02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("rollout-%s-%s.jsonl", at.Format("2006-01-02T15-04-05"), id)
	line := fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q,"timestamp":%q}}`+"\n", id, cwd, at.UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(dir, name), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverSessionsFindsTheOneCreatedAfterTheStartInTheWorktree(t *testing.T) {
	cfg, wt, elsewhere := t.TempDir(), t.TempDir(), t.TempDir()
	start := time.Now().Add(-time.Minute).Truncate(time.Second)
	const old, mine, other = "0196a7b2-0000-7000-8000-000000000001", "0196a7b2-0000-7000-8000-000000000002", "0196a7b2-0000-7000-8000-000000000003"
	writeMetaRollout(t, cfg, old, wt, start.Add(-time.Hour))
	writeMetaRollout(t, cfg, other, elsewhere, start.Add(time.Second))
	got, err := codex.DiscoverSessions(t.Context(), cfg, wt, start, time.Now().Add(time.Hour))
	if err != nil || len(got) != 0 {
		t.Fatalf("before mine: %v, %v", got, err)
	}
	writeMetaRollout(t, cfg, mine, wt, start.Add(2*time.Second))
	if got, err = codex.DiscoverSessions(t.Context(), cfg, wt, start, time.Now().Add(time.Hour)); err != nil || len(got) != 1 || got[0] != mine {
		t.Fatalf("sessions = %v, %v", got, err)
	}
	writeMetaRollout(t, cfg, "0196a7b2-0000-7000-8000-000000000004", wt, start.Add(3*time.Second))
	if got, _ = codex.DiscoverSessions(t.Context(), cfg, wt, start, start.Add(2*time.Second)); len(got) != 1 || got[0] != mine {
		t.Errorf("a session created after the end of the launch is not its own: %v", got)
	}
	if got, _ = codex.DiscoverSessions(t.Context(), cfg, wt, start, time.Now().Add(time.Hour)); len(got) != 2 {
		t.Errorf("two sessions are both reported (the caller refuses them): %v", got)
	}
}
