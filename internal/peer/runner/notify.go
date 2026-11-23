package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/Autarquico/codeflock/internal/support/atomicfile"
)

// turnComplete is the type of the notification Codex sends when a turn ends.
const turnComplete = "agent-turn-complete"

// HandleNotify is the program Codex runs with `notify`: payload is the JSON it
// passes as its last argument. It writes the waiting marker when the turn that
// ended is the one of the Job's session, and does nothing for any other
// notification: Codex also notifies for its internal threads (the one that
// writes the title), whose thread-id is not the Job's. The Job's session is in
// sessionFile, which the launch writes once it knows it; until then nothing is
// waiting.
func HandleNotify(markerPath, sessionFile, payload string) error {
	var n struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread-id"`
	}
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		return fmt.Errorf("runner: notify payload: %w", err)
	}
	if n.Type != turnComplete || n.ThreadID == "" {
		return nil
	}
	raw, err := os.ReadFile(sessionFile) //nolint:gosec // a file of the launch
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("runner: reading the session of the Job: %w", err)
	}
	if strings.TrimSpace(string(raw)) != n.ThreadID {
		return nil
	}
	return atomicfile.Write(markerPath, nil, 0o600)
}
