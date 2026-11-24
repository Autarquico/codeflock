package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Autarquico/codeflock/internal/support/atomicfile"
	"github.com/Autarquico/codeflock/internal/support/flock"
	"github.com/Autarquico/codeflock/internal/worker/execwrap"
)

// ErrNotFound is returned when a Job has no directory in the store.
var ErrNotFound = errors.New("job: not found")

// ErrExists is returned when the id asked for already has a directory.
var ErrExists = errors.New("job: id already exists")

// Store keeps the Jobs of this peer under <root>/jobs/<id>/.
type Store struct {
	root  string
	newID func() string
}

// NewStore returns a store rooted at root (~/.local/state/cflk by default, see DefaultRoot).
func NewStore(root string) *Store { return &Store{root: root, newID: NewID} }

// DefaultRoot is ~/.local/state/cflk.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("job: home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "cflk"), nil
}

// Dir is the directory of a Job.
func (s *Store) Dir(id string) string { return filepath.Join(s.root, "jobs", id) }

// WorktreeDir is the worktree of one generation of a Job: jobs/<id>/g<gen>.
func (s *Store) WorktreeDir(id string, gen int) string { return filepath.Join(s.Dir(id), GenDir(gen)) }

// LaunchDir holds the records of one launch: started, exit and the waiting marker.
func (s *Store) LaunchDir(id, launchID string) string {
	return filepath.Join(s.Dir(id), "launches", launchID)
}

// StartedPath is the wrapper's record of the launch's process: the file execwrap writes.
func (s *Store) StartedPath(id, launchID string) string {
	return filepath.Join(s.LaunchDir(id, launchID), execwrap.StartedFile)
}

// ExitPath is the wrapper's record of how the launch ended.
func (s *Store) ExitPath(id, launchID string) string {
	return filepath.Join(s.LaunchDir(id, launchID), execwrap.ExitFile)
}

// WaitingPath is the marker that says the agent waits for a person.
func (s *Store) WaitingPath(id, launchID string) string {
	return filepath.Join(s.LaunchDir(id, launchID), "waiting")
}

func (s *Store) jobFile(id string) string  { return filepath.Join(s.Dir(id), "job.json") }
func (s *Store) lockFile(id string) string { return filepath.Join(s.Dir(id), "lock") }

// Create gives j a fresh id and its branch cflk/<id>, creates its directory
// exclusively (a collision draws another id) and writes job.json.
func (s *Store) Create(j Job) (Job, error) {
	if err := os.MkdirAll(filepath.Join(s.root, "jobs"), 0o700); err != nil {
		return Job{}, fmt.Errorf("job: creating state root: %w", err)
	}
	for {
		created, err := s.create(j, s.newID())
		if errors.Is(err, ErrExists) {
			continue
		}
		return created, err
	}
}

// CreateAs is Create for an id someone else drew (a Job delegated by another
// peer, whose code already travels under that id). An id that is taken is
// ErrExists: the caller draws another.
func (s *Store) CreateAs(id string, j Job) (Job, error) {
	if !ValidID(id) {
		return Job{}, fmt.Errorf("job: invalid id %q", id)
	}
	if err := os.MkdirAll(filepath.Join(s.root, "jobs"), 0o700); err != nil {
		return Job{}, fmt.Errorf("job: creating state root: %w", err)
	}
	return s.create(j, id)
}

func (s *Store) create(j Job, id string) (Job, error) {
	err := os.Mkdir(s.Dir(id), 0o700)
	if errors.Is(err, os.ErrExist) {
		return Job{}, ErrExists
	}
	if err != nil {
		return Job{}, fmt.Errorf("job: creating %s: %w", id, err)
	}
	j.ID, j.Branch = id, "cflk/"+id
	if err := s.write(j); err != nil {
		_ = os.RemoveAll(s.Dir(id))
		return Job{}, err
	}
	return j, nil
}

// LockStart takes the lock that serializes the start of the Job with this id,
// whoever asks, and blocks until it has it. It is not the Job's lock: it exists
// before the Job does, and is held across the creation, the worktree and the launch.
func (s *Store) LockStart(id string) (release func(), err error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("job: invalid id %q", id)
	}
	if err := os.MkdirAll(filepath.Join(s.root, "jobs"), 0o700); err != nil {
		return nil, fmt.Errorf("job: creating state root: %w", err)
	}
	return flock.Lock(filepath.Join(s.root, "jobs", id+".start"))
}

// Get reads a Job. Callers that mean to change it use WithLock.
func (s *Store) Get(id string) (Job, error) {
	if !ValidID(id) {
		return Job{}, fmt.Errorf("job: invalid id %q", id)
	}
	return s.read(id)
}

// List returns every readable Job in id order. A directory without job.json
// (a creation in progress) is skipped.
func (s *Store) List() ([]Job, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "jobs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("job: listing jobs: %w", err)
	}
	var jobs []Job
	for _, e := range entries {
		if !ValidID(e.Name()) {
			continue
		}
		j, err := s.read(e.Name())
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].ID < jobs[b].ID })
	return jobs, nil
}

// WithLock takes the Job's exclusive lock, reads it, runs fn and, if fn
// succeeded and changed the Job, writes it back before releasing the lock.
// Every check and change of a Job belongs inside one WithLock.
func (s *Store) WithLock(id string, fn func(*Job) error) error {
	return s.WithLockSave(id, func(j *Job, _ func() error) error { return fn(j) })
}

// WithLockSave is WithLock for a change that must reach the disk before it is
// finished: fn may call save to write the Job as it is now, while still holding
// the lock (a launch records itself before it starts anything). What fn leaves
// unsaved is written when it succeeds; when it fails, the disk keeps the last save.
func (s *Store) WithLockSave(id string, fn func(j *Job, save func() error) error) error {
	if !ValidID(id) {
		return fmt.Errorf("job: invalid id %q", id)
	}
	if _, err := os.Stat(s.Dir(id)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return fmt.Errorf("job: %s: %w", id, err)
	}
	release, err := flock.Lock(s.lockFile(id))
	if err != nil {
		return err
	}
	defer release()

	j, err := s.read(id)
	if err != nil {
		return err
	}
	onDisk, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("job: encoding %s: %w", id, err)
	}
	save := func() error {
		now, err := json.Marshal(j)
		if err != nil {
			return fmt.Errorf("job: encoding %s: %w", id, err)
		}
		if string(now) == string(onDisk) {
			return nil
		}
		if err := s.write(j); err != nil {
			return err
		}
		onDisk = now
		return nil
	}
	if err := fn(&j, save); err != nil {
		return err
	}
	return save()
}

func (s *Store) read(id string) (Job, error) {
	data, err := os.ReadFile(s.jobFile(id))
	if errors.Is(err, os.ErrNotExist) {
		return Job{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Job{}, fmt.Errorf("job: reading %s: %w", id, err)
	}
	var j Job
	if err := json.Unmarshal(data, &j); err != nil {
		return Job{}, fmt.Errorf("job: decoding %s: %w", id, err)
	}
	if j.ID != id {
		return Job{}, fmt.Errorf("job: %s holds the record of %q", id, j.ID)
	}
	return j, nil
}

func (s *Store) write(j Job) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("job: encoding %s: %w", j.ID, err)
	}
	if err := atomicfile.Write(s.jobFile(j.ID), data, 0o600); err != nil {
		return fmt.Errorf("job: %w", err)
	}
	return nil
}
