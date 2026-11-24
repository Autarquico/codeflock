//go:build linux || darwin

package runner_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/Autarquico/codeflock/internal/peer/runner"
	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
)

// asWrapper makes the test binary the wrapper of a launch, as `cflk` is in production.
const asWrapper = "CF_TEST_AS_WRAPPER"

// asNotify makes it the notify program of Codex: marker, session file and payload are its arguments.
const asNotify = "CF_TEST_AS_NOTIFY"

func TestMain(m *testing.M) {
	fakeharness.RunIfRequested()
	if os.Getenv(asNotify) != "" && len(os.Args) == 4 {
		if err := runner.HandleNotify(os.Args[1], os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if dir := os.Getenv(asWrapper); dir != "" {
		_ = os.Unsetenv(asWrapper)
		if err := runner.RunWrapper(dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
