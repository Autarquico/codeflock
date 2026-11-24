package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// run answers like a client with nothing registered: `get` finds no server.
func (r *recorded) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if args[1] == "get" {
		return []byte("No MCP server named \"cflk\""), errors.New("exit 1")
	}
	return nil, nil
}

func TestInstallRegistersTheServerAndWritesTheSkillOnce(t *testing.T) {
	home := withFakeClaude(t)
	var r recorded

	res, err := installMCP(t.Context(), r.run, claudeClient, "user", mcp.Read, false)
	if err != nil {
		t.Fatal(err)
	}
	add := r.calls[1]
	if got := strings.Join(add[:6], " "); got != "claude mcp add --scope user cflk" {
		t.Errorf("add = %v", add)
	}
	if tail := strings.Join(add[len(add)-4:], " "); tail != "mcp serve --profile read" {
		t.Errorf("add = %v", add)
	}
	if verbs(r.calls) != "get add" {
		t.Errorf("the registration is read, then added: %v", r.calls)
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
	if len(r.calls) != 0 || len(res.Commands) != 2 || len(res.Fallback) != 1 {
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
	r := &fakeTool{}
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

// fakeTool is the `mcp` of a client: one server called cflk that is either
// registered (registered is what `mcp get` prints) or not.
type fakeTool struct {
	recorded
	registered  string
	addFails    func(n int) bool // the n-th add (from 1) fails whatever the state
	removeFails bool
	adds        int
	getFails    bool // `get` fails for a reason other than there being no server
}

func ourRegistration(command, profile string) string {
	return "cflk:\n  Type: stdio\n  Command: " + command + "\n  Args: mcp serve --profile " + profile + "\n  Environment:\n"
}

func (f *fakeTool) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	_, _ = f.recorded.run(ctx, name, args...)
	switch args[1] {
	case "add":
		f.adds++
		if f.registered != "" || (f.addFails != nil && f.addFails(f.adds)) {
			return []byte("boom"), errors.New("exit 1")
		}
		at := slices.Index(args, "--")
		f.registered = "Command: " + args[at+1] + "\nArgs: " + strings.Join(args[at+2:], " ") + "\n"
	case "get":
		if f.getFails {
			return []byte("cannot read config"), errors.New("exit 1")
		}
		if f.registered == "" {
			return []byte("No MCP server named \"cflk\""), errors.New("exit 1")
		}
		return []byte(f.registered), nil
	case "remove":
		if f.removeFails {
			return []byte("read-only config"), errors.New("exit 1")
		}
		f.registered = ""
	}
	return nil, nil
}

func verbs(calls [][]string) string {
	var out []string
	for _, c := range calls {
		out = append(out, c[2])
	}
	return strings.Join(out, " ")
}

func TestInstallReplacesARegistrationThatItCanWriteAgain(t *testing.T) {
	withFakeClaude(t)
	f := &fakeTool{registered: ourRegistration("/old/bin/cflk", "read")}
	if _, err := installMCP(t.Context(), f.run, claudeClient, "user", mcp.Write, false); err != nil {
		t.Fatal(err)
	}
	if verbs(f.calls) != "get remove add" || !strings.Contains(f.registered, "--profile write") {
		t.Errorf("calls %v, now %q", verbs(f.calls), f.registered)
	}
}

func TestInstallNeverTouchesARegistrationItCannotWriteAgain(t *testing.T) {
	withFakeClaude(t)
	for _, other := range []string{
		"Command: /usr/bin/other\nArgs: serve\n",
		"Command: /usr/local/bin/cflk\nArgs: something else\n",
		"Command: /usr/local/bin/cflk\nArgs: mcp serve --profile read --extra\n",
		"Command: /usr/local/bin/cflk\nArgs: mcp serve --profile read\nEnvironment:\n    TOKEN=x\n",
		"Command: /usr/local/bin/cflk\nArgs: mcp serve --profile read\nenv: TOKEN=*****\n",
		"unreadable",
	} {
		for _, c := range []mcpClient{claudeClient, codexClient} {
			withCodex(t)
			f := &fakeTool{registered: other}
			_, err := installMCP(t.Context(), f.run, c, "user", mcp.Write, false)
			if err == nil || !strings.Contains(err.Error(), "mcp remove") || verbs(f.calls) != "get" || f.registered != other {
				t.Errorf("%s %q: err %v calls %v", c.name, other, err, verbs(f.calls))
			}
		}
	}
}

func TestInstallLeavesARegistrationAloneWhenItCannotTellWhetherThereIsOne(t *testing.T) {
	withFakeClaude(t)
	f := &fakeTool{registered: ourRegistration("/old/bin/cflk", "read"), getFails: true}
	_, err := installMCP(t.Context(), f.run, claudeClient, "user", mcp.Write, false)
	if err == nil || verbs(f.calls) != "get" {
		t.Errorf("err %v calls %v", err, verbs(f.calls))
	}
}

func TestInstallDoesNotRetryAnAddThatFailsOnNothingRegistered(t *testing.T) {
	withFakeClaude(t)
	f := &fakeTool{addFails: func(int) bool { return true }}
	_, err := installMCP(t.Context(), f.run, claudeClient, "user", mcp.Write, false)
	if err == nil || verbs(f.calls) != "get add" {
		t.Errorf("err %v calls %v", err, verbs(f.calls))
	}
}

func TestInstallPutsBackTheRegistrationWhenTheReplacementFails(t *testing.T) {
	withFakeClaude(t)
	before := ourRegistration("/old/bin/cflk", "read")
	f := &fakeTool{registered: before, addFails: func(n int) bool { return n == 1 }}
	ctx, cancel := context.WithCancel(t.Context())
	run := func(c context.Context, name string, args ...string) ([]byte, error) {
		out, err := f.run(c, name, args...)
		if args[1] == "add" && err != nil {
			cancel() // cancelled after the removal: the restore still goes through
		}
		if c.Err() != nil {
			return nil, c.Err()
		}
		return out, err
	}
	_, err := installMCP(ctx, run, claudeClient, "user", mcp.Write, false)
	if err == nil || !strings.Contains(err.Error(), "put back") || verbs(f.calls) != "get remove add add" {
		t.Fatalf("err %v calls %v", err, verbs(f.calls))
	}
	if !strings.Contains(f.registered, "/old/bin/cflk") || !strings.Contains(f.registered, "--profile read") {
		t.Errorf("now %q", f.registered)
	}
}

func TestInstallReportsWhatItRan(t *testing.T) {
	withFakeClaude(t)
	f := &fakeTool{}
	res, err := installMCP(t.Context(), f.run, claudeClient, "user", mcp.Write, false)
	if err != nil || verbs(res.Commands) != "get add" {
		t.Errorf("%v %v", err, res.Commands)
	}
}

func TestUninstallFailsWhenTheServerCannotBeRemovedAndKeepsTheSkill(t *testing.T) {
	home := withFakeClaude(t)
	f := &fakeTool{}
	if _, err := installMCP(t.Context(), f.run, claudeClient, "user", mcp.Write, false); err != nil {
		t.Fatal(err)
	}
	f.removeFails = true
	_, err := uninstallMCP(t.Context(), f.run, claudeClient, "user", false)
	if err == nil || !strings.Contains(err.Error(), "read-only config") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "codeflock", "SKILL.md")); err != nil {
		t.Errorf("the skill went with a server that stayed: %v", err)
	}
}

func TestUninstallWithoutTheToolFails(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	var r recorded
	if _, err := uninstallMCP(t.Context(), r.run, claudeClient, "user", false); err == nil || !strings.Contains(err.Error(), "not in PATH") {
		t.Errorf("err = %v", err)
	}
}

func TestUninstallLeavesAServerThatIsNotOurs(t *testing.T) {
	withFakeClaude(t)
	f := &fakeTool{registered: "Command: /usr/bin/other\nArgs: serve\n"}
	if _, err := uninstallMCP(t.Context(), f.run, claudeClient, "user", false); err == nil || verbs(f.calls) != "get" {
		t.Errorf("err %v calls %v", err, verbs(f.calls))
	}
}

func TestUninstallKeepsEverythingWhenItCannotTellWhetherTheServerIsRegistered(t *testing.T) {
	home := withFakeClaude(t)
	f := &fakeTool{}
	if _, err := installMCP(t.Context(), f.run, claudeClient, "user", mcp.Write, false); err != nil {
		t.Fatal(err)
	}
	f.getFails = true
	_, err := uninstallMCP(t.Context(), f.run, claudeClient, "user", false)
	if err == nil || !strings.Contains(err.Error(), "cannot read config") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "codeflock", "SKILL.md")); err != nil {
		t.Errorf("the skill went: %v", err)
	}
}

func TestUninstallOfNothingRegisteredSucceeds(t *testing.T) {
	withFakeClaude(t)
	f := &fakeTool{}
	if _, err := uninstallMCP(t.Context(), f.run, claudeClient, "user", false); err != nil || verbs(f.calls) != "get" {
		t.Errorf("err %v calls %v", err, verbs(f.calls))
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
	if got := strings.Join(r.calls[1][1:3], " ") + " " + strings.Join(r.calls[1][3:], " "); !strings.HasPrefix(got, "mcp add cflk -- ") || !strings.HasSuffix(got, "mcp serve --profile read") {
		t.Errorf("add = %v", r.calls[1])
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
