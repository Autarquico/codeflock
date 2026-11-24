package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/mcp"
)

// newMCPCommand is `cflk mcp`: the server a model talks to over stdio, and the
// commands that register it in Claude Code or Codex.
func newMCPCommand(jobs cli.Jobs) *cobra.Command {
	root := &cobra.Command{Use: "mcp", Short: "Use Code Flock from Claude Code or Codex through MCP"}

	var profile string
	serve := &cobra.Command{
		Use:   "serve [--profile read|write]",
		Short: "Serve the peer network to a model over MCP on stdio",
		Long: "Serve the Jobs of the peer network over the Model Context Protocol on stdin and stdout.\n" +
			"The profile limits what the model is offered; it is a local restriction, not a security boundary.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return domain.NewError(domain.CodeInvalidRequest, "cflk mcp serve speaks JSON-RPC on stdout: --json does not apply", nil)
			}
			p, err := mcp.ParseProfile(profile)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			fmt.Fprintf(cmd.ErrOrStderr(), "cflk mcp: serving profile %s\n", p)
			return mcp.Serve(cmd.Context(), mcp.NewServer(p, jobs, cmd.ErrOrStderr()), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	serve.Flags().StringVar(&profile, "profile", string(mcp.Write), "what the model may do: read or write")

	root.AddCommand(serve, newMCPInstallCommand(), newMCPUninstallCommand())
	return root
}
