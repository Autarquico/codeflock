package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/charmbracelet/x/term"

	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/tui"
)

// openTUI opens the terminal UI when the command line asks for it: `cflk` with
// no arguments on a terminal, or `cflk tui`. It reports whether it did, and the
// exit code if so. Anything else (a pipe, any other command) is the CLI's.
func openTUI(args []string, jobs cli.Jobs) (code int, opened bool) {
	asked := len(args) == 1 && args[0] == "tui"
	bare := len(args) == 0 && term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
	if !asked && !bare {
		return 0, false
	}
	if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
		fmt.Fprintln(os.Stderr, "cflk tui needs a terminal")
		return 1, true
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := tui.Run(ctx, jobs, tui.Options{}); err != nil {
		fmt.Fprintln(os.Stderr, "cflk:", err)
		return 1, true
	}
	return 0, true
}
