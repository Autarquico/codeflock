package job

import (
	"errors"
	"testing"
)

func intp(n int) *int { return &n }

func TestNormalizeRepo(t *testing.T) {
	const want = "github.com/acme/widgets"
	for _, in := range []string{
		"git@github.com:acme/widgets.git",
		"git@github.com:acme/widgets",
		"git@GitHub.com:acme/widgets.git/",
		"github.com:acme/widgets",
		"git@github.com:/acme/widgets.git",
		"ssh://git@github.com/acme/widgets.git",
		"ssh://git@github.com:22/acme/widgets.git",
		"https://github.com/acme/widgets",
		"https://GITHUB.com/acme/widgets.git",
		"https://github.com/acme/widgets/",
		"https://github.com:443/acme/widgets.git",
		"https://user:tok@github.com/acme/widgets.git",
		"git://github.com/acme/widgets.git",
		"  https://github.com/acme/widgets.git\n",
	} {
		got, err := NormalizeRepo(in)
		if err != nil || got != want {
			t.Errorf("NormalizeRepo(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestNormalizeRepoKeepsNonDefaultPortAndPathCase(t *testing.T) {
	for in, want := range map[string]string{
		"ssh://git@git.example.com:2222/Acme/Widgets.git": "git.example.com:2222/Acme/Widgets",
		"https://git.example.com:8443/acme/widgets":       "git.example.com:8443/acme/widgets",
		"https://gitlab.com/group/sub/widgets.git":        "gitlab.com/group/sub/widgets",
		"ssh://git@[2001:db8::1]:2222/acme/widgets":       "[2001:db8::1]:2222/acme/widgets",
	} {
		if got, err := NormalizeRepo(in); err != nil || got != want {
			t.Errorf("NormalizeRepo(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestNormalizeRepoRejectsAmbiguousForms(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"/srv/git/widgets.git",
		"./widgets",
		"../acme/widgets",
		"file:///srv/git/widgets.git",
		"ftp://github.com/acme/widgets",
		"C:\\repos\\widgets",
		"C:/repos/widgets",
		"git@github.com:22/acme/widgets.git", // host:port or owner "22"?
		"github.com/acme/widgets",            // no scheme, no colon
		"https://github.com/widgets",         // no owner
		"https://github.com/",
		"https://github.com//widgets",
		"https://github.com/acme/../widgets",
		"https://github.com/acme/widgets?ref=x",
		"https://github.com/acme/widgets#frag",
		"https://github.com/acme/wid gets",
		"https:///acme/widgets",
		"https://github.com:abc/acme/widgets",
	} {
		got, err := NormalizeRepo(in)
		if !errors.Is(err, ErrAmbiguousRepo) {
			t.Errorf("NormalizeRepo(%q) = %q, %v; want ErrAmbiguousRepo", in, got, err)
		}
	}
}

func TestRepoHashIsStableSHA256(t *testing.T) {
	h := RepoHash("github.com/acme/widgets")
	if len(h) != 64 || h != RepoHash("github.com/acme/widgets") || h == RepoHash("github.com/acme/other") {
		t.Fatalf("unexpected hash %q", h)
	}
}

func TestIDs(t *testing.T) {
	for range 50 {
		if id := NewID(); !ValidID(id) {
			t.Fatalf("NewID() = %q is not valid", id)
		}
		if l := NewLaunchID(); !ValidLaunchID(l) {
			t.Fatalf("NewLaunchID() = %q is not valid", l)
		}
	}
	for _, bad := range []string{
		"", "job-", "job-abcdefghi", "job-abcdefghijk", "job-ABCDEFGHIJ", "job-abcdefghi1",
		"job-abcdefghij\n", "x job-abcdefghij", "../job-abcdefghij", "job-abcdefghij/..", "abcdefghij",
	} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
	for _, bad := range []string{"", "abcdefg", "abcdefghi", "ABCDEFGH", "abcdefg1", "abcdefgh\n", "../abcdef"} {
		if ValidLaunchID(bad) {
			t.Errorf("ValidLaunchID(%q) = true", bad)
		}
	}
}

func TestNewLaunchNamesItsTmuxSession(t *testing.T) {
	l := NewLaunch("job-abcdefghij")
	if want := "cflk-job-abcdefghij-" + l.ID; l.Session != want {
		t.Fatalf("Session = %q, want %q", l.Session, want)
	}
}

func TestReconcile(t *testing.T) {
	tests := []struct {
		name  string
		state State
		facts Facts
		want  State
	}{
		{"alive running", Running, Facts{SessionAlive: true}, Running},
		{"alive waiting marker", Running, Facts{SessionAlive: true, Waiting: true}, Waiting},
		{"marker removed", Waiting, Facts{SessionAlive: true}, Running},
		{"lost with session back", Lost, Facts{SessionAlive: true}, Running},
		{"alive wins over exit", Running, Facts{SessionAlive: true, Exit: intp(0)}, Running},
		{"exit 0", Running, Facts{Exit: intp(0), GroupEmpty: true}, Done},
		{"exit nonzero", Waiting, Facts{Exit: intp(3), GroupEmpty: true}, Failed},
		{"exit from lost", Lost, Facts{Exit: intp(0), GroupEmpty: true}, Done},
		{"exit with leftovers", Running, Facts{Exit: intp(0)}, Stopping},
		{"no session no exit", Running, Facts{GroupEmpty: true}, Lost},
		{"waiting no session no exit", Waiting, Facts{GroupEmpty: true}, Lost},
		{"lost stays lost", Lost, Facts{GroupEmpty: true}, Lost},
		{"lost with leftovers", Lost, Facts{}, Stopping},
		{"no session leftovers", Running, Facts{}, Stopping},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Reconcile(tc.state, tc.facts)
			if got != tc.want {
				t.Fatalf("Reconcile(%s, %+v) = %s, want %s", tc.state, tc.facts, got, tc.want)
			}
			if got != tc.state && !CanTransition(tc.state, got) {
				t.Fatalf("outcome %s -> %s is not an allowed transition", tc.state, got)
			}
		})
	}
}

func TestReconcileLeavesOtherStatesAlone(t *testing.T) {
	facts := []Facts{{}, {SessionAlive: true}, {Exit: intp(1), GroupEmpty: true}, {GroupEmpty: true}}
	for _, s := range []State{Stopping, Stopped, Done, Failed, Moving, Finishing, Moved} {
		for _, f := range facts {
			if got := Reconcile(s, f); got != s {
				t.Errorf("Reconcile(%s, %+v) = %s, want unchanged", s, f, got)
			}
		}
	}
}

func TestTransitions(t *testing.T) {
	allowed := [][2]State{
		{Running, Stopping},
		{Stopping, Stopped},
		{Stopped, Running},
		{Stopped, Moving},
		{Moving, Moved},
		{Moving, Stopped},
		{Moving, Moving},
		{Stopped, Finishing},
		{Finishing, Done},
		{Finishing, Stopped},
		{Lost, Running},
		{Failed, Running},
		{Running, Waiting},
		{Waiting, Running},
	}
	for _, p := range allowed {
		if !CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s refused", p[0], p[1])
		}
	}
	refused := [][2]State{
		{Running, Stopped},
		{Running, Moving},
		{Running, Moved},
		{Stopping, Running},
		{Stopping, Moving},
		{Stopped, Done},
		{Stopped, Moved},
		{Done, Running},
		{Moved, Running},
		{Moved, Stopped},
		{Moving, Done},
		{Finishing, Moved},
		{Waiting, Stopped},
		{Done, Done},
		{"bogus", Running},
		{Running, "bogus"},
	}
	for _, p := range refused {
		if CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s allowed", p[0], p[1])
		}
	}
}

func TestStatesAreExactlyTheSpecs(t *testing.T) {
	want := []State{Running, Waiting, Stopping, Stopped, Done, Failed, Lost, Moving, Finishing, Moved}
	if len(transitions) != len(want) {
		t.Fatalf("%d states, want %d", len(transitions), len(want))
	}
	for _, s := range want {
		if !s.Valid() {
			t.Errorf("%s is not valid", s)
		}
	}
	if State("paused").Valid() {
		t.Error("paused is valid")
	}
}

func TestGenDir(t *testing.T) {
	if GenDir(2) != "g2" {
		t.Fatal(GenDir(2))
	}
}

func TestJobTransition(t *testing.T) {
	j := Job{ID: "job-abcdefghij", State: Running}
	if err := j.Transition(Stopped); err == nil || j.State != Running {
		t.Fatalf("illegal transition: err=%v state=%s", err, j.State)
	}
	if err := j.Transition(Stopping); err != nil || j.State != Stopping {
		t.Fatalf("legal transition: err=%v state=%s", err, j.State)
	}
	if err := j.Transition(Stopped); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestAnOrderForAnotherGenerationOrAMovedJobIsStale(t *testing.T) {
	j := Job{ID: "job-abcdefghij", Gen: 3, State: Running}
	if err := j.ExpectGen(0); err != nil {
		t.Errorf("gen 0 means any: %v", err)
	}
	if err := j.ExpectGen(3); err != nil {
		t.Errorf("the generation it is in: %v", err)
	}
	var stale *StaleError
	if err := j.ExpectGen(2); !errors.As(err, &stale) || stale.Have != 3 {
		t.Errorf("an older generation: %v", err)
	}
	j.State = Moved
	if err := j.ExpectGen(3); !errors.As(err, &stale) {
		t.Errorf("a moved Job is nobody's to order: %v", err)
	}
}

func TestOwnedGenLooksInTheHistory(t *testing.T) {
	j := Job{History: []Entry{{Gen: 1, Peer: "a"}, {Gen: 2, Peer: "b"}}}
	if !j.OwnedGen("b", 2) || j.OwnedGen("a", 2) || j.OwnedGen("b", 3) {
		t.Error("OwnedGen must match peer and generation")
	}
}

func TestAJobThatDoesNotExistIsCreatedUnderTheLockAndDroppedOnRequest(t *testing.T) {
	s := newTestStore(t)
	id := "job-abcdefghij"
	err := s.WithLockNew(id, func(j *Job, exists bool, _ func()) error {
		if exists {
			t.Error("it does not exist yet")
		}
		j.Repo, j.Gen, j.State = "r", 2, Stopped
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(id); err != nil || got.Gen != 2 {
		t.Fatalf("created: %+v %v", got, err)
	}
	if err := s.WithLockNew(id, func(_ *Job, exists bool, drop func()) error {
		if !exists {
			t.Error("it exists now")
		}
		drop()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dropped: %v", err)
	}
}
