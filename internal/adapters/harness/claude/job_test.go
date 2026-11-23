package claude_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
	"github.com/Autarquico/codeflock/internal/adapters/harness/claude"
	"github.com/Autarquico/codeflock/internal/adapters/harness/localhost"
	"github.com/Autarquico/codeflock/internal/domain"
)

func jobHost(t *testing.T) (*localhost.Host, string) {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a stub binary
		t.Fatal(err)
	}
	h, err := localhost.New(localhost.Options{Home: home, Env: []string{"PATH=" + bin}})
	if err != nil {
		t.Fatal(err)
	}
	return h, home
}

func TestStartSessionNamesTheSessionAndKeepsTheHostsConfiguration(t *testing.T) {
	h, home := jobHost(t)
	wt := t.TempDir()
	run := harness.RunSpec{Mode: domain.ModeInteractive, Prompt: "fix it", Worktree: wt, ConfigDir: filepath.Join(home, ".claude")}
	const id = "3b9a610c-7d42-4e58-9f1e-c40de5fab271"
	l, err := claude.Adapter{}.StartSession(t.Context(), h, run, id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(l.Argv, "--session-id="+id) || l.Argv[len(l.Argv)-1] != "fix it" {
		t.Errorf("argv = %v", l.Argv)
	}
	if len(l.Env) != 0 || len(l.SecretEnv) != 0 {
		t.Errorf("env %v, secrets %v: the host's own login is used", l.Env, l.SecretEnv)
	}
	run.Prompt = ""
	if l, err = (claude.Adapter{}).StartSession(t.Context(), h, run, id); err != nil || slices.Contains(l.Argv, "--") {
		t.Errorf("an empty prompt is a start without one: %v, %v", l.Argv, err)
	}
}

func TestTrustFolderSetsOnlyThatProject(t *testing.T) {
	h, home := jobHost(t)
	state := filepath.Join(home, claude.StateFile)
	if err := os.WriteFile(state, []byte(`{"theme":"dark","projects":{"/other":{"hasTrustDialogAccepted":false,"x":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	if err := (claude.Adapter{}).TrustFolder(h, wt); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(state) //nolint:gosec // a file of the test
	var got struct {
		Theme    string                    `json:"theme"`
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(wt)
	if got.Theme != "dark" || got.Projects[resolved]["hasTrustDialogAccepted"] != true || got.Projects["/other"]["hasTrustDialogAccepted"] != false || got.Projects["/other"]["x"] == nil {
		t.Errorf("state = %s", raw)
	}
	if err := (claude.Adapter{}).TrustFolder(h, "relative"); err == nil {
		t.Error("a relative path is refused")
	}
}

func TestUntrustFolderRemovesOnlyThatProject(t *testing.T) {
	h, home := jobHost(t)
	state := filepath.Join(home, claude.StateFile)
	wt := t.TempDir()
	if err := os.WriteFile(state, []byte(`{"theme":"dark","projects":{"/other":{"x":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a := claude.Adapter{}
	if err := a.TrustFolder(h, wt); err != nil {
		t.Fatal(err)
	}
	if err := a.UntrustFolder(h, wt); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(state) //nolint:gosec // a file of the test
	var got struct {
		Theme    string                    `json:"theme"`
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if resolved, _ := filepath.EvalSymlinks(wt); got.Theme != "dark" || len(got.Projects) != 1 || got.Projects["/other"]["x"] == nil {
		t.Errorf("state = %s (the entry of %s should be gone and the rest kept)", raw, resolved)
	}
}

func TestUntrustFolderWithoutStateDoesNotCreateIt(t *testing.T) {
	h, home := jobHost(t)
	if err := (claude.Adapter{}).UntrustFolder(h, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, claude.StateFile)); err == nil {
		t.Error("untrusting created the state file")
	}
}

func TestConcurrentTrustsKeepEveryEntry(t *testing.T) {
	h, home := jobHost(t)
	const n = 24
	var wg sync.WaitGroup
	paths := make([]string, n)
	for i := range paths {
		paths[i] = t.TempDir()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := (claude.Adapter{}).TrustFolder(h, paths[i]); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	raw, _ := os.ReadFile(filepath.Join(home, claude.StateFile)) //nolint:gosec // a file of the test
	var got struct {
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		resolved, _ := filepath.EvalSymlinks(p)
		if got.Projects[resolved]["hasTrustDialogAccepted"] != true {
			t.Errorf("the trust of %s was lost", resolved)
		}
	}
}

func TestTrustFolderWritesTheStateOfCLAUDE_CONFIG_DIRWhenTheHostSetsIt(t *testing.T) {
	home, cfg := t.TempDir(), t.TempDir()
	h, err := localhost.New(localhost.Options{Home: home, Env: []string{claude.ConfigHomeEnv + "=" + cfg}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (claude.Adapter{}).TrustFolder(h, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg, claude.StateFile)); err != nil {
		t.Errorf("no state in the config dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, claude.StateFile)); err == nil {
		t.Error("~/.claude.json was written although the host sets CLAUDE_CONFIG_DIR")
	}
}
