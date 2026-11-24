package runner_test

import (
	"regexp"
	"testing"

	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
	"github.com/Autarquico/codeflock/internal/peer/runner"
)

func TestNewSessionIDHasTheFormatOfTheHarness(t *testing.T) {
	v4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	v7 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for id, re := range map[string]*regexp.Regexp{claude.ID: v4, codex.ID: v7} {
		a, err := runner.NewSessionID(id)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := runner.NewSessionID(id)
		if !re.MatchString(a) || a == b {
			t.Errorf("%s: %q, %q", id, a, b)
		}
	}
	if _, err := runner.NewSessionID("nope"); err == nil {
		t.Error("an unknown harness has no format")
	}
}
