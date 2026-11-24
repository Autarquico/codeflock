package job

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Store { return NewStore(t.TempDir()) }

func sample() Job {
	return Job{Repo: "github.com/acme/widgets", Harness: "claude", Gen: 1, Peer: "node-a", State: Running, CreatedBy: "node-a"}
}

func TestCreateAndGet(t *testing.T) {
	s := newTestStore(t)
	j, err := s.Create(sample())
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(j.ID) || j.Branch != "cflk/"+j.ID {
		t.Fatalf("id/branch: %+v", j)
	}
	got, err := s.Get(j.ID)
	if err != nil || got.Repo != j.Repo || got.State != Running || got.Gen != 1 {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := s.Get(NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job: %v", err)
	}
}

func TestCreateRegeneratesOnCollision(t *testing.T) {
	s := newTestStore(t)
	first, err := s.Create(sample())
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{first.ID, first.ID, "job-" + "aaaaaaaaaa"}
	s.newID = func() string { id := ids[0]; ids = ids[1:]; return id }
	second, err := s.Create(sample())
	if err != nil || second.ID != "job-aaaaaaaaaa" || len(ids) != 0 {
		t.Fatalf("second = %+v, %v, left %v", second, err, ids)
	}
	if got, _ := s.Get(first.ID); got.ID != first.ID {
		t.Fatal("the first job was disturbed")
	}
}

func TestInvalidIDNeverTouchesDisk(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"../etc", "job-abcdefghij/../..", ""} {
		if _, err := s.Get(id); err == nil {
			t.Errorf("Get(%q) succeeded", id)
		}
		if err := s.WithLock(id, func(*Job) error { return nil }); err == nil {
			t.Errorf("WithLock(%q) succeeded", id)
		}
	}
}

func TestWithLockPersistsChangesOnlyOnSuccess(t *testing.T) {
	s := newTestStore(t)
	j, _ := s.Create(sample())

	boom := errors.New("boom")
	err := s.WithLock(j.ID, func(j *Job) error { j.State = Stopping; return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := s.Get(j.ID); got.State != Running {
		t.Fatalf("failed fn was persisted: %s", got.State)
	}

	err = s.WithLock(j.ID, func(j *Job) error {
		j.Launch = &Launch{ID: "abcdefgh", Session: "cflk-x"}
		return j.Transition(Stopping)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(j.ID)
	if got.State != Stopping || got.Launch == nil || got.Launch.ID != "abcdefgh" {
		t.Fatalf("not persisted: %+v", got)
	}
}

func TestWithLockMissingJob(t *testing.T) {
	s := newTestStore(t)
	if err := s.WithLock(NewID(), func(*Job) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestWithLockSerializesWriters(t *testing.T) {
	s := newTestStore(t)
	j, _ := s.Create(sample())
	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			if err := s.WithLock(j.ID, func(j *Job) error { j.Gen++; return nil }); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got, _ := s.Get(j.ID); got.Gen != 1+n {
		t.Fatalf("Gen = %d, want %d (lost updates)", got.Gen, 1+n)
	}
}

func TestListSkipsHalfCreatedAndSorts(t *testing.T) {
	s := newTestStore(t)
	if jobs, err := s.List(); err != nil || len(jobs) != 0 {
		t.Fatalf("empty store: %v, %v", jobs, err)
	}
	a, _ := s.Create(sample())
	b, _ := s.Create(sample())
	if err := os.Mkdir(s.Dir(NewID()), 0o700); err != nil { // creation in progress
		t.Fatal(err)
	}
	jobs, err := s.List()
	if err != nil || len(jobs) != 2 || jobs[0].ID > jobs[1].ID {
		t.Fatalf("List = %+v, %v (a=%s b=%s)", jobs, err, a.ID, b.ID)
	}
}

func TestPaths(t *testing.T) {
	s := NewStore("/state")
	id, l := "job-abcdefghij", "abcdefgh"
	for got, want := range map[string]string{
		s.Dir(id):            "/state/jobs/job-abcdefghij",
		s.WorktreeDir(id, 3): "/state/jobs/job-abcdefghij/g3",
		s.StartedPath(id, l): "/state/jobs/job-abcdefghij/launches/abcdefgh/started.json",
		s.ExitPath(id, l):    "/state/jobs/job-abcdefghij/launches/abcdefgh/exit.json",
		s.WaitingPath(id, l): "/state/jobs/job-abcdefghij/launches/abcdefgh/waiting",
	} {
		if filepath.ToSlash(got) != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

func TestDefaultRoot(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	root, err := DefaultRoot()
	if err != nil || filepath.ToSlash(root) != "/home/u/.local/state/cflk" {
		t.Fatalf("DefaultRoot = %q, %v", root, err)
	}
}

func TestWithLockSaveKeepsWhatWasSavedWhenFnFailsLater(t *testing.T) {
	s := newTestStore(t)
	j, err := s.Create(sample())
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err = s.WithLockSave(j.ID, func(j *Job, save func() error) error {
		j.State = Waiting
		if err := save(); err != nil {
			return err
		}
		j.State = Done // not saved: fn fails
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := s.Get(j.ID); got.State != Waiting {
		t.Errorf("state = %s, want the saved waiting", got.State)
	}
}

func TestWithLockSaveWritesBackWhatFnRestoredAfterSaving(t *testing.T) {
	s := newTestStore(t)
	j, err := s.Create(sample())
	if err != nil {
		t.Fatal(err)
	}
	err = s.WithLockSave(j.ID, func(j *Job, save func() error) error {
		before := *j
		j.State = Waiting
		if err := save(); err != nil {
			return err
		}
		*j = before // back to what it was: the disk must follow
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(j.ID); got.State != Running {
		t.Errorf("state = %s, want running", got.State)
	}
}

func TestCreateAsTakesTheIdItIsGivenAndRefusesATakenOne(t *testing.T) {
	s := NewStore(t.TempDir())
	j, err := s.CreateAs("job-abcdefghij", Job{State: Stopped})
	if err != nil || j.ID != "job-abcdefghij" || j.Branch != "cflk/job-abcdefghij" {
		t.Fatalf("CreateAs = %+v, %v", j, err)
	}
	if _, err := s.CreateAs("job-abcdefghij", Job{}); !errors.Is(err, ErrExists) {
		t.Errorf("a taken id: %v", err)
	}
	if _, err := s.CreateAs("../x", Job{}); err == nil {
		t.Error("an invalid id was accepted")
	}
}
