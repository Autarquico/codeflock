package main

import (
	"bytes"
	"context"
	"os"
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
	add := r.calls[len(r.calls)-1]
	if got := strings.Join(add[:6], " "); got != "claude mcp add --scope user cflk" {
		t.Errorf("add = %v", add)
	}
	if tail := strings.Join(add[len(add)-4:], " "); tail != "mcp serve --profile read" {
		t.Errorf("add = %v", add)
	}
	if r.calls[0][2] != "remove" {
		t.Errorf("a registration is replaced, so it is removed first: %v", r.calls)
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
	if len(r.calls) != 0 || len(res.Commands) != 2 {
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
