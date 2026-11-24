package wire

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestAHeaderIsReadStrictlyDuplicatesCasesNullsAndFieldsOfNoUse(t *testing.T) {
	bad := map[string]string{
		"duplicate key":        `{"op":"show","job":"` + testJob + `","job":"` + testJob + `"}`,
		"case mismatch":        `{"op":"show","Job":"` + testJob + `"}`,
		"upper op key":         `{"OP":"ps"}`,
		"null field":           `{"op":"show","job":null}`,
		"null gen":             `{"op":"show","job":"` + testJob + `","gen":null}`,
		"gen zero on ps":       `{"op":"ps","gen":0}`,
		"size zero on ps":      `{"op":"ps","size":0}`,
		"empty job on ps":      `{"op":"ps","job":""}`,
		"run fields on show":   `{"op":"show","job":"` + testJob + `","harness":""}`,
		"unknown field":        `{"op":"ps","x":1}`,
		"data after the value": `{"op":"ps"} {"op":"ps"}`,
	}
	for name, header := range bad {
		if _, _, err := ReadRequest(strings.NewReader(header + "\n")); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, ok := range []string{`{"op":"ps"}`, `{"op":"show","job":"` + testJob + `","gen":0}`} {
		if _, _, err := ReadRequest(strings.NewReader(ok + "\n")); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for name, header := range map[string]string{
		"nested duplicate": `{"error":{"code":"CF_X","code":"CF_Y","message":"m"}}`,
		"nested case":      `{"hello":{"Name":"x","version":"1","harnesses":[],"jobs":1}}`,
		"null in a list":   `{"hello":{"name":"x","version":"1","harnesses":[null],"jobs":1}}`,
		"null result":      `{"job":null}`,
	} {
		if _, _, err := ReadResponse(strings.NewReader(header + "\n")); !errors.Is(err, ErrProtocol) {
			t.Errorf("response, %s: %v", name, err)
		}
	}
}

func TestAListingTravelsInTheBodySoALongHistoryDoesNotOverflowTheHeader(t *testing.T) {
	jobs := make([]JobView, 0, MaxListedJobs)
	for i := range MaxListedJobs {
		jobs = append(jobs, JobView{
			ID: testID(i), Peer: "mac-mini", Repo: "example.com/o/r", Branch: "cflk/x", Harness: "claude",
			State: "stopped", Gen: 1, Problem: strings.Repeat("p", 2*maxViewText),
		}.Clamp())
	}
	var buf bytes.Buffer
	if err := WriteResponse(&buf, Response{Jobs: jobs}, nil); err != nil {
		t.Fatal(err)
	}
	if header, _, _ := bytes.Cut(buf.Bytes(), []byte("\n")); len(header) > 100 {
		t.Errorf("the header carries the list: %d bytes", len(header))
	}
	resp, _, err := ReadResponse(&buf)
	if err != nil || len(resp.Jobs) != MaxListedJobs || resp.Jobs[7].ID != jobs[7].ID {
		t.Fatalf("got %d Jobs, %v", len(resp.Jobs), err)
	}
	if err := resp.Expect(Request{Op: OpPS}); err != nil {
		t.Error(err)
	}
}

func TestAnEmptyListingIsStillAListing(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteResponse(&buf, Response{Jobs: []JobView{}}, nil); err != nil {
		t.Fatal(err)
	}
	resp, _, err := ReadResponse(&buf)
	if err != nil || !resp.Listing || len(resp.Jobs) != 0 {
		t.Fatalf("%+v, %v", resp, err)
	}
	if err := (Response{}).Expect(Request{Op: OpPS}); !errors.Is(err, ErrProtocol) {
		t.Errorf("an answer with no listing is the answer to ps: %v", err)
	}
}

func TestAListingThatIsNotWellFormedIsRefused(t *testing.T) {
	good := `{"id":"` + testJob + `","state":"running","gen":1,"peer":"","repo":"","branch":"","harness":""}`
	for name, body := range map[string]string{
		"null":        `null`,
		"object":      `{}`,
		"null item":   `[null]`,
		"duplicate":   `[{"id":"` + testJob + `","id":"` + testJob + `","state":"running"}]`,
		"case":        `[{"ID":"` + testJob + `","state":"running"}]`,
		"bad state":   `[{"id":"` + testJob + `","state":"nope"}]`,
		"trailing":    `[` + good + `] []`,
		"long text":   `[{"id":"` + testJob + `","state":"running","problem":"` + strings.Repeat("x", maxViewText+1) + `"}]`,
		"unknown key": `[{"id":"` + testJob + `","state":"running","x":1}]`,
	} {
		stream := fmt.Sprintf(`{"listing":true,"size":%d}`+"\n%s", len(body), body)
		if _, _, err := ReadResponse(strings.NewReader(stream)); !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := ReadResponse(strings.NewReader(`{"listing":true}` + "\n")); !errors.Is(err, ErrProtocol) {
		t.Errorf("a listing with no body: %v", err)
	}
}

func TestAResponseIsOnlyTheAnswerToTheOpThatWasAsked(t *testing.T) {
	view := &JobView{ID: testJob, State: "running"}
	other := &JobView{ID: "job-bbbbbbbbbb", State: "running"}
	for _, c := range []struct {
		name string
		resp Response
		req  Request
		ok   bool
	}{
		{"hello for ps", Response{Hello: &Hello{}}, Request{Op: OpPS}, false},
		{"job for screen with a job", Response{Job: view}, Request{Op: OpScreen, Job: testJob}, false},
		{"screen for a show", Response{}, Request{Op: OpShow, Job: testJob}, false},
		{"other job for stop", Response{Job: other}, Request{Op: OpStop, Job: testJob}, false},
		{"other job for run", Response{Job: other}, Request{Op: OpRun, Job: testJob}, false},
		{"mirror for hello", Response{Mirror: "/a"}, Request{Op: OpHello}, false},
		{"jobs for show", Response{Listing: true}, Request{Op: OpShow, Job: testJob}, false},
		{"stop", Response{Job: view}, Request{Op: OpStop, Job: testJob}, true},
		{"screen", Response{Size: 4}, Request{Op: OpScreen, Job: testJob}, true},
		{"hello", Response{Hello: &Hello{}}, Request{Op: OpHello}, true},
		{"mirror", Response{Mirror: "/a"}, Request{Op: OpMirror}, true},
	} {
		if err := c.resp.Expect(c.req); (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrProtocol)) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// testID is the i-th distinct valid Job id.
func testID(i int) string {
	const digits = "abcdefghijklmnopqrstuvwxyz234567"
	id := []byte("job-aaaaaaaaaa")
	for pos := len(id) - 1; i > 0; pos, i = pos-1, i/len(digits) {
		id[pos] = digits[i%len(digits)]
	}
	return string(id)
}
