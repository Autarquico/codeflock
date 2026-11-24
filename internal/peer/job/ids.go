package job

import (
	"crypto/rand"
	"fmt"
	"regexp"
)

const (
	idPrefix      = "job-"
	idLen         = 10
	launchIDLen   = 8
	transferIDLen = 16
	base32Lower   = "abcdefghijklmnopqrstuvwxyz234567"
	sessionPrefix = "cflk-"
)

var (
	idPattern       = regexp.MustCompile(`^job-[a-z2-7]{10}$`)
	launchIDPattern = regexp.MustCompile(`^[a-z2-7]{8}$`)
	transferPattern = regexp.MustCompile(`^[a-z2-7]{16}$`)
)

// NewID returns a random Job id, job-<10 base32 characters>.
func NewID() string { return idPrefix + randomBase32(idLen) }

// ValidID reports whether id is a well-formed Job id.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// NewLaunchID returns a random launch id, 8 base32 characters.
func NewLaunchID() string { return randomBase32(launchIDLen) }

// ValidLaunchID reports whether id is a well-formed launch id.
func ValidLaunchID(id string) bool { return launchIDPattern.MatchString(id) }

// NewTransferID returns the random id of a movement of a Job (or of a finish).
func NewTransferID() string { return randomBase32(transferIDLen) }

// ValidTransferID reports whether id is a well-formed transfer id.
func ValidTransferID(id string) bool { return transferPattern.MatchString(id) }

// Launch is the reference to one start of the agent. Its records live in
// jobs/<id>/launches/<launch>/ (see Store.LaunchDir).
type Launch struct {
	ID      string `json:"id"`
	Session string `json:"session"` // tmux session name
}

// NewLaunch creates the reference of a fresh launch of the Job jobID.
func NewLaunch(jobID string) Launch {
	id := NewLaunchID()
	return Launch{ID: id, Session: SessionName(jobID, id)}
}

// SessionName is the tmux session of a launch: cflk-<job>-<launch>.
func SessionName(jobID, launchID string) string {
	return fmt.Sprintf("%s%s-%s", sessionPrefix, jobID, launchID)
}

func randomBase32(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("job: no randomness: %v", err))
	}
	for i := range b {
		b[i] = base32Lower[b[i]&31] // 32 divides 256: no bias
	}
	return string(b)
}
