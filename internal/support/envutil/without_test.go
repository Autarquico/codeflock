package envutil_test

import (
	"slices"
	"testing"

	"github.com/Autarquico/codeflock/internal/support/envutil"
)

func TestWithoutDropsOnlyTheNamedVariables(t *testing.T) {
	env := []string{"A=1", "CLAUDE_CONFIG_DIR=/x", "CLAUDE_CONFIG_DIR_OTHER=keep", "B=2"}
	got := envutil.Without(env, "CLAUDE_CONFIG_DIR")
	if !slices.Equal(got, []string{"A=1", "CLAUDE_CONFIG_DIR_OTHER=keep", "B=2"}) {
		t.Errorf("got %v", got)
	}
	if env[1] != "CLAUDE_CONFIG_DIR=/x" {
		t.Error("the input was changed")
	}
}
