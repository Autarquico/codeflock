package runner

import (
	"encoding/json"
	"os"
	"testing"
)

// WriteClaudeSettings exposes the hooks writer to the tests.
var WriteClaudeSettings = writeClaudeSettings

// ReadHooks is the commands of each event of a settings file.
func ReadHooks(t *testing.T, path string) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for event, groups := range s.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				out[event] = append(out[event], h.Command)
			}
		}
	}
	return out
}
