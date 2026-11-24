package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/mcp"
)

// fakeJobs is a peer network of one Job that records what the tools asked.
type fakeJobs struct {
	state    string
	screen   string
	sent     []string
	runs     []cli.RunRequest
	attached []bool
	attach   error
	moves    []cli.MoveRequest
	pulls    []cli.PullRequest
	cleaned  []string
}

func (f *fakeJobs) job(id string) cli.JobView {
	return cli.JobView{ID: id, Peer: "omarch01", Repo: "web", Branch: "cflk/" + id, Harness: "claude", State: f.state}
}

func (f *fakeJobs) Run(_ context.Context, req cli.RunRequest) (cli.JobView, error) {
	f.runs = append(f.runs, req)
	f.state = "running"
	return f.job("job-0000000001"), nil
}

func (f *fakeJobs) List(_ context.Context, peer string) (cli.JobList, error) {
	if peer == "none" {
		return cli.JobList{}, nil
	}
	return cli.JobList{
		Jobs:        []cli.JobView{f.job("job-0000000001")},
		Unreachable: []cli.UnreachablePeer{{Peer: "mac-mini", Problem: "ssh: timeout"}},
	}, nil
}

func (f *fakeJobs) Peers(context.Context) ([]cli.PeerView, error) {
	return []cli.PeerView{{Name: "omarch01", This: true, Reachable: true, Harnesses: []string{"claude"}, Jobs: 1}}, nil
}

func (f *fakeJobs) Show(_ context.Context, id string) (cli.JobView, error) {
	if id == "job-gone" {
		return cli.JobView{}, domain.NewError(domain.CodeNotFound, "no such Job", nil)
	}
	return f.job(id), nil
}

func (f *fakeJobs) Stop(_ context.Context, id string) (cli.JobView, error) {
	f.state = "stopped"
	return f.job(id), nil
}

func (f *fakeJobs) Resume(_ context.Context, id string) (cli.JobView, error) {
	f.state = "running"
	return f.job(id), nil
}

func (f *fakeJobs) Screen(context.Context, string) (string, error) { return f.screen, nil }

func (f *fakeJobs) Send(_ context.Context, id, text string) (cli.JobView, error) {
	f.sent = append(f.sent, text)
	f.state = "running"
	return f.job(id), nil
}

func (f *fakeJobs) Move(_ context.Context, req cli.MoveRequest) (cli.JobView, error) {
	f.moves = append(f.moves, req)
	f.state = "moved"
	return f.job(req.ID), nil
}

func (f *fakeJobs) Pull(_ context.Context, req cli.PullRequest) (cli.JobView, error) {
	f.pulls = append(f.pulls, req)
	f.state = "done"
	return f.job(req.ID), nil
}

func (f *fakeJobs) Clean(_ context.Context, id string, _ bool) (cli.JobView, error) {
	f.cleaned = append(f.cleaned, id)
	return f.job(id), nil
}

func (f *fakeJobs) Attach(_ context.Context, _ string, readOnly bool) error {
	f.attached = append(f.attached, readOnly)
	return f.attach
}

func connect(t *testing.T, profile mcp.Profile, jobs mcp.Jobs) *sdk.ClientSession {
	t.Helper()
	ctx := t.Context()
	serverSide, clientSide := sdk.NewInMemoryTransports()
	if _, err := mcp.NewServer(profile, jobs, io.Discard).Connect(ctx, serverSide, nil); err != nil {
		t.Fatal(err)
	}
	session, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func call(t *testing.T, s *sdk.ClientSession, tool string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	res, err := s.CallTool(t.Context(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res
}

func structured(t *testing.T, res *sdk.CallToolResult) map[string]any {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func toolNames(t *testing.T, s *sdk.ClientSession) []string {
	t.Helper()
	var names []string
	for tool, err := range s.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func TestParseProfile(t *testing.T) {
	for _, ok := range []string{"read", "write"} {
		if _, err := mcp.ParseProfile(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	if _, err := mcp.ParseProfile("execute"); err == nil {
		t.Error("execute is not a profile any more")
	}
}

// Scenario: Todo desde Claude Code.
func TestWriteProfileDrivesAJobFromRunToAnswer(t *testing.T) {
	f := &fakeJobs{state: "waiting", screen: "Which database?"}
	s := connect(t, mcp.Write, f)

	run := structured(t, call(t, s, "jobs_run", map[string]any{"prompt": "fix the bug", "on": "any", "harness": "codex"}))
	if run["id"] != "job-0000000001" || run["state"] != "running" {
		t.Errorf("jobs_run = %v", run)
	}
	if len(f.runs) != 1 || f.runs[0] != (cli.RunRequest{Prompt: "fix the bug", On: "any", Harness: "codex"}) {
		t.Errorf("run request = %+v", f.runs)
	}

	f.state = "waiting"
	list := structured(t, call(t, s, "jobs_list", nil))
	jobs, _ := list["jobs"].([]any)
	unreachable, _ := list["unreachable"].([]any)
	if len(jobs) != 1 || len(unreachable) != 1 || jobs[0].(map[string]any)["state"] != "waiting" {
		t.Errorf("jobs_list = %v", list)
	}

	if screen := structured(t, call(t, s, "jobs_screen", map[string]any{"job": "job-0000000001"})); screen["screen"] != "Which database?" {
		t.Errorf("jobs_screen = %v", screen)
	}
	sent := structured(t, call(t, s, "jobs_send", map[string]any{"job": "job-0000000001", "text": "postgres"}))
	if sent["state"] != "running" || !slices.Equal(f.sent, []string{"postgres"}) {
		t.Errorf("jobs_send = %v, sent %v", sent, f.sent)
	}

	if stopped := structured(t, call(t, s, "jobs_stop", map[string]any{"job": "job-0000000001"})); stopped["state"] != "stopped" {
		t.Errorf("jobs_stop = %v", stopped)
	}
	if resumed := structured(t, call(t, s, "jobs_resume", map[string]any{"job": "job-0000000001"})); resumed["state"] != "running" {
		t.Errorf("jobs_resume = %v", resumed)
	}
	if got := structured(t, call(t, s, "jobs_get", map[string]any{"job": "job-0000000001"})); got["peer"] != "omarch01" {
		t.Errorf("jobs_get = %v", got)
	}
}

func TestListsAreArraysWhenEmpty(t *testing.T) {
	s := connect(t, mcp.Read, &fakeJobs{})
	list := structured(t, call(t, s, "jobs_list", map[string]any{"peer": "none"}))
	if jobs, ok := list["jobs"].([]any); !ok || len(jobs) != 0 {
		t.Errorf("jobs = %#v", list["jobs"])
	}
	if un, ok := list["unreachable"].([]any); !ok || len(un) != 0 {
		t.Errorf("unreachable = %#v", list["unreachable"])
	}
	peers := structured(t, call(t, s, "peers_list", nil))
	if p, _ := peers["peers"].([]any); len(p) != 1 {
		t.Errorf("peers_list = %v", peers)
	}
}

// The read profile has no tool that changes anything, and asking for one is an unknown tool.
func TestReadProfileExposesOnlyReadTools(t *testing.T) {
	s := connect(t, mcp.Read, &fakeJobs{})
	want := []string{"jobs_attach", "jobs_get", "jobs_list", "jobs_screen", "peers_list"}
	if got := toolNames(t, s); !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
	for _, tool := range []string{"jobs_run", "jobs_send", "jobs_stop", "jobs_resume", "jobs_move", "jobs_pull", "jobs_clean"} {
		if _, err := s.CallTool(t.Context(), &sdk.CallToolParams{Name: tool, Arguments: map[string]any{"job": "job-0000000001"}}); err == nil {
			t.Errorf("%s answered in the read profile", tool)
		}
	}
}

func TestWriteProfileExposesEveryTool(t *testing.T) {
	want := []string{"jobs_attach", "jobs_clean", "jobs_get", "jobs_list", "jobs_move", "jobs_pull", "jobs_resume", "jobs_run", "jobs_screen", "jobs_send", "jobs_stop", "peers_list"}
	if got := toolNames(t, connect(t, mcp.Write, &fakeJobs{})); !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

// Scenario: Entrar desde Claude Code.
func TestAttachOpensAWindowAndReadIsAlwaysReadOnly(t *testing.T) {
	f := &fakeJobs{}
	res := structured(t, call(t, connect(t, mcp.Write, f), "jobs_attach", map[string]any{"job": "job-0000000001"}))
	if res["job"] != "job-0000000001" || res["read_only"] != false || !slices.Equal(f.attached, []bool{false}) {
		t.Errorf("write: %v, attached %v", res, f.attached)
	}

	f = &fakeJobs{}
	res = structured(t, call(t, connect(t, mcp.Read, f), "jobs_attach", map[string]any{"job": "job-0000000001", "read_only": false}))
	if res["read_only"] != true || !slices.Equal(f.attached, []bool{true}) {
		t.Errorf("read: %v, attached %v", res, f.attached)
	}
}

func TestErrorsKeepTheirCode(t *testing.T) {
	f := &fakeJobs{attach: domain.NewError(domain.CodeRunNotAttachable, "there is no terminal here", nil)}
	s := connect(t, mcp.Write, f)
	for tool, args := range map[string]map[string]any{
		"jobs_get":    {"job": "job-gone"},
		"jobs_attach": {"job": "job-0000000001"},
	} {
		res := call(t, s, tool, args)
		if !res.IsError {
			t.Errorf("%s did not fail", tool)
			continue
		}
		if code := structured(t, res)["code"]; code == "" || code == nil {
			t.Errorf("%s: no code in %v", tool, res.StructuredContent)
		}
	}
}

// Scenario: Todo desde Claude Code (mover, traer y limpiar).
func TestMovePullAndCleanMapToTheirRequests(t *testing.T) {
	f := &fakeJobs{}
	s := connect(t, mcp.Write, f)
	id := "job-0000000001"

	if got := structured(t, call(t, s, "jobs_move", map[string]any{"job": id, "to": "mac-mini"})); got["state"] != "moved" {
		t.Errorf("jobs_move = %v", got)
	}
	call(t, s, "jobs_move", map[string]any{"job": id, "retry": true})
	call(t, s, "jobs_move", map[string]any{"job": id, "abort": true})
	wantMoves := []cli.MoveRequest{{ID: id, To: "mac-mini"}, {ID: id, Retry: true}, {ID: id, Abort: true}}
	if !slices.Equal(f.moves, wantMoves) {
		t.Errorf("moves = %+v", f.moves)
	}

	call(t, s, "jobs_pull", map[string]any{"job": id})
	call(t, s, "jobs_pull", map[string]any{"job": id, "finish": true, "push": true})
	call(t, s, "jobs_pull", map[string]any{"job": id, "finish": true, "abort": true})
	wantPulls := []cli.PullRequest{{ID: id}, {ID: id, Finish: true, Push: true}, {ID: id, Finish: true, Abort: true}}
	if !slices.Equal(f.pulls, wantPulls) {
		t.Errorf("pulls = %+v", f.pulls)
	}

	call(t, s, "jobs_clean", map[string]any{"job": id})
	if !slices.Equal(f.cleaned, []string{id}) {
		t.Errorf("cleaned = %v", f.cleaned)
	}
}

func TestMoveAndPullRejectWhatTheCLIRejects(t *testing.T) {
	f := &fakeJobs{}
	s := connect(t, mcp.Write, f)
	id := "job-0000000001"
	for name, c := range map[string]struct {
		tool string
		args map[string]any
	}{
		"move without destination":  {"jobs_move", map[string]any{"job": id}},
		"move retry and abort":      {"jobs_move", map[string]any{"job": id, "retry": true, "abort": true}},
		"move retry with to":        {"jobs_move", map[string]any{"job": id, "retry": true, "to": "mac-mini"}},
		"pull push without finish":  {"jobs_pull", map[string]any{"job": id, "push": true}},
		"pull abort without finish": {"jobs_pull", map[string]any{"job": id, "abort": true}},
		"pull push and abort":       {"jobs_pull", map[string]any{"job": id, "finish": true, "push": true, "abort": true}},
	} {
		res := call(t, s, c.tool, c.args)
		if !res.IsError || structured(t, res)["code"] != string(domain.CodeInvalidRequest) {
			t.Errorf("%s: %v", name, res.StructuredContent)
		}
	}
	if len(f.moves)+len(f.pulls) != 0 {
		t.Errorf("an invalid request reached the peer: %+v %+v", f.moves, f.pulls)
	}
}
