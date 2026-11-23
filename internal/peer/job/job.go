// Package job is the local model of a Job on one peer: its record, its
// identifiers, its state machine and the store that keeps it on disk.
package job

import (
	"errors"
	"fmt"
)

// State is where a Job is in its life on this peer.
type State string

// The states of a Job, exactly as the p2p-peers spec lists them.
const (
	Running   State = "running"
	Waiting   State = "waiting"   // waiting for a person
	Stopping  State = "stopping"  // being stopped; a process may remain
	Stopped   State = "stopped"   //
	Done      State = "done"      //
	Failed    State = "failed"    //
	Lost      State = "lost"      // no process and no exit record, e.g. after a reboot
	Moving    State = "moving"    // transfer in progress or unconfirmed
	Finishing State = "finishing" // a `pull --finish` in progress
	Moved     State = "moved"     // lives on another peer
)

// Valid reports whether s is one of the spec's states.
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// Job is the record kept in jobs/<id>/job.json.
type Job struct {
	ID        string  `json:"id"`
	Repo      string  `json:"repo"` // normalized host/owner/repo
	Branch    string  `json:"branch"`
	Harness   string  `json:"harness"`
	SessionID string  `json:"session_id,omitempty"`
	Gen       int     `json:"gen"`
	Peer      string  `json:"peer"`
	State     State   `json:"state"`
	CreatedBy string  `json:"created_by"`
	History   []Entry `json:"history,omitempty"`

	Launch  *Launch   `json:"launch,omitempty"`   // the current launch
	Move    *Move     `json:"move,omitempty"`     // set while Moving
	Finish  *Finish   `json:"finish,omitempty"`   // set while Finishing
	MovedTo *Location `json:"moved_to,omitempty"` // set when Moved
}

// Entry is one ownership step of the Job.
type Entry struct {
	Gen    int    `json:"gen"`
	Peer   string `json:"peer"`
	Commit string `json:"commit,omitempty"`
}

// Move is the reservation a source peer writes before transferring the Job.
type Move struct {
	To       string `json:"to"`
	Gen      int    `json:"gen"`
	Transfer string `json:"transfer"`
	Commit   string `json:"commit"`
}

// Finish is the reservation written by `pull --finish`.
type Finish struct {
	ID     string `json:"finish"`
	Commit string `json:"commit"`
}

// Location says where a moved Job lives now.
type Location struct {
	Peer string `json:"peer"`
	Gen  int    `json:"gen"`
}

// ErrInvalidTransition is what a change of state the spec does not allow wraps.
var ErrInvalidTransition = errors.New("not allowed in that state")

// Transition changes the state of j, refusing the changes the spec does not allow.
func (j *Job) Transition(to State) error {
	if !CanTransition(j.State, to) {
		return fmt.Errorf("job %s is %s, it cannot become %s: %w", j.ID, j.State, to, ErrInvalidTransition)
	}
	j.State = to
	return nil
}

// GenDir is the worktree directory name of a generation.
func GenDir(gen int) string { return fmt.Sprintf("g%d", gen) }
