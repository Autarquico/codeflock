package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/worker/doctor"
)

// DoctorReport is the result of `cflk doctor --json`.
type DoctorReport struct {
	Components []doctor.Component `json:"components"`
}

func newDoctorCmd(opts Options) *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that this machine has what a Job needs: tmux, git and the harnesses with their login",
		Long: "Checks the runtime, git, the free disk and, for each harness installed here, its configuration and\n" +
			"its login, and lists what the Controller and the Worker of 0.4 left here. A failing check is shown with what to do about it;\n" +
			"the command only fails when it cannot run.\n\n" +
			"With --fix it stops and removes the Controller and Worker services and the cf and cf.prev binaries of this machine.\n" +
			"It never deletes data or credentials: it prints the command. Without --fix nothing is changed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.Doctor == nil {
				return domain.NewError(domain.CodeNotImplemented, "this build has no checks for the machine", nil)
			}
			components, err := opts.Doctor(cmd.Context(), fix)
			if err != nil {
				return err
			}
			rep := DoctorReport{Components: components}
			if rep.Components == nil {
				rep.Components = []doctor.Component{}
			}
			return emit(cmd, rep, func(out io.Writer) error { return printDoctor(out, rep) })
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "remove the Controller and Worker services and the cf binaries left by 0.4")
	return cmd
}

func printDoctor(out io.Writer, rep DoctorReport) error {
	t := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(t, "COMPONENT\tSTATUS\tMESSAGE\tACTION")
	for _, c := range rep.Components {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", printable(c.Name), c.Status, dash(printable(c.Message)), dash(printable(c.Action)))
	}
	return t.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
