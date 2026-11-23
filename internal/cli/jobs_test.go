package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeJobs struct {
	list     []JobView
	ran      RunRequest
	attached []string
	err      error
}

func (f *fakeJobs) Run(_ context.Context, r RunRequest) (JobView, error) {
	f.ran = r
	return JobView{ID: "job-aaaaaaaaaa", State: "running"}, f.err
}
func (f *fakeJobs) List(context.Context) ([]JobView, error) { return f.list, f.err }
func (f *fakeJobs) Show(_ context.Context, id string) (JobView, error) {
	return JobView{ID: id, State: "waiting", Repo: "h/o/r", Branch: "cflk/" + id}, f.err
}

func (f *fakeJobs) Stop(_ context.Context, id string) (JobView, error) {
	return JobView{ID: id, State: "stopped"}, f.err
}

func (f *fakeJobs) Resume(_ context.Context, id string) (JobView, error) {
	return JobView{ID: id, State: "running"}, f.err
}
func (f *fakeJobs) Screen(context.Context, string) (string, error) { return "the screen\n", f.err }
func (f *fakeJobs) Send(_ context.Context, id, text string) (JobView, error) {
	return JobView{ID: id, State: "running", Problem: text}, f.err
}

func (f *fakeJobs) Attach(_ context.Context, id string, readOnly bool) error {
	f.attached = append(f.attached, id)
	if readOnly {
		f.attached = append(f.attached, "ro")
	}
	return f.err
}

func runWith(opts Options, args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = RunContextWith(context.Background(), opts, args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRunPassesTheFlagsAndThePromptAfterTheSeparator(t *testing.T) {
	f := &fakeJobs{}
	code, out, errOut := runWith(Options{Jobs: f}, "run", "--harness", "codex", "--branch", "main", "--", "fix $(it); now")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	want := RunRequest{Harness: "codex", Branch: "main", Prompt: "fix $(it); now"}
	if f.ran != want {
		t.Errorf("request = %+v, want %+v", f.ran, want)
	}
	if !strings.Contains(out, "job-aaaaaaaaaa") {
		t.Errorf("stdout = %q", out)
	}
}

func TestPsIsATableAndJSONIsAnArrayEvenWhenEmpty(t *testing.T) {
	f := &fakeJobs{list: []JobView{{ID: "job-aaaaaaaaaa", Peer: "mini", State: "waiting", Harness: "codex", Repo: "h/o/r", Branch: "cflk/x"}}}
	_, out, _ := runWith(Options{Jobs: f}, "ps")
	for _, want := range []string{"JOB", "STATE", "job-aaaaaaaaaa", "waiting", "mini"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	_, out, _ = runWith(Options{Jobs: &fakeJobs{}}, "ps", "--json")
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("empty --json = %q", out)
	}
	_, out, _ = runWith(Options{Jobs: f}, "ps", "--json")
	var got []JobView
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 1 || got[0].State != "waiting" {
		t.Errorf("--json = %q, %v", out, err)
	}
}

func TestEachJobCommandReachesItsOperation(t *testing.T) {
	f := &fakeJobs{}
	for _, c := range []struct{ args, want string }{
		{"show job-bbbbbbbbbb", "waiting"},
		{"stop job-bbbbbbbbbb", "stopped"},
		{"resume job-bbbbbbbbbb", "running"},
		{"screen job-bbbbbbbbbb", "the screen"},
		{"send job-bbbbbbbbbb hola", "hola"},
		{"attach job-bbbbbbbbbb", ""},
		{"attach --read-only job-bbbbbbbbbb", ""},
	} {
		code, out, errOut := runWith(Options{Jobs: f}, strings.Fields(c.args)...)
		if code != 0 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", c.args, code, out, errOut)
		}
	}
	if strings.Join(f.attached, ",") != "job-bbbbbbbbbb,job-bbbbbbbbbb,ro" {
		t.Errorf("attached = %v", f.attached)
	}
}

func TestScreenJSONCarriesTheText(t *testing.T) {
	_, out, _ := runWith(Options{Jobs: &fakeJobs{}}, "screen", "--json", "job-bbbbbbbbbb")
	if v := oneJSONValue(t, out); v["screen"] != "the screen\n" {
		t.Errorf("json = %q", out)
	}
}

func TestAnErrorOfTheJobsKeepsItsExitCode(t *testing.T) {
	code, out, _ := runWith(Options{Jobs: &fakeJobs{err: errors.New("boom")}}, "show", "--json", "job-bbbbbbbbbb")
	if code != 1 || errorOf(t, out)["message"] != "boom" {
		t.Errorf("exit %d, stdout %q", code, out)
	}
}

func TestJobCommandsWithoutJobsAreNotImplemented(t *testing.T) {
	code, out, _ := runWith(Options{}, "ps", "--json")
	if code == 0 || errorOf(t, out)["code"] != "CF_NOT_IMPLEMENTED" {
		t.Errorf("exit %d, stdout %q", code, out)
	}
}

func TestWrongArgumentCountsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"run"}, {"run", "a", "b"}, {"send", "job-bbbbbbbbbb"}, {"stop"}, {"ps", "x"}} {
		if code, _, _ := runWith(Options{Jobs: &fakeJobs{}}, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestTheInternalCommandsAreHiddenAndCallTheirHandlers(t *testing.T) {
	var wrapped string
	var notified []string
	opts := Options{
		Wrapper: func(dir string) error { wrapped = dir; return nil },
		Notify:  func(m, s, p string) error { notified = []string{m, s, p}; return nil },
	}
	_, help, _ := runWith(opts, "--help")
	if strings.Contains(help, "internal") {
		t.Errorf("help shows the internal commands:\n%s", help)
	}
	if code, _, e := runWith(opts, "internal", "wrapper", "/launch"); code != 0 || wrapped != "/launch" {
		t.Errorf("wrapper: exit %d, dir %q, %s", code, wrapped, e)
	}
	if code, _, e := runWith(opts, "internal", "notify", "/m", "/s", `{"type":"x"}`); code != 0 || strings.Join(notified, "|") != `/m|/s|{"type":"x"}` {
		t.Errorf("notify: exit %d, args %v, %s", code, notified, e)
	}
}
