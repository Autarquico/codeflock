// Package cli implements the cflk command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/worker/doctor"
)

// Persistent flags every command shares.
const (
	flagJSON  = "json"
	flagDebug = "debug"
)

// Options are what the binary composes into the CLI that the CLI cannot reach
// itself (ADR 0001): only cmd/cflk may know the adapters.
type Options struct {
	// Doctor replaces the checks of `cflk doctor`; nil answers CF_NOT_IMPLEMENTED.
	Doctor func(context.Context) ([]doctor.Component, error)
	// Jobs is the peer the job commands act on; nil answers CF_NOT_IMPLEMENTED.
	Jobs Jobs
	// Wrapper runs execwrap on a launch directory (`cflk internal wrapper`), and
	// Notify is the program Codex runs at the end of a turn (`cflk internal notify`).
	Wrapper func(launchDir string) error
	Notify  func(marker, sessionFile, payload string) error
}

// ExecuteWith runs the cf command tree with the composition of the binary and
// returns the exit code.
func ExecuteWith(opts Options) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return RunContextWith(ctx, opts, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// Run is ExecuteWith over explicit streams.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return RunContextWith(context.Background(), Options{}, args, stdin, stdout, stderr)
}

// RunContextWith is Run under a context the caller ends, with the composition of the binary.
func RunContextWith(ctx context.Context, opts Options, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := newRootCmd(opts)
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	// Cobra would add these at Execute, after the usage-error wrapping, and the
	// completion command binds the output stream when it is created.
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	reportUsageAsInvalidRequest(root)
	if err := root.ExecuteContext(ctx); err != nil {
		debug, _ := root.PersistentFlags().GetBool(flagDebug)
		return report(stdout, stderr, err, jsonRequested(root, args), debug)
	}
	return 0
}

// jsonRequested also looks at the raw arguments: a usage error can stop flag
// parsing before --json was read, and its report must still be JSON.
func jsonRequested(root *cobra.Command, args []string) bool {
	if on, _ := root.PersistentFlags().GetBool(flagJSON); on {
		return true
	}
	end := slices.Index(args, "--")
	if end < 0 {
		end = len(args)
	}
	return slices.ContainsFunc(args[:end], func(arg string) bool {
		if arg == "--"+flagJSON {
			return true
		}
		value, ok := strings.CutPrefix(arg, "--"+flagJSON+"=")
		on, err := strconv.ParseBool(value)
		return ok && err == nil && on
	})
}

// report prints err and maps it to the §40 exit code. With --json the §40
// envelope goes to stdout, so a script parses one stream whether or not the
// command worked. Errors without a code are unclassified (1).
func report(stdout, stderr io.Writer, err error, asJSON, debug bool) int {
	var coded *domain.Error
	isCoded := errors.As(err, &coded)
	if !isCoded {
		coded = domain.NewError(domain.CodeInternal, err.Error(), nil)
	}
	switch {
	case asJSON:
		_ = encodeJSON(stdout, errorEnvelope{Error: errorBody{Code: coded.Code, Message: coded.Message, Details: coded.Details}})
	case !isCoded:
		fmt.Fprintln(stderr, "error:", err)
	default:
		fmt.Fprintln(stderr, humanError(coded, debug))
	}
	return domain.ExitCode(coded.Code)
}

// errorEnvelope is what --json prints for a failure: {"error":{code,message,details}}.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    domain.Code    `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// humanError keeps internals out of sight: an internal error shows only the
// request ID that finds it in the log, and --debug adds the rest of the details.
func humanError(e *domain.Error, debug bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "error: %s: %s", e.Code, printable(e.Message))
	if id, ok := e.Details["request_id"].(string); ok && e.Code == domain.CodeInternal {
		fmt.Fprintf(&b, " (request %s)", printable(id))
	}
	if debug {
		keys := make([]string, 0, len(e.Details))
		for k := range e.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "\n  %s: %s", printable(k), printable(fmt.Sprint(e.Details[k])))
		}
	}
	return b.String()
}

// printable drops control characters from text that came from outside, so
// it cannot move the cursor, recolour the terminal or fake extra lines.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func newRootCmd(opts Options) *cobra.Command {
	root := &cobra.Command{
		Use:           "cflk",
		Short:         "Code Flock: coding agents that move between your machines",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE:          showHelpOrRejectArgs,
	}
	flags := root.PersistentFlags()
	flags.Bool(flagJSON, false, "output as JSON; errors too, as {\"error\":{code,message,details}} on stdout")
	flags.Bool(flagDebug, false, "show the details of an error")
	root.AddCommand(newVersionCmd(), newDoctorCmd(opts))
	root.AddCommand(newJobCmds(opts)...)
	return root
}

// reportUsageAsInvalidRequest makes a wrong flag or argument count a
// CF_INVALID_REQUEST (exit 2) instead of an unclassified failure.
func reportUsageAsInvalidRequest(root *cobra.Command) {
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError(err) })
	var wrap func(*cobra.Command)
	wrap = func(cmd *cobra.Command) {
		if cmd.HasSubCommands() && !cmd.Runnable() {
			cmd.Args, cmd.RunE = cobra.ArbitraryArgs, showHelpOrRejectArgs
		}
		if validate := cmd.Args; validate != nil {
			cmd.Args = func(c *cobra.Command, args []string) error {
				if err := validate(c, args); err != nil {
					return usageError(err)
				}
				return nil
			}
		}
		for _, sub := range cmd.Commands() {
			wrap(sub)
		}
	}
	wrap(root)
}

// showHelpOrRejectArgs is the behaviour of a command that only groups others.
func showHelpOrRejectArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageError(fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath()))
	}
	if on, _ := cmd.Flags().GetBool(flagJSON); on {
		return usageError(fmt.Errorf("%q needs a subcommand", cmd.CommandPath()))
	}
	return cmd.Help()
}

func usageError(err error) error {
	return domain.NewError(domain.CodeInvalidRequest, err.Error(), nil)
}
