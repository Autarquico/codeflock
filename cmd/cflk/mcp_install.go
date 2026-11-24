package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codeflock/internal/buildinfo"
	"github.com/Autarquico/codeflock/internal/cli/skills"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/mcp"
)

const (
	mcpServerName = "cflk"
	skillDirName  = "codeflock"
)

// mcpClient is what differs between the tools that cflk registers itself in.
type mcpClient struct {
	name      string // the CLI that registers the server
	skillsDir string // relative to the home or to the repo root
	// scoped says whether `mcp add` takes --scope; Codex registers for the user only.
	scoped bool
	hint   string
}

var (
	claudeClient = mcpClient{
		name: "claude", skillsDir: filepath.Join(".claude", "skills"), scoped: true,
		hint: "install Claude Code (https://docs.claude.com/en/docs/claude-code) and run this again",
	}
	codexClient = mcpClient{
		name: "codex", skillsDir: filepath.Join(".agents", "skills"),
		hint: "install Codex (npm install -g @openai/codex) and run this again",
	}
)

// mcpInstall is what install and uninstall ask of a tool; fields are what --json prints.
type mcpInstall struct {
	Tool     string     `json:"tool"`
	Scope    string     `json:"scope"`
	Profile  string     `json:"profile,omitempty"`
	Binary   string     `json:"binary,omitempty"`
	DryRun   bool       `json:"dry_run"`
	Commands [][]string `json:"commands"` // what ran, or in a dry run what would run first
	// Fallback is what a dry run of install would run, before add, only if a
	// registration of ours that it can write again exists: remove it.
	Fallback [][]string `json:"fallback,omitempty"`
	Skill    skillState `json:"skill"`
	Note     string     `json:"note,omitempty"`
}

type skillState struct {
	Path  string `json:"path"`
	State string `json:"state"` // written, current, removed, kept or absent
}

// execCommand runs a program and returns its combined output; tests replace it.
type execCommand func(ctx context.Context, name string, args ...string) ([]byte, error)

func realExec(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // the CLI of the tool the user chose, with arguments built here
}

type mcpFlags struct {
	claude, codex  bool
	scope, profile string
	dryRun         bool
}

func (f *mcpFlags) bind(cmd *cobra.Command, withProfile bool) {
	fl := cmd.Flags()
	fl.BoolVar(&f.claude, "claude", false, "Claude Code")
	fl.BoolVar(&f.codex, "codex", false, "Codex")
	fl.StringVar(&f.scope, "scope", "user", "user (this machine) or project (this repository). Codex registers servers for the user only: with project, only the skill goes to the repository (the server is not touched)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print what would be done, and do nothing")
	if withProfile {
		fl.StringVar(&f.profile, "profile", string(mcp.Write), "what the model may do: read or write")
	}
}

func (f *mcpFlags) client() (mcpClient, error) {
	switch {
	case f.claude == f.codex:
		return mcpClient{}, invalid("choose exactly one of --claude and --codex")
	case f.scope != "user" && f.scope != "project":
		return mcpClient{}, invalid("--scope must be user or project, not %q", f.scope)
	case f.claude:
		return claudeClient, nil
	}
	return codexClient, nil
}

func newMCPInstallCommand() *cobra.Command {
	var f mcpFlags
	cmd := &cobra.Command{
		Use:   "install --claude|--codex [--scope user|project] [--profile read|write] [--dry-run]",
		Short: "Register `cflk mcp serve` in Claude Code or Codex and install the codeflock skill",
		Long: "Registers `cflk mcp serve` with the CLI of the tool (claude mcp add, codex mcp add) and writes the\n" +
			"codeflock skill where the tool reads skills. It is idempotent: it replaces a registration of its own and\n" +
			"leaves the skill alone when it is already current.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := f.client()
			if err != nil {
				return err
			}
			p, err := mcp.ParseProfile(f.profile)
			if err != nil {
				return err
			}
			res, err := installMCP(cmd.Context(), realExec, c, f.scope, p, f.dryRun)
			if err != nil {
				return err
			}
			return printMCP(cmd, res, "install")
		},
	}
	f.bind(cmd, true)
	return cmd
}

func newMCPUninstallCommand() *cobra.Command {
	var f mcpFlags
	cmd := &cobra.Command{
		Use:   "uninstall --claude|--codex [--scope user|project] [--dry-run]",
		Short: "Remove the registration of `cflk mcp serve` and the codeflock skill",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := f.client()
			if err != nil {
				return err
			}
			res, err := uninstallMCP(cmd.Context(), realExec, c, f.scope, f.dryRun)
			if err != nil {
				return err
			}
			return printMCP(cmd, res, "uninstall")
		},
	}
	f.bind(cmd, false)
	return cmd
}

func (c mcpClient) removeArgs(scope string) []string {
	args := []string{c.name, "mcp", "remove"}
	if c.scoped {
		args = append(args, "--scope", scope)
	}
	return append(args, mcpServerName)
}

func (c mcpClient) addArgs(scope, command string, args ...string) []string {
	argv := []string{c.name, "mcp", "add"}
	if c.scoped {
		argv = append(argv, "--scope", scope)
	}
	return append(append(argv, mcpServerName, "--", command), args...)
}

func (c mcpClient) getArgs() []string { return []string{c.name, "mcp", "get", mcpServerName} }

// registration is the command and arguments of the server called cflk, as
// `mcp get` of Claude Code (Command: / Args:) and of Codex (command: / args:)
// print them; ok is false when the output has no command.
func registration(out []byte) (command string, args []string, ok bool) {
	for line := range strings.Lines(string(out)) {
		key, value, _ := strings.Cut(strings.TrimSpace(line), ":")
		value = strings.TrimSpace(value)
		switch strings.ToLower(key) {
		case "command":
			command = value
		case "args":
			if value != "-" {
				args = strings.Fields(value)
			}
		}
	}
	return command, args, command != ""
}

// ours says whether a registration is `<this binary or a cflk> mcp serve ...`,
// the one install writes. Anything else called cflk is the user's.
func ours(command string, args []string) bool {
	self, _ := os.Executable()
	isBinary := command == self || filepath.Base(command) == "cflk"
	return isBinary && len(args) >= 2 && args[0] == "mcp" && args[1] == "serve"
}

// replaceable says whether the output of `mcp get` is a registration that
// install may replace because it can write it again as it was: ours, with
// exactly the arguments `mcp serve --profile <profile>` and no environment.
func replaceable(out []byte) bool {
	command, args, found := registration(out)
	if !found || !ours(command, args) || len(args) != 4 || args[2] != "--profile" {
		return false
	}
	if _, err := mcp.ParseProfile(args[3]); err != nil {
		return false
	}
	inEnv := false
	for line := range strings.Lines(string(out)) {
		key, value, _ := strings.Cut(strings.TrimSpace(line), ":")
		switch lower := strings.ToLower(key); {
		case lower == "env" || lower == "environment":
			if v := strings.TrimSpace(value); v != "" && v != "-" {
				return false
			}
			inEnv = true
		case inEnv && strings.Contains(line, "="):
			return false
		case strings.TrimSpace(line) == "":
		default:
			inEnv = false
		}
	}
	return true
}

// notFound says whether the output of a failed `mcp get` is the tool saying
// there is no server of that name, which both clients word as `No MCP server named ...`.
func notFound(out []byte) bool {
	return strings.Contains(strings.ToLower(string(out)), "no mcp server named")
}

// managesServer says whether the server is registered in this scope: Codex
// has user scope only, so with project scope only its skill is ours to touch.
func (c mcpClient) managesServer(scope string) bool { return c.scoped || scope == "user" }

const codexProjectNote = "Codex registers servers for the user only: the server was left as it is; use --scope user to manage it"

// repoRoot is the top level of the work tree of the current directory; tests replace it.
var repoRoot = func(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", domain.NewError(domain.CodeInvalidRequest, "--scope project needs a git repository: run this inside one", nil)
	}
	return strings.TrimSpace(string(out)), nil
}

func (c mcpClient) skillPath(ctx context.Context, scope string) (string, error) {
	var base string
	var err error
	if scope == "user" {
		base, err = os.UserHomeDir()
	} else {
		base, err = repoRoot(ctx)
	}
	if err != nil {
		return "", err
	}
	return filepath.Join(base, c.skillsDir, skillDirName, "SKILL.md"), nil
}

// installMCP registers the server and writes the skill. The CLI of the tool is
// looked up before anything is written. The registration is read before it is
// touched, so that one that is not ours, or that has settings cflk cannot write
// again, is never replaced.
func installMCP(ctx context.Context, run execCommand, c mcpClient, scope string, p mcp.Profile, dryRun bool) (mcpInstall, error) {
	res := mcpInstall{Tool: c.name, Scope: scope, Profile: string(p), DryRun: dryRun, Commands: [][]string{}}
	bin, err := os.Executable()
	if err != nil {
		return res, err
	}
	res.Binary = bin
	if res.Skill.Path, err = c.skillPath(ctx, scope); err != nil {
		return res, err
	}
	if _, err := exec.LookPath(c.name); err != nil && !dryRun {
		return res, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s is not in PATH: %s", c.name, c.hint), nil)
	}
	skill, err := skills.Codeflock(buildinfo.Get().Version)
	if err != nil {
		return res, err
	}
	have, err := readSkill(res.Skill.Path)
	if err != nil {
		return res, err
	}
	if have != nil && !bytes.Contains(have, []byte(skills.Marker)) {
		return res, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s is a skill that cflk did not write: nothing was changed; move it away and run this again", res.Skill.Path), nil)
	}
	add := c.addArgs(scope, bin, "mcp", "serve", "--profile", string(p))
	if c.managesServer(scope) {
		if dryRun {
			res.Commands = append(res.Commands, c.getArgs(), add)
			res.Fallback = [][]string{c.removeArgs(scope)}
		}
	} else {
		res.Note = codexProjectNote
	}
	switch {
	case bytes.Equal(have, skill):
		res.Skill.State = "current"
	default:
		res.Skill.State = "written"
	}
	if dryRun {
		return res, nil
	}
	if c.managesServer(scope) {
		if err := register(ctx, &res, run, c, scope, add); err != nil {
			return res, err
		}
	}
	if res.Skill.State == "written" {
		if err := os.MkdirAll(filepath.Dir(res.Skill.Path), 0o755); err != nil { //nolint:gosec // a skills directory is read by the tool
			return res, err
		}
		if err := os.WriteFile(res.Skill.Path, skill, 0o644); err != nil { //nolint:gosec // idem
			return res, err
		}
	}
	return res, nil
}

// register adds the server. A registration called cflk that exists is
// replaced only when it is ours and install can write it again as it was
// (replaceable), and is put back if the new one cannot be added; any other is
// refused before anything is changed. res.Commands records what ran.
func register(ctx context.Context, res *mcpInstall, run execCommand, c mcpClient, scope string, add []string) error {
	do := func(ctx context.Context, argv []string) ([]byte, error) {
		res.Commands = append(res.Commands, argv)
		return run(ctx, argv[0], argv[1:]...)
	}
	failed := func(argv []string, out []byte, err error) error {
		return fmt.Errorf("%s: %w: %s", strings.Join(argv[:3], " "), err, strings.TrimSpace(string(out)))
	}
	current, err := do(ctx, c.getArgs())
	switch {
	case err != nil && !notFound(current):
		return failed(c.getArgs(), current, err)
	case err != nil:
		if out, err := do(ctx, add); err != nil {
			return failed(add, out, err)
		}
		return nil
	case !replaceable(current):
		command, args, _ := registration(current)
		return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s already has a server called %s that cflk cannot replace without losing settings (%s): "+
			"nothing was changed; run `%s` and then this again", c.name, mcpServerName, strings.TrimSpace(command+" "+strings.Join(args, " ")),
			strings.Join(c.removeArgs(scope), " ")), nil)
	}
	command, args, _ := registration(current)
	if out, err := do(ctx, c.removeArgs(scope)); err != nil {
		return fmt.Errorf("could not replace the registration of %s: %w", mcpServerName, failed(c.removeArgs(scope), out, err))
	}
	out, err := do(ctx, add)
	if err == nil {
		return nil
	}
	previous := c.addArgs(scope, command, args...)
	// Put it back even if the install was cancelled meanwhile.
	if _, restoreErr := do(context.WithoutCancel(ctx), previous); restoreErr != nil {
		return fmt.Errorf("%w (the previous registration could not be put back: `%s`)", failed(add, out, err), strings.Join(previous, " "))
	}
	return fmt.Errorf("%w (the previous registration was put back)", failed(add, out, err))
}

// uninstallMCP removes the registration and the SKILL.md that cflk wrote; one
// that cflk did not write stays. A registration that cannot be removed is an
// error, and the skill goes only after the server is gone: a skill that
// outlives its server is harmless, a server that outlives its skill is not.
func uninstallMCP(ctx context.Context, run execCommand, c mcpClient, scope string, dryRun bool) (mcpInstall, error) {
	res := mcpInstall{Tool: c.name, Scope: scope, DryRun: dryRun, Commands: [][]string{}}
	var err error
	if res.Skill.Path, err = c.skillPath(ctx, scope); err != nil {
		return res, err
	}
	have, err := readSkill(res.Skill.Path)
	if err != nil {
		return res, err
	}
	switch {
	case have == nil:
		res.Skill.State = "absent"
	case !bytes.Contains(have, []byte(skills.Marker)):
		res.Skill.State = "kept"
	default:
		res.Skill.State = "removed"
	}
	switch {
	case !c.managesServer(scope):
		res.Note = codexProjectNote
	case dryRun:
		res.Commands = append(res.Commands, c.getArgs(), c.removeArgs(scope))
	default:
		if err := removeServer(ctx, &res, run, c, scope); err != nil {
			return res, err
		}
	}
	if dryRun {
		return res, nil
	}
	if res.Skill.State == "removed" {
		if err := os.Remove(res.Skill.Path); err != nil {
			return res, err
		}
		_ = os.Remove(filepath.Dir(res.Skill.Path)) // only when it is empty
	}
	return res, nil
}

// removeServer removes the registration if it is ours; none registered is done.
func removeServer(ctx context.Context, res *mcpInstall, run execCommand, c mcpClient, scope string) error {
	if _, err := exec.LookPath(c.name); err != nil {
		return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s is not in PATH, so its registration of %s was not removed: %s", c.name, mcpServerName, c.hint), nil)
	}
	do := func(argv []string) ([]byte, error) {
		res.Commands = append(res.Commands, argv)
		return run(ctx, argv[0], argv[1:]...)
	}
	current, err := do(c.getArgs())
	if err != nil {
		if notFound(current) {
			return nil // the tool has no server called cflk
		}
		return fmt.Errorf("%s: %w: %s (the registration of %s may still be there, so the skill was kept)", strings.Join(c.getArgs()[:3], " "), err, strings.TrimSpace(string(current)), mcpServerName)
	}
	if command, args, found := registration(current); !found || !ours(command, args) {
		return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s has a server called %s that cflk did not register (%s): "+
			"it was left as it is", c.name, mcpServerName, strings.TrimSpace(command+" "+strings.Join(args, " "))), nil)
	}
	if out, err := do(c.removeArgs(scope)); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(c.removeArgs(scope)[:3], " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// readSkill is the content of a SKILL.md, nil when there is none.
func readSkill(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the skill path of the tool the user chose
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

func printMCP(cmd *cobra.Command, res mcpInstall, verb string) error {
	out := cmd.OutOrStdout()
	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		return json.NewEncoder(out).Encode(res)
	}
	return writeMCP(out, res, verb)
}

func writeMCP(w io.Writer, res mcpInstall, verb string) error {
	prefix := ""
	if res.DryRun {
		prefix = "dry run, nothing changed: "
	}
	if _, err := fmt.Fprintf(w, "%s%s for %s (scope %s)\n", prefix, verb, res.Tool, res.Scope); err != nil {
		return err
	}
	for _, argv := range res.Commands {
		if _, err := fmt.Fprintf(w, "  %s\n", strings.Join(argv, " ")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "  skill %s: %s\n", res.Skill.State, res.Skill.Path); err != nil {
		return err
	}
	if res.Note != "" {
		_, err := fmt.Fprintf(w, "  note: %s\n", res.Note)
		return err
	}
	return nil
}
