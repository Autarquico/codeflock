package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/mcp"
)

// withFakeClaude puts a `claude` on the PATH and a home to write skills into.
func withFakeClaude(t *testing.T) (home string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a fake program for the test
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	home = t.TempDir()
	t.Setenv("HOME", home)
	return home
}

type recorded struct{ calls [][]string }

func (r *recorded) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return nil, nil
}

func TestInstallRegistersTheServerAndWritesTheSkillOnce(t *testing.T) {
	home := withFakeClaude(t)
	var r recorded

	res, err := installMCP(t.Context(), r.run, claudeClient, "user", mcp.Read, false)
	if err != nil {
		t.Fatal(err)
	}
	add := r.calls[0]
	if got := strings.Join(add[:6], " "); got != "claude mcp add --scope user cflk" {
		t.Errorf("add = %v", add)
	}
	if tail := strings.Join(add[len(add)-4:], " "); tail != "mcp serve --profile read" {
		t.Errorf("add = %v", add)
	}
	if len(r.calls) != 1 || r.calls[0][2] != "add" {
		t.Errorf("the registration is added first, and only that: %v", r.calls)
	}
	skill := filepath.Join(home, ".claude", "skills", "codeflock", "SKILL.md")
	if res.Skill.State != "written" || res.Skill.Path != skill {
		t.Errorf("skill = %+v", res.Skill)
	}
	if b, err := os.ReadFile(skill); err != nil || !strings.Contains(string(b), "cflk-version: ") || strings.Contains(string(b), "{{") { //nolint:gosec // test file
		t.Errorf("skill not rendered: %v %q", err, b)
	}

	if res, err = installMCP(t.Context(), r.run, claudeClient, "user", mcp.Read, false); err != nil || res.Skill.State != "current" {
		t.Errorf("second install: %v %+v", err, res.Skill)
	}
}

func TestInstallDryRunChangesNothing(t *testing.T) {
	home := withFakeClaude(t)
	var r recorded
	res, err := installMCP(t.Context(), r.run, claudeClient, "user", mcp.Write, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 0 || len(res.Commands) != 3 {
		t.Errorf("ran %v, would run %v", r.calls, res.Commands)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
		t.Error("a dry run wrote the skill")
	}
}

func TestInstallWithoutTheToolWritesNothing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	var r recorded
	if _, err := installMCP(t.Context(), r.run, claudeClient, "user", mcp.Write, false); err == nil || !strings.Contains(err.Error(), "not in PATH") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil || len(r.calls) != 0 {
		t.Error("something was written or run")
	}
}

func TestInstallKeepsASkillThatIsNotCflks(t *testing.T) {
	home := withFakeClaude(t)
	path := filepath.Join(home, ".claude", "skills", "codeflock", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // test dir
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("mine"), 0o644); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}
	var r recorded
	if _, err := installMCP(t.Context(), r.run, claudeClient, "user", mcp.Write, false); err == nil {
		t.Error("overwrote a foreign skill")
	}
	if b, _ := os.ReadFile(path); string(b) != "mine" || len(r.calls) != 0 { //nolint:gosec // test file
		t.Errorf("skill %q, calls %v", b, r.calls)
	}
}

func TestUninstallRemovesWhatInstallWrote(t *testing.T) {
	home := withFakeClaude(t)
	var r recorded
	if _, err := installMCP(t.Context(), r.run, claudeClient, "user", mcp.Write, false); err != nil {
		t.Fatal(err)
	}
	res, err := uninstallMCP(t.Context(), r.run, claudeClient, "user", false)
	if err != nil || res.Skill.State != "removed" {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "codeflock")); err == nil {
		t.Error("the skill directory stayed")
	}
	if last := r.calls[len(r.calls)-1]; last[2] != "remove" {
		t.Errorf("last call = %v", last)
	}
}

func TestServeRejectsJSONAndUnknownProfiles(t *testing.T) {
	for _, args := range [][]string{{"mcp", "serve", "--json"}, {"mcp", "serve", "--profile", "execute"}} {
		var out, errOut bytes.Buffer
		code := cli.RunContextWith(t.Context(), cli.Options{Commands: []*cobra.Command{newMCPCommand(nil)}}, args, strings.NewReader(""), &out, &errOut)
		if code == 0 {
			t.Errorf("%v succeeded", args)
		}
	}
}

// failing is a runner whose first n calls fail.
type failing struct {
	recorded
	n int
}

func (f *failing) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, _ := f.recorded.run(ctx, name, args...)
	if len(f.calls) <= f.n {
		return []byte("boom"), errors.New("exit 1")
	}
	return out, nil
}

func TestInstallReplacesARegistrationOnlyWhenAddIsRefused(t *testing.T) {
	withFakeClaude(t)
	f := &failing{n: 1}
	if _, err := installMCP(t.Context(), f.run, claudeClient, "user", mcp.Read, false); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 || f.calls[1][2] != "remove" || f.calls[2][2] != "add" {
		t.Errorf("calls = %v", f.calls)
	}
	f = &failing{n: 3}
	_, err := installMCP(t.Context(), f.run, claudeClient, "user", mcp.Read, false)
	if err == nil || !strings.Contains(err.Error(), "run install again") {
		t.Errorf("err = %v", err)
	}
}

func withCodex(t *testing.T) (home string) {
	t.Helper()
	home = withFakeClaude(t)
	bin := filepath.Dir(mustLook(t, "claude"))
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec // a fake program for the test
		t.Fatal(err)
	}
	return home
}

func mustLook(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCodexUserScopeAddsWithoutScopeAndProjectScopeOnlyWritesTheSkill(t *testing.T) {
	withCodex(t)
	var r recorded
	if _, err := installMCP(t.Context(), r.run, codexClient, "user", mcp.Read, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.calls[0][1:3], " ") + " " + strings.Join(r.calls[0][3:], " "); !strings.HasPrefix(got, "mcp add cflk -- ") || !strings.HasSuffix(got, "mcp serve --profile read") {
		t.Errorf("add = %v", r.calls[0])
	}

	repo := t.TempDir()
	repoRootWas := repoRoot
	repoRoot = func(context.Context) (string, error) { return repo, nil }
	t.Cleanup(func() { repoRoot = repoRootWas })
	r = recorded{}
	res, err := installMCP(t.Context(), r.run, codexClient, "project", mcp.Read, false)
	if err != nil || len(r.calls) != 0 || res.Note == "" {
		t.Fatalf("install project: %v calls %v note %q", err, r.calls, res.Note)
	}
	if want := filepath.Join(repo, ".agents", "skills", "codeflock", "SKILL.md"); res.Skill.Path != want || res.Skill.State != "written" {
		t.Errorf("skill = %+v", res.Skill)
	}
	if res, err = uninstallMCP(t.Context(), r.run, codexClient, "project", false); err != nil || len(r.calls) != 0 || res.Skill.State != "removed" {
		t.Errorf("uninstall project: %v calls %v %+v", err, r.calls, res)
	}
}

func TestProjectScopeNeedsARepository(t *testing.T) {
	withFakeClaude(t)
	repoRootWas := repoRoot
	repoRoot = func(context.Context) (string, error) { return "", errors.New("not a repo") }
	t.Cleanup(func() { repoRoot = repoRootWas })
	var r recorded
	if _, err := installMCP(t.Context(), r.run, claudeClient, "project", mcp.Read, false); err == nil || len(r.calls) != 0 {
		t.Errorf("err %v calls %v", err, r.calls)
	}
}
