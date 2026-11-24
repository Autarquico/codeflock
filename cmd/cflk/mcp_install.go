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
	Commands [][]string `json:"commands"`
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
			"codeflock skill where the tool reads skills. It is idempotent: it replaces the registration and\n" +
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

func (c mcpClient) addArgs(scope, bin string, p mcp.Profile) []string {
	args := []string{c.name, "mcp", "add"}
	if c.scoped {
		args = append(args, "--scope", scope)
	}
	return append(args, mcpServerName, "--", bin, "mcp", "serve", "--profile", string(p))
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
// looked up before anything is written. The registration is added first, and
// replaced only if that fails (it exists already), so a failure leaves the
// previous one in place whenever the tool allows it.
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
	remove, add := c.removeArgs(scope), c.addArgs(scope, bin, p)
	if c.managesServer(scope) {
		res.Commands = append(res.Commands, add, remove, add)
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
		if err := register(ctx, run, remove, add); err != nil {
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

// register adds the server; if the tool refuses (it is registered already) it
// replaces the registration, and says so when that leaves none.
func register(ctx context.Context, run execCommand, remove, add []string) error {
	if _, err := run(ctx, add[0], add[1:]...); err == nil {
		return nil
	}
	_, _ = run(ctx, remove[0], remove[1:]...)
	if out, err := run(ctx, add[0], add[1:]...); err != nil {
		return fmt.Errorf("%s: %w: %s (any previous registration was removed: run install again)", strings.Join(add[:3], " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// uninstallMCP removes the registration and the SKILL.md that cflk wrote; one
// that cflk did not write stays.
func uninstallMCP(ctx context.Context, run execCommand, c mcpClient, scope string, dryRun bool) (mcpInstall, error) {
	res := mcpInstall{Tool: c.name, Scope: scope, DryRun: dryRun, Commands: [][]string{}}
	var err error
	if res.Skill.Path, err = c.skillPath(ctx, scope); err != nil {
		return res, err
	}
	remove := c.removeArgs(scope)
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
	if !c.managesServer(scope) {
		res.Note = codexProjectNote
	} else if _, lookErr := exec.LookPath(c.name); lookErr == nil {
		res.Commands = append(res.Commands, remove)
	}
	if dryRun {
		return res, nil
	}
	if len(res.Commands) > 0 {
		_, _ = run(ctx, remove[0], remove[1:]...) // not registered is not an error
	}
	if res.Skill.State == "removed" {
		if err := os.Remove(res.Skill.Path); err != nil {
			return res, err
		}
		_ = os.Remove(filepath.Dir(res.Skill.Path)) // only when it is empty
	}
	return res, nil
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
