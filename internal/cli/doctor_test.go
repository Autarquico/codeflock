package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	return Options{Doctor: func(context.Context, bool) ([]doctor.Component, error) { return cs, nil }}
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
	code, stdout, _ := runDoctor(t, withComponents(doctor.Component{Name: "harness:x", Status: doctor.ComponentOK}), "--json")
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
	_, stdout, _ := runDoctor(t, withComponents(doctor.Component{Name: "harness:x", Message: "a\x1b[31m\nb"}))
	if strings.ContainsAny(stdout, "\x1b") || strings.Count(stdout, "\n") != 2 {
		t.Errorf("%q", stdout)
	}
}

func TestDoctorFixFlagReachesTheChecks(t *testing.T) {
	var got []bool
	opts := Options{Doctor: func(_ context.Context, fix bool) ([]doctor.Component, error) {
		got = append(got, fix)
		return nil, nil
	}}
	runDoctor(t, opts)
	runDoctor(t, opts, "--fix")
	if len(got) != 2 || got[0] || !got[1] {
		t.Errorf("fix per call: %v", got)
	}
}

func TestDoctorWithoutChecksIsNotImplemented(t *testing.T) {
	code, stdout, _ := runDoctor(t, Options{}, "--json")
	if env := errorOf(t, stdout); code != domain.ExitCode(domain.CodeNotImplemented) || env["code"] != string(domain.CodeNotImplemented) {
		t.Errorf("exit %d, envelope %v", code, env)
	}
}

type fleetOf struct {
	fakeJobs
	peers []PeerView
	err   error
}

func (f *fleetOf) Peers(context.Context) ([]PeerView, error) { return f.peers, f.err }

func doctorOf(f *fleetOf, machine ...doctor.Component) Options {
	opts := withComponents(machine...)
	opts.Jobs = f
	return opts
}

func TestDoctorSaysHowEachPeerAnswersAndWhenNoHarnessIsInstalled(t *testing.T) {
	f := &fleetOf{peers: []PeerView{
		{Name: "laptop", This: true, Reachable: true},
		{Name: "omarch01", Reachable: true, Harnesses: []string{"claude"}, Jobs: 2},
		{Name: "mac-mini", Problem: "exit status 127: cflk: command not found", Probe: "ssh mini.tail.ts.net bin/cflk peer"},
	}}
	code, stdout, _ := runDoctor(t, doctorOf(f, doctor.Component{Name: "git", Status: doctor.ComponentOK}))
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"harness", "peer:omarch01", "responde (hello); claude; 2 jobs", "peer:mac-mini", "cflk: command not found", "ssh mini.tail.ts.net bin/cflk peer"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("%q is not in:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "peer:laptop") {
		t.Errorf("this machine is not a peer of itself:\n%s", stdout)
	}
}

func TestDoctorFailsTheConfigThatDoesNotParse(t *testing.T) {
	f := &fleetOf{err: errors.New("config.toml: line 1: expected a string")}
	_, stdout, _ := runDoctor(t, doctorOf(f, doctor.Component{Name: "harness:claude", Status: doctor.ComponentOK}), "--json")
	var rep DoctorReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil || len(rep.Components) != 2 {
		t.Fatalf("%q: %v", stdout, err)
	}
	if c := rep.Components[1]; c.Name != "peers" || c.Status != doctor.ComponentFail || !strings.Contains(c.Message, "line 1") {
		t.Errorf("component = %+v", c)
	}
}

func TestDoctorFixFailsWhenALeftoverCannotBeRemoved(t *testing.T) {
	opts := withComponents(doctor.Component{Name: "legacy:unit:cf-worker", Status: doctor.ComponentFail, Message: "no se pudo parar"})
	if code, _, _ := runDoctor(t, opts); code != 0 {
		t.Errorf("without --fix exit %d", code)
	}
	if code, _, _ := runDoctor(t, opts, "--fix"); code == 0 {
		t.Error("--fix with a failed removal exited 0")
	}
}

func TestDoctorFixJSONFailureIsOneDocument(t *testing.T) {
	opts := withComponents(doctor.Component{Name: "legacy:unit:cf-worker", Status: doctor.ComponentFail, Message: "no se pudo parar"})
	code, stdout, _ := runDoctor(t, opts, "--fix", "--json")
	var doc struct {
		Error struct {
			Details struct {
				Components []doctor.Component `json:"components"`
			} `json:"details"`
		} `json:"error"`
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&doc); err != nil || dec.More() || code == 0 || len(doc.Error.Details.Components) == 0 {
		t.Errorf("exit %d, err %v, more %v, stdout %q", code, err, dec.More(), stdout)
	}
}
