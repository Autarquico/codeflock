package runner_test

import (
	"encoding/json"

	"github.com/Autarquico/codeflock/internal/testutil/fakeharness"
)

func jsonScript(steps []fakeharness.Step) ([]byte, error) {
	return json.Marshal(fakeharness.Script{Steps: steps})
}
