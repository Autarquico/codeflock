package codex

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
)

// ConfigDir is where the host keeps Codex's sessions and configuration:
// CODEX_HOME, or ~/.codex.
func ConfigDir(host harness.Host) string {
	if env := host.Getenv(ConfigHomeEnv); env != "" {
		return env
	}
	return filepath.Join(host.Home(), ".codex")
}

// StartSession builds the interactive process of a Job: a new session when id is
// empty (Codex has no flag to name it; see DiscoverSessions) and the resume of
// id otherwise. It runs with the configuration and login of the host, so the
// Launch has no Env. The prompt may be empty only for a resume.
func (a Adapter) StartSession(_ context.Context, host harness.Host, run harness.RunSpec, id string) (harness.Launch, error) {
	if err := a.validate(run); err != nil {
		return harness.Launch{}, err
	}
	switch {
	case id != "" && !validSessionID(id):
		return harness.Launch{}, fmt.Errorf("codex: %q is not a session id", id)
	case id == "" && run.Prompt == "":
		return harness.Launch{}, errors.New("codex: the prompt is empty: there is nothing to ask")
	}
	l, err := launch(host, run, id, run.Prompt)
	if err != nil {
		return harness.Launch{}, err
	}
	l.Env = nil
	return l, nil
}

// DiscoverSessions lists the sessions under <configDir>/sessions that were created
// at or after `after` with cwd as their working directory (compared with symlinks
// resolved). A launch that starts one session finds exactly one.
func DiscoverSessions(ctx context.Context, configDir, cwd string, after time.Time) ([]string, error) {
	all, err := rollouts(configDir)
	if err != nil {
		return nil, fmt.Errorf("codex: looking for sessions: %w", err)
	}
	want := realPath(cwd)
	var ids []string
	for _, r := range all {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if info, err := os.Stat(r.path); err != nil || info.ModTime().Before(after) {
			continue // a rollout is written as it grows: one older than `after` cannot be new
		}
		raw, err := readHead(r.path)
		if err != nil {
			continue
		}
		meta, err := readMeta(raw)
		if err != nil || meta.Timestamp.Before(after) || realPath(meta.Cwd) != want {
			continue
		}
		ids = append(ids, r.id)
	}
	return ids, nil
}

// Rollout is the file of the session id, for whoever wants to know when it last grew.
func Rollout(configDir, id string) (string, error) {
	files, err := Adapter{}.Locate(context.Background(), harness.SessionRef{ConfigDir: configDir, SessionID: id})
	return files.Transcript, err
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// readHead reads the first line of a rollout, where its session_meta is.
func readHead(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // a rollout of the host
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReaderSize(io.LimitReader(f, maxMetaLine), 64<<10).ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return nil, err
	}
	return line, nil
}

// maxMetaLine bounds how much of a rollout is read to find its session_meta.
const maxMetaLine = 4 << 20
