package wire

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

const (
	testJob    = "job-abcdefghij"
	testCommit = "0123456789abcdef0123456789abcdef01234567"
)

func runRequest() Request {
	return Request{Op: OpRun, Job: testJob, Harness: "claude", Repo: "example.com/o/r", Commit: testCommit, CreatedBy: "mac-mini"}
}

func TestARequestSurvivesTheWire(t *testing.T) {
	var buf bytes.Buffer
	prompt := []byte("fix \"the\" tests; $(rm -rf /)\nthen `stop`")
	if err := WriteRequest(&buf, runRequest(), prompt); err != nil {
		t.Fatal(err)
	}
	req, body, err := ReadRequest(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if req.Job != testJob || req.Size != int64(len(prompt)) || !bytes.Equal(body, prompt) {
		t.Errorf("got %+v %q", req, body)
	}
}

func TestARequestThatDoesNotMatchItsOpIsRefused(t *testing.T) {
	bad := map[string]Request{
		"unknown op":         {Op: "exec"},
		"show without job":   {Op: OpShow},
		"ps with a job":      {Op: OpPS, Job: testJob},
		"job id with shell":  {Op: OpShow, Job: "job-$(id)aaaaa"},
		"upper case id":      {Op: OpShow, Job: "job-ABCDEFGHIJ"},
		"negative gen":       {Op: OpShow, Job: testJob, Gen: -1},
		"gen on run":         {Op: OpRun, Job: testJob, Gen: 2},
		"repo not normal":    {Op: OpMirror, Repo: "https://example.com/o/r.git"},
		"repo with dots":     {Op: OpMirror, Repo: "example.com/../r"},
		"hello with a body":  {Op: OpHello, Size: 3},
		"run with a bad sha": {Op: OpRun, Job: testJob, Harness: "claude", Repo: "example.com/o/r", Commit: "main", CreatedBy: "a", Size: 1},
		"run name is an opt": {Op: OpRun, Job: testJob, Harness: "claude", Repo: "example.com/o/r", Commit: testCommit, CreatedBy: "-oProxyCommand=x", Size: 1},
		"commit outside run": {Op: OpShow, Job: testJob, Commit: testCommit},
		"send without body":  {Op: OpSend, Job: testJob},
	}
	for name, req := range bad {
		if err := req.Validate(); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAStreamThatDoesNotMatchItsHeaderIsRefusedWhole(t *testing.T) {
	good := `{"op":"send","job":"` + testJob + `","size":5}` + "\n"
	bad := map[string]string{
		"truncated body":     good + "abc",
		"extra after body":   good + "abcdef",
		"unknown field":      `{"op":"ps","cwd":"/"}` + "\n",
		"two values":         `{"op":"ps"}{"op":"ps"}` + "\n",
		"no newline":         `{"op":"ps"}`,
		"not json":           "ps\n",
		"header too long":    `{"op":"` + strings.Repeat("a", MaxHeader) + `"}` + "\n",
		"duplicate trailing": `{"op":"ps"}` + "\n" + `{"op":"ps"}` + "\n",
	}
	for name, stream := range bad {
		if _, _, err := ReadRequest(strings.NewReader(stream)); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, body, err := ReadRequest(strings.NewReader(good + "abcde")); err != nil || string(body) != "abcde" {
		t.Errorf("the good stream: %q, %v", body, err)
	}
}

func TestAHeaderOfExactlyTheLimitIsRead(t *testing.T) {
	pad := MaxHeader - len(`{"mirror":""}`)
	var resp Response
	line := `{"mirror":"` + strings.Repeat("a", pad) + `"}`
	if len(line) != MaxHeader {
		t.Fatalf("test header is %d bytes", len(line))
	}
	if _, err := ReadFrame(strings.NewReader(line+"\n"), &resp, func() int64 { return 0 }); err != nil {
		t.Errorf("a header at the limit: %v", err)
	}
}

func TestAResponseCannotNameAJobItDoesNotHave(t *testing.T) {
	stream := `{"job":{"id":"../../etc","peer":"p","repo":"r","branch":"b","harness":"claude","state":"running","gen":1}}` + "\n"
	if _, _, err := ReadResponse(strings.NewReader(stream)); !errors.Is(err, ErrProtocol) {
		t.Errorf("a view with a path for an id: %v", err)
	}
	stream = `{"job":{"id":"` + testJob + `","peer":"p","repo":"r","branch":"b","harness":"claude","state":"flying","gen":1}}` + "\n"
	if _, _, err := ReadResponse(strings.NewReader(stream)); !errors.Is(err, ErrProtocol) {
		t.Errorf("a view with an invented state: %v", err)
	}
}

func TestMirrorRefIsBuiltOnlyFromAValidJobAndGen(t *testing.T) {
	if ref, err := MirrorRef(testJob, 1); err != nil || ref != "refs/cflk/"+testJob+"/g1" {
		t.Errorf("ref = %q, %v", ref, err)
	}
	for _, id := range []string{"", "job-x", "../x", testJob + "/g1"} {
		if _, err := MirrorRef(id, 1); err == nil {
			t.Errorf("a ref for %q", id)
		}
	}
	if _, err := MirrorRef(testJob, 0); err == nil {
		t.Error("a ref for gen 0")
	}
}

func FuzzReadRequest(f *testing.F) {
	var buf bytes.Buffer
	_ = WriteRequest(&buf, runRequest(), []byte("prompt"))
	f.Add(buf.Bytes())
	f.Add([]byte(`{"op":"ps"}` + "\n"))
	f.Add([]byte(`{"op":"send","job":"` + testJob + `","size":2}` + "\nhi"))
	f.Add([]byte("{"))
	f.Fuzz(func(t *testing.T, stream []byte) {
		req, body, err := ReadRequest(bytes.NewReader(stream))
		if err != nil {
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("a refusal that is not a protocol error: %v", err)
			}
			return
		}
		if err := req.Validate(); err != nil {
			t.Fatalf("an accepted request does not validate: %v", err)
		}
		if int64(len(body)) != req.Size {
			t.Fatalf("body of %d bytes for size %d", len(body), req.Size)
		}
	})
}

func TestAResponseSaysOneThingAndItsTextIsBounded(t *testing.T) {
	view := &JobView{ID: testJob, State: "running"}
	bad := map[string]Response{
		"error and result":    {Error: &Error{Code: "CF_X", Message: "m"}, Job: view},
		"two results":         {Job: view, Mirror: "/a"},
		"hello and jobs":      {Hello: &Hello{}, Jobs: []JobView{*view}},
		"error with a body":   {Error: &Error{Code: "CF_X"}, Size: 1},
		"code with a newline": {Error: &Error{Code: "CF_X\nfake"}},
		"lowercase code":      {Error: &Error{Code: "oops"}},
		"huge message":        {Error: &Error{Code: "CF_X", Message: strings.Repeat("m", maxErrorMessage+1)}},
		"negative jobs":       {Hello: &Hello{Jobs: -1}},
		"harness not a name":  {Hello: &Hello{Harnesses: []string{"a b"}}},
		"too many harnesses":  {Hello: &Hello{Harnesses: make([]string, maxHarnesses+1)}},
		"long name":           {Hello: &Hello{Name: strings.Repeat("n", maxHelloText+1)}},
	}
	for name, r := range bad {
		if err := r.Validate(); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v", name, err)
		}
	}
	good := []Response{
		{},
		{Job: view, Size: 0},
		{Mirror: "/a/b.git"},
		{Size: 3},
		{Jobs: []JobView{*view}},
		{Hello: &Hello{Name: "this machine", Harnesses: []string{"claude", "codex"}, Jobs: 2}},
		{Error: &Error{Code: "CF_NOT_FOUND", Message: "gone"}},
	}
	for i, r := range good {
		if err := r.Validate(); err != nil {
			t.Errorf("good response %d: %v", i, err)
		}
	}
}

func FuzzReadResponse(f *testing.F) {
	var buf bytes.Buffer
	_ = WriteResponse(&buf, Response{Job: &JobView{ID: testJob, State: "running"}}, nil)
	f.Add(buf.Bytes())
	buf.Reset()
	_ = WriteResponse(&buf, Response{}, []byte("screen"))
	f.Add(buf.Bytes())
	buf.Reset()
	_ = WriteResponse(&buf, Response{Jobs: []JobView{{ID: testJob, State: "running"}}}, nil)
	f.Add(buf.Bytes())
	f.Add([]byte(`{"error":{"code":"CF_NOT_FOUND","message":"gone"}}` + "\n"))
	f.Add([]byte(`{"hello":{"name":"x","version":"1","harnesses":["claude"],"jobs":1}}` + "\n"))
	f.Add([]byte("{"))
	f.Fuzz(func(t *testing.T, stream []byte) {
		resp, body, err := ReadResponse(bytes.NewReader(stream))
		if err != nil {
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("a refusal that is not a protocol error: %v", err)
			}
			return
		}
		if err := resp.Validate(); err != nil {
			t.Fatalf("an accepted response does not validate: %v", err)
		}
		if int64(len(body)) != resp.Size {
			t.Fatalf("body of %d bytes for size %d", len(body), resp.Size)
		}
	})
}

func TestAHelloAnswerWithABodyIsNotTheAnswerToHello(t *testing.T) {
	req := Request{Op: OpHello}
	if err := (Response{Hello: &Hello{Name: "mac-mini"}}).Expect(req); err != nil {
		t.Fatalf("a plain hello: %v", err)
	}
	if err := (Response{Hello: &Hello{Name: "mac-mini"}, Size: 3}).Expect(req); err == nil {
		t.Error("a hello with a body was accepted")
	}
}

func TestTheOrdersOfAMovementCarryOnlyWhatTheyNeed(t *testing.T) {
	transfer := "abcdefghijklmnop"
	ok := map[string]Request{
		"move":     {Op: OpMove, Job: testJob, Gen: 1, To: "mac-mini"},
		"retry":    {Op: OpMoveRetry, Job: testJob, Gen: 1},
		"abort":    {Op: OpMoveAbort, Job: testJob, Gen: 1},
		"activate": {Op: OpActivate, Job: testJob, Gen: 2, Transfer: transfer},
		"abort-t":  {Op: OpAbortTransfer, Job: testJob, Gen: 2, Transfer: transfer},
		"force":    {Op: OpResume, Job: testJob, Force: true},
	}
	for name, req := range ok {
		if err := req.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]Request{
		"move without destination":  {Op: OpMove, Job: testJob, Gen: 1},
		"move without generation":   {Op: OpMove, Job: testJob, To: "mac-mini"},
		"retry names a destination": {Op: OpMoveRetry, Job: testJob, Gen: 1, To: "mac-mini"},
		"activate without transfer": {Op: OpActivate, Job: testJob, Gen: 2},
		"transfer with shell":       {Op: OpActivate, Job: testJob, Gen: 2, Transfer: "$(id)aaaaaaaaaaaa"},
		"stop forced":               {Op: OpStop, Job: testJob, Force: true},
	}
	for name, req := range bad {
		if err := req.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
