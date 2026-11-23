package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
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
// sessionFile, which the launch writes once it knows it; a turn that ends
// before that is kept (pendingSuffix) and the launch marks it waiting then.
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
	known, err := sessionOf(sessionFile)
	if err != nil {
		return err
	}
	if known == "" {
		// The launch has not found its session yet (a first turn that ends before
		// Codex's rollout is seen): keep the turn, and look again, in this order so
		// that the launch either sees the turn or this sees the session.
		if err := recordTurn(sessionFile+pendingSuffix, n.ThreadID); err != nil {
			return err
		}
		if known, err = sessionOf(sessionFile); err != nil {
			return err
		}
	}
	if known != n.ThreadID {
		return nil
	}
	return atomicfile.Write(markerPath, nil, 0o600)
}

// pendingSuffix names, next to the session file, the threads whose turn ended
// before the session was known.
const pendingSuffix = ".turns"

// markTurnsEndedBefore writes the waiting marker if the turn of session ended
// before the launch knew it; the launch calls it right after recording the session.
func markTurnsEndedBefore(markerPath, sessionFile, session string) error {
	raw, err := os.ReadFile(sessionFile + pendingSuffix) //nolint:gosec // a file of the launch
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("runner: reading the turns of the launch: %w", err)
	}
	if slices.Contains(strings.Fields(string(raw)), session) {
		return atomicfile.Write(markerPath, nil, 0o600)
	}
	return nil
}

func recordTurn(path, threadID string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // a file of the launch
	if err != nil {
		return fmt.Errorf("runner: recording the turn: %w", err)
	}
	_, werr := f.WriteString(threadID + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("runner: recording the turn: %w", werr)
	}
	return nil
}

// sessionOf is the session the launch recorded; empty if it has not yet.
func sessionOf(sessionFile string) (string, error) {
	raw, err := os.ReadFile(sessionFile) //nolint:gosec // a file of the launch
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("runner: reading the session of the Job: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}
