package codex_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/Autarquico/codeflock/internal/adapters/harness/codex"
	"github.com/Autarquico/codeflock/internal/adapters/harness/localhost"
)

const userConfig = `# my codex
model = "gpt-5"

[projects."/Users/me/src/app"]
trust_level = "trusted"   # keep

[mcp_servers.docs]
command = "docs"
`

func trustHost(t *testing.T, config string) (home, file string, host *localhost.Host) {
	t.Helper()
	home = t.TempDir()
	file = filepath.Join(home, ".codex", "config.toml")
	if config != "" {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(config), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	h, err := localhost.New(localhost.Options{Home: home, Env: []string{"PATH=" + t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	return home, file, h
}

func trustOf(t *testing.T, file, project string) string {
	t.Helper()
	raw, err := os.ReadFile(file) //nolint:gosec // a test's file
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		}
	}
	if err := toml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("not TOML: %v\n%s", err, raw)
	}
	return doc.Projects[project].TrustLevel
}

func TestTrustFolderAddsOnlyThatProjectAndKeepsTheFileAsItWas(t *testing.T) {
	_, file, host := trustHost(t, userConfig)
	wt := t.TempDir()
	key, _ := filepath.EvalSymlinks(wt)

	if err := (codex.Adapter{}).TrustFolder(host, wt); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(file) //nolint:gosec // a test's file
	if !strings.HasPrefix(string(got), userConfig) {
		t.Fatalf("what was there changed:\n%s", got)
	}
	if trustOf(t, file, key) != "trusted" || trustOf(t, file, "/Users/me/src/app") != "trusted" {
		t.Fatalf("trust = %s", got)
	}
	if info, _ := os.Stat(file); info.Mode().Perm() != 0o400 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
}

func TestTrustFolderIsIdempotentAndCreatesTheFileWhenThereIsNone(t *testing.T) {
	_, file, host := trustHost(t, "")
	wt := t.TempDir()
	key, _ := filepath.EvalSymlinks(wt)
	for range 3 {
		if err := (codex.Adapter{}).TrustFolder(host, wt); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(file) //nolint:gosec // a test's file
	if strings.Count(string(raw), "trust_level") != 1 || trustOf(t, file, key) != "trusted" {
		t.Fatalf("file = %s", raw)
	}
	if info, _ := os.Stat(file); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
}

func TestUntrustFolderRemovesOnlyItsTableAndRestoresTheFile(t *testing.T) {
	for name, config := range map[string]string{
		"a file":                      userConfig,
		"a file whose end is a table": userConfig + "\n[tui]\nnotifications = true\n",
		"no file":                     "",
	} {
		t.Run(name, func(t *testing.T) {
			_, file, host := trustHost(t, config)
			wt, other := t.TempDir(), t.TempDir()
			otherReal, _ := filepath.EvalSymlinks(other)
			if err := (codex.Adapter{}).TrustFolder(host, wt); err != nil {
				t.Fatal(err)
			}
			if err := (codex.Adapter{}).TrustFolder(host, other); err != nil {
				t.Fatal(err)
			}
			if err := (codex.Adapter{}).UntrustFolder(host, wt); err != nil {
				t.Fatal(err)
			}
			key, _ := filepath.EvalSymlinks(wt)
			if trustOf(t, file, key) != "" || trustOf(t, file, otherReal) != "trusted" {
				t.Fatalf("after the removal: %s", mustRead(t, file))
			}
			if err := (codex.Adapter{}).UntrustFolder(host, other); err != nil {
				t.Fatal(err)
			}
			if got := mustRead(t, file); got != config {
				t.Fatalf("not restored:\n%q\nwas\n%q", got, config)
			}
			if err := (codex.Adapter{}).UntrustFolder(host, wt); err != nil { // no entry: left as it is
				t.Fatal(err)
			}
		})
	}
}

func mustRead(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file) //nolint:gosec // a test's file
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestTrustFolderRefusesAFileThatIsNotTOMLAndLeavesItAlone(t *testing.T) {
	_, file, host := trustHost(t, "model = \n")
	if err := (codex.Adapter{}).TrustFolder(host, t.TempDir()); err == nil {
		t.Fatal("a broken file was edited")
	}
	if got := mustRead(t, file); got != "model = \n" {
		t.Fatalf("file = %q", got)
	}
	if err := (codex.Adapter{}).TrustFolder(host, "relative/path"); err == nil {
		t.Fatal("a relative path was trusted")
	}
}

func TestTrustFolderKeepsEveryEntryWhenManyJobsStartAtOnce(t *testing.T) {
	_, file, host := trustHost(t, userConfig)
	var wg sync.WaitGroup
	trees := make([]string, 8)
	for i := range trees {
		trees[i] = t.TempDir()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := (codex.Adapter{}).TrustFolder(host, trees[i]); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for _, wt := range trees {
		key, _ := filepath.EvalSymlinks(wt)
		if trustOf(t, file, key) != "trusted" {
			t.Fatalf("%s lost:\n%s", wt, mustRead(t, file))
		}
	}
}

func TestTrustFolderEscapesAPathWithQuotesAndBackslashes(t *testing.T) {
	_, file, host := trustHost(t, "")
	wt := filepath.Join(t.TempDir(), `we"ird\dir`)
	if err := os.Mkdir(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	key, _ := filepath.EvalSymlinks(wt)
	if err := (codex.Adapter{}).TrustFolder(host, wt); err != nil {
		t.Fatal(err)
	}
	if trustOf(t, file, key) != "trusted" {
		t.Fatalf("file = %s", mustRead(t, file))
	}
}

func TestTrustingThroughASymlinkedConfigEditsTheTargetAndKeepsTheLink(t *testing.T) {
	home, file, host := trustHost(t, "")
	target := filepath.Join(t.TempDir(), "dotfiles", "codex.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(userConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(home, "wt")
	if err := os.Mkdir(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (codex.Adapter{}).TrustFolder(host, wt); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(file); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", info, err)
	}
	resolved, err := filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatal(err)
	}
	if got := trustOf(t, target, resolved); got == "" {
		t.Fatal("the target was not edited")
	}
}
