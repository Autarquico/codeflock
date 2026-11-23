package sessions

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
)

const (
	claudeID = "11111111-2222-4333-8444-555555555555"
	codexID  = "01a1b2c3-d4e5-7f60-8a7b-9c0d1e2f3a4b"
	workDir  = "/work/repo"
)

// machine is a HOME of synthetic sessions: a Local that looks in two temporary homes.
type machine struct {
	*Local
	claudeHome, codexHome string
}

func newMachine(t *testing.T) machine {
	t.Helper()
	m := machine{claudeHome: t.TempDir(), codexHome: t.TempDir()}
	m.Local = New()
	m.ConfigDirs = map[string]string{claude.ID: m.claudeHome, codex.ID: m.codexHome}
	m.getenv = func(string) string { return "" }
	return m
}

func claudeLine(id, kind, content string) string {
	return fmt.Sprintf(`{"type":%q,"sessionId":%q,"cwd":%q,"version":"2.1.319","message":{"role":%q,"content":%s}}`,
		kind, id, workDir, kind, content)
}

// claudeBody is a conversation that ends in a tool call, its result and a closing text.
func claudeBody(id string, extra ...string) []string {
	lines := []string{
		claudeLine(id, "user", `"hi"`),
		claudeLine(id, "assistant", `[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"cflk handoff"}}]`),
		claudeLine(id, "user", `[{"type":"tool_result","tool_use_id":"toolu_1","content":"a"}]`),
		claudeLine(id, "assistant", `[{"type":"text","text":"done"}]`),
	}
	return append(lines, extra...)
}

// claudeInFlight is the tool call of cflk handoff once Claude Code has written it: no result follows.

func (m machine) writeClaude(t *testing.T, id string, lines []string) string {
	t.Helper()
	p := filepath.Join(m.claudeHome, "projects", "-work-repo", id+".jsonl")
	writeFile(t, p, strings.Join(lines, "\n")+"\n")
	return p
}

func codexLine(ordinal int, kind, payload string) string {
	return fmt.Sprintf(`{"timestamp":"2026-11-12T09:30:15.%03dZ","ordinal":%d,"type":%q,"payload":%s}`, ordinal, ordinal, kind, payload)
}

func codexBody(id string, extra ...string) []string {
	lines := []string{
		codexLine(1, "session_meta", fmt.Sprintf(`{"session_id":%q,"id":%q,"timestamp":"2026-11-12T09:30:15.120Z","cwd":%q,"cli_version":"0.167.0"}`, id, id, workDir)),
		codexLine(2, "response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":"fix"}]}`),
		codexLine(3, "response_item", `{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}`),
		codexLine(4, "response_item", `{"type":"function_call_output","call_id":"c1","output":"ok"}`),
		codexLine(5, "response_item", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`),
	}
	return append(lines, extra...)
}

func (m machine) writeCodex(t *testing.T, id string, lines []string) string {
	t.Helper()
	p := filepath.Join(m.codexHome, "sessions", "2026", "11", "12", "rollout-2026-11-12T09-30-15-"+id+".jsonl")
	writeFile(t, p, strings.Join(lines, "\n")+"\n")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // a test path
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (m machine) export(t *testing.T, id string) Export {
	t.Helper()
	e, err := m.Export(t.Context(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestFindLooksInEveryHarnessAndSaysWhichHasIt(t *testing.T) {
	m := newMachine(t)
	m.writeClaude(t, claudeID, claudeBody(claudeID))
	m.writeCodex(t, codexID, codexBody(codexID))

	if ref, err := m.Find(t.Context(), claudeID, ""); err != nil || ref.Adapter != claude.ID || ref.Cwd != workDir {
		t.Fatalf("claude: %+v %v", ref, err)
	}
	if ref, err := m.Find(t.Context(), codexID, ""); err != nil || ref.Adapter != codex.ID || ref.Cwd != workDir {
		t.Fatalf("codex: %+v %v", ref, err)
	}
	if _, err := m.Find(t.Context(), "99999999-2222-4333-8444-555555555555", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a session nobody has: %v", err)
	}
	if _, err := m.Find(t.Context(), "../etc/passwd", ""); err == nil {
		t.Fatal("an id that is a path was accepted")
	}
}

func TestExportIsTheSameForTheSameSession(t *testing.T) {
	m := newMachine(t)
	m.writeClaude(t, claudeID, claudeBody(claudeID))
	if a, b := m.export(t, claudeID), m.export(t, claudeID); a.Digest != b.Digest || !strings.HasPrefix(a.Digest, "sha256:") {
		t.Fatalf("digests %q and %q", a.Digest, b.Digest)
	}
}

// Every import takes a new id: a Job that returns to a peer where it already was (A to B to A)
// finds its old transcript as it left it, and the session that comes in is beside it.
func TestImportAlwaysInstallsUnderANewIDAndLeavesTheOldTranscriptAlone(t *testing.T) {
	for _, adapter := range []string{claude.ID, codex.ID} {
		t.Run(adapter, func(t *testing.T) {
			m := newMachine(t)
			var old, oldID string
			if adapter == claude.ID {
				oldID, old = claudeID, m.writeClaude(t, claudeID, claudeBody(claudeID))
			} else {
				oldID, old = codexID, m.writeCodex(t, codexID, codexBody(codexID))
			}
			before := readFile(t, old)
			archive := m.export(t, oldID).Archive
			for range 2 {
				res, err := m.Import(t.Context(), archive, workDir)
				if err != nil || res.SessionID == oldID || res.Adapter != adapter {
					t.Fatalf("%+v %v", res, err)
				}
				if !strings.Contains(readFile(t, res.Transcript), res.SessionID) {
					t.Error("the transcript does not say its new id")
				}
				if res.Transcript == old {
					t.Fatal("the old transcript was the destination")
				}
			}
			if readFile(t, old) != before {
				t.Error("the old transcript was touched")
			}
			if _, err := os.Stat(old + ".bak"); !os.IsNotExist(err) {
				t.Error("a .bak was left")
			}
		})
	}
}

func TestImportRefusesWhatIsNotAnArchive(t *testing.T) {
	m := newMachine(t)
	if _, err := m.Import(t.Context(), []byte("nope"), workDir); err == nil {
		t.Fatal("garbage was imported")
	}
}

func TestFindClaudeTakesTheTranscriptWrittenLast(t *testing.T) {
	dir := t.TempDir()
	const id = "11111111-2222-4333-8444-555555555555"
	older := filepath.Join(dir, "projects", "-repo", id+".jsonl")
	newer := filepath.Join(dir, "projects", "-repo-worktree", id+".jsonl")
	for path, cwd := range map[string]string{older: "/repo", newer: "/repo-worktree"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"type":"user","cwd":"`+cwd+`"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}
	path, cwd, found, err := findClaude(dir, id)
	if err != nil || !found || path != newer || cwd != "/repo-worktree" {
		t.Fatalf("findClaude = %q, %q, %v, %v; want the worktree's transcript", path, cwd, found, err)
	}
}

// The id in the manifest of a downloaded archive is not trusted to build a path.
func TestImportRefusesAManifestWhoseSessionIDIsAPath(t *testing.T) {
	m := newMachine(t)
	var buf bytes.Buffer
	manifest := harness.SessionManifest{Adapter: claude.ID, SessionID: "../../../elsewhere/x", Cwd: workDir, Lines: 1}
	if err := harness.WriteSessionArchive(&buf, manifest, []byte("{}\n"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Import(t.Context(), buf.Bytes(), workDir); err == nil {
		t.Fatal("a manifest with a path as its session id was accepted")
	}
}
