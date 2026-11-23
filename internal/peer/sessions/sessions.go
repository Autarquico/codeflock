// Package sessions moves the native session of a harness (Claude Code under ~/.claude,
// Codex under ~/.codex) out of and into this peer: it finds a session by id, exports it
// as the portable archive of the adapter, and imports an archive that comes from another
// peer. An import always installs the session under a new id, so a Job that comes back
// to a peer where it already was never touches its old transcript.
package sessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
)

// ErrNotFound is a session that none of the harnesses has in its home.
var ErrNotFound = errors.New("sessions: not found")

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{7,63}$`)

// Local is the sessions of this machine. The zero value is not usable: see New.
type Local struct {
	// ConfigDirs replaces the home of a harness (by adapter id): the tests', or a
	// CLAUDE_CONFIG_DIR the environment does not tell.
	ConfigDirs map[string]string

	adapters []access
	getenv   func(string) string
}

// New is the sessions of this machine.
func New() *Local { return &Local{adapters: accesses(), getenv: os.Getenv} }

// Ref is a session found on this machine.
type Ref struct {
	Adapter string
	ID      string
	// Cwd is the directory the session ran in; Transcript the file of the conversation.
	Cwd        string
	Transcript string
}

func (l *Local) access(adapter string) (access, string, error) {
	for _, a := range l.adapters {
		if a.id != adapter {
			continue
		}
		dir := l.ConfigDirs[a.id]
		if dir == "" {
			var err error
			if dir, err = a.home(l.getenv); err != nil {
				return access{}, "", err
			}
		}
		return a, dir, nil
	}
	return access{}, "", fmt.Errorf("sessions: no harness %q keeps sessions here", adapter)
}

// Find locates the session id in the home of the harness named, or of any of them
// if adapter is empty. An id in more than one is an error that asks for the harness.
func (l *Local) Find(ctx context.Context, id, adapter string) (Ref, error) {
	if !safeID.MatchString(id) {
		return Ref{}, fmt.Errorf("sessions: %q is not a session id", id)
	}
	var found []Ref
	for _, a := range l.adapters {
		if adapter != "" && a.id != adapter {
			continue
		}
		if err := ctx.Err(); err != nil {
			return Ref{}, err
		}
		_, dir, err := l.access(a.id)
		if err != nil {
			return Ref{}, err
		}
		path, cwd, ok, err := a.find(dir, id)
		if err != nil {
			return Ref{}, fmt.Errorf("sessions: looking for %s in %s: %w", id, dir, err)
		}
		if ok {
			found = append(found, Ref{Adapter: a.id, ID: id, Cwd: cwd, Transcript: path})
		}
	}
	switch len(found) {
	case 0:
		return Ref{}, fmt.Errorf("%w: no harness on this machine has the session %s", ErrNotFound, id)
	case 1:
		return found[0], nil
	}
	return Ref{}, fmt.Errorf("sessions: %s exists for more than one harness: say which", id)
}

// Export is a session ready to travel.
type Export struct {
	Ref Ref
	// Archive is the portable archive of the adapter; Digest its sha256 as "sha256:<hex>".
	Archive  []byte
	Digest   string
	Manifest harness.SessionManifest
}

// Export finds the session and writes its archive, whole. The caller has stopped the
// Job first: nothing here asks whether the session is open.
func (l *Local) Export(ctx context.Context, id, adapter string) (Export, error) {
	ref, err := l.Find(ctx, id, adapter)
	if err != nil {
		return Export{}, err
	}
	a, dir, err := l.access(ref.Adapter)
	if err != nil {
		return Export{}, err
	}
	var archive bytes.Buffer
	manifest, err := a.porter.Export(ctx, harness.SessionRef{ConfigDir: dir, Cwd: ref.Cwd, SessionID: ref.ID}, harness.ExportOptions{}, &archive)
	if err != nil {
		return Export{}, err
	}
	sum := sha256.Sum256(archive.Bytes())
	return Export{Ref: ref, Archive: archive.Bytes(), Digest: "sha256:" + hex.EncodeToString(sum[:]), Manifest: manifest}, nil
}

// ImportResult is where the session went.
type ImportResult struct {
	Adapter   string
	SessionID string
	// Transcript is the file written.
	Transcript string
}

// Import installs an archive for the checkout at cwd under a new session id, never
// over an existing session.
func (l *Local) Import(ctx context.Context, archive []byte, cwd string) (ImportResult, error) {
	manifest, err := readManifest(archive)
	if err != nil {
		return ImportResult{}, err
	}
	a, dir, err := l.access(manifest.Adapter)
	if err != nil {
		return ImportResult{}, err
	}
	res, err := a.porter.Import(ctx, harness.SessionImport{ConfigDir: dir, Cwd: cwd, NewSessionID: a.newID()}, bytes.NewReader(archive))
	if err != nil {
		return ImportResult{}, err
	}
	return ImportResult{Adapter: a.id, SessionID: res.SessionID, Transcript: res.Transcript}, nil
}
