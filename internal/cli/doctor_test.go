package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/worker/doctor"
)

func runDoctor(t *testing.T, opts Options, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = RunContextWith(context.Background(), opts, append([]string{"doctor"}, args...), strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func withComponents(cs ...doctor.Component) Options {
	return Options{Doctor: func(context.Context) ([]doctor.Component, error) { return cs, nil }}
}

func TestDoctorShowsEachComponentWithItsAction(t *testing.T) {
	code, stdout, stderr := runDoctor(t, withComponents(
		doctor.Component{Name: "git", Status: doctor.ComponentOK, Message: "git version 2.50"},
		doctor.Component{Name: "runtime", Status: doctor.ComponentFail, Message: "no tmux", Action: "instala tmux"},
	))
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"git version 2.50", "no tmux", "instala tmux"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("%q is not in %q", want, stdout)
		}
	}
}

func TestDoctorJSONIsOneValue(t *testing.T) {
	code, stdout, _ := runDoctor(t, withComponents(doctor.Component{Name: "git", Status: doctor.ComponentOK}), "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var rep DoctorReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil || len(rep.Components) != 1 {
		t.Errorf("%q: %v", stdout, err)
	}
	oneJSONValue(t, stdout)
}

func TestDoctorTextFromTheMachineHasNoControlCharacters(t *testing.T) {
	_, stdout, _ := runDoctor(t, withComponents(doctor.Component{Name: "x", Message: "a\x1b[31m\nb"}))
	if strings.ContainsAny(stdout, "\x1b") || strings.Count(stdout, "\n") != 2 {
		t.Errorf("%q", stdout)
	}
}

func TestDoctorWithoutChecksIsNotImplemented(t *testing.T) {
	code, stdout, _ := runDoctor(t, Options{}, "--json")
	if env := errorOf(t, stdout); code != domain.ExitCode(domain.CodeNotImplemented) || env["code"] != string(domain.CodeNotImplemented) {
		t.Errorf("exit %d, envelope %v", code, env)
	}
}
