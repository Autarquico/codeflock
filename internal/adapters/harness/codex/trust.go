package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/support/atomicfile"
	"github.com/Autarquico/codeflock/internal/support/flock"
)

// configFile is Codex's configuration, in ConfigDir.
const configFile = "config.toml"

const trusted = "trusted"

// TrustFolder marks the worktree as trusted in the config.toml of the host
// ([projects."<path>"] trust_level = "trusted"), so that the first start there does
// not stop at "Trust this folder?". The flag -c projects.<path>.trust_level does not
// stop it. Only that project's table is added; the rest of the file is kept as it is,
// comments included, and a folder that is trusted already is left alone. Codex keys
// the trust by the real path of the folder.
func (Adapter) TrustFolder(host harness.Host, worktree string) error {
	return editTrust(host, worktree, func(text, key string, present bool) (string, bool, error) {
		if present {
			return text, false, nil
		}
		return withProject(text, key), true, nil
	})
}

// UntrustFolder removes the table TrustFolder added for the worktree, and only that
// one. A folder with no entry is left as it is.
func (Adapter) UntrustFolder(host harness.Host, worktree string) error {
	return editTrust(host, worktree, func(text, key string, present bool) (string, bool, error) {
		if !present {
			return text, false, nil
		}
		out, ok := withoutProject(text, key)
		if !ok {
			return text, false, errors.New("the entry is not a table of its own that can be removed")
		}
		return out, true, nil
	})
}

// editTrust applies change to the text of config.toml under the lock every cflk
// write of it holds (next to the file, so whoever shares the file shares the lock),
// checks that the result is valid TOML that says what was meant, and writes it
// atomically with the mode the file had. Codex writes the file too, without the
// lock: a change of its own landing between our read and our rename can be lost.
func editTrust(host harness.Host, worktree string, change func(text, key string, present bool) (string, bool, error)) error {
	key, err := trustKey(worktree)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(ConfigDir(host))
	if err != nil {
		return fmt.Errorf("codex: trusting the folder: %w", err)
	}
	path := filepath.Join(dir, configFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("codex: trusting the folder: %w", err)
	}
	release, err := flock.Lock(path + ".cflk.lock")
	if err != nil {
		return fmt.Errorf("codex: trusting the folder: %w", err)
	}
	defer release()
	perm := os.FileMode(0o600)
	raw, err := os.ReadFile(path) //nolint:gosec // the configuration of the host
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("codex: trusting the folder: %w", err)
	default:
		if info, statErr := os.Stat(path); statErr == nil {
			perm = info.Mode().Perm()
		}
	}
	text := string(raw)
	have, err := trustLevel(text, key)
	if err != nil {
		return fmt.Errorf("codex: trusting the folder: %s: %w", path, err)
	}
	out, changed, err := change(text, key, have != "")
	if err != nil {
		return fmt.Errorf("codex: trusting the folder: %s: %w", path, err)
	}
	if !changed {
		return nil
	}
	if _, err := trustLevel(out, key); err != nil {
		return fmt.Errorf("codex: trusting the folder: the result would not be valid TOML: %w", err)
	}
	if err := atomicfile.Write(path, []byte(out), perm); err != nil {
		return fmt.Errorf("codex: trusting the folder: %w", err)
	}
	return nil
}

func trustKey(worktree string) (string, error) {
	if !filepath.IsAbs(worktree) {
		return "", fmt.Errorf("codex: the worktree %q is not an absolute path", worktree)
	}
	resolved, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return "", fmt.Errorf("codex: resolving the worktree: %w", err)
	}
	if !utf8.ValidString(resolved) || strings.ContainsFunc(resolved, unicode.IsControl) {
		return "", fmt.Errorf("codex: the path %q cannot be a TOML key", resolved)
	}
	return resolved, nil
}

// trustLevel is the trust_level the text gives the project, empty if it gives none.
func trustLevel(text, key string) (string, error) {
	var doc struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
	}
	if err := toml.Unmarshal([]byte(text), &doc); err != nil {
		return "", err
	}
	return doc.Projects[key].TrustLevel, nil
}

var basicString = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

func header(key string) string { return `[projects."` + basicString.Replace(key) + `"]` }

// withProject is the text with the table of the project at its end.
func withProject(text, key string) string {
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if text != "" && !strings.HasSuffix(text, "\n\n") {
		text += "\n"
	}
	return text + header(key) + "\ntrust_level = \"" + trusted + "\"\n"
}

// withoutProject is the text without the table [projects."<key>"], which runs
// from its header to the line before the next one (the blank lines that precede
// that one stay). It reports false when the table is not there in that form.
func withoutProject(text, key string) (string, bool) {
	lines := strings.SplitAfter(text, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == header(key) {
			start = i
			break
		}
	}
	if start < 0 {
		return text, false
	}
	end := start + 1
	for end < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[end]), "[") {
		end++
	}
	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	if end == len(lines) || strings.TrimSpace(strings.Join(lines[end:], "")) == "" { // the table was the last: take the blank line that set it apart
		for start > 0 && strings.TrimSpace(lines[start-1]) == "" {
			start--
		}
		end = len(lines)
	}
	return strings.Join(lines[:start], "") + strings.Join(lines[end:], ""), true
}
