package runner

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
)

// NewSessionID is a fresh native session id in the format of the harness: a UUIDv4
// for Claude Code, a UUIDv7 for Codex (which refuses a v4 when resuming). Any
// other harness is an error. A Job that is imported takes one of these (F4).
func NewSessionID(harnessID string) (string, error) {
	switch harnessID {
	case claude.ID:
		return format(randomBytes(), 4), nil
	case codex.ID:
		return uuidV7(time.Now()), nil
	}
	return "", fmt.Errorf("runner: no session id format for harness %q", harnessID)
}

func uuidV7(at time.Time) string {
	u := randomBytes()
	ms := uint64(at.UnixMilli()) //nolint:gosec // milliseconds since the epoch are positive
	for i := range 6 {
		u[i] = byte(ms >> (8 * (5 - i)) & 0xff)
	}
	return format(u, 7)
}

func randomBytes() [16]byte {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		panic(fmt.Sprintf("runner: no randomness: %v", err))
	}
	return u
}

func format(u [16]byte, version byte) string {
	u[6] = u[6]&0x0f | version<<4
	u[8] = u[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}
