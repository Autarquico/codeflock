package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
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
			rep := DoctorReport{Components: append(components, peerChecks(cmd.Context(), opts, components)...)}
			if rep.Components == nil {
				rep.Components = []doctor.Component{}
			}
			if n := failedRemovals(components); fix && n > 0 {
				failure := domain.NewError(domain.CodeInternal, fmt.Sprintf("--fix could not remove %d of the leftovers of 0.4: see the table", n), nil)
				if asJSON, _ := cmd.Flags().GetBool(flagJSON); asJSON {
					// One document: the error envelope carries the report.
					failure.Details = map[string]any{"components": rep.Components}
					return failure
				}
				if err := printDoctor(cmd.OutOrStdout(), rep); err != nil {
					return err
				}
				return failure
			}
			return emit(cmd, rep, func(out io.Writer) error { return printDoctor(out, rep) })
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "remove the Controller and Worker services and the cf binaries left by 0.4")
	return cmd
}

// peerChecks are what a fleet needs on top of the checks of this machine: a
// harness to run Jobs with, a config file that parses and, for each peer, an
// answer to a hello. It only reads.
func peerChecks(ctx context.Context, opts Options, machine []doctor.Component) []doctor.Component {
	var out []doctor.Component
	if !slices.ContainsFunc(machine, func(c doctor.Component) bool { return strings.HasPrefix(c.Name, "harness:") }) {
		out = append(out, doctor.Component{
			Name: "harness", Status: doctor.ComponentWarn, Message: "ni claude ni codex están disponibles para un peer",
			Action: "instala claude o codex; un peer los busca con el PATH de tu shell de login",
		})
	}
	if opts.Jobs == nil {
		return out
	}
	peers, err := opts.Jobs.Peers(ctx)
	if err != nil {
		return append(out, doctor.Component{
			Name: "peers", Status: doctor.ComponentFail, Message: err.Error(),
			Action: "revisa `peers = [...]` en ~/.config/cflk/config.toml",
		})
	}
	others := 0
	for _, p := range peers {
		if p.This {
			continue
		}
		others++
		out = append(out, doctor.CleanComponent(peerCheck(p)))
	}
	if others == 0 {
		out = append(out, doctor.Component{
			Name: "peers", Status: doctor.ComponentOK,
			Message: "ningún peer: añade `peers = [\"host\"]` en ~/.config/cflk/config.toml o el tag:cflk en Tailscale",
		})
	}
	return out
}

func peerCheck(p PeerView) doctor.Component {
	name := "peer:" + p.Name
	if !p.Reachable {
		return doctor.Component{
			Name: name, Status: doctor.ComponentWarn, Message: "no responde: " + p.Problem,
			Action: "prueba `" + p.Probe + "` (debe esperar en stdin); si cflk está en otra ruta, `[cflk_path]` en la config",
		}
	}
	harnesses := "sin harnesses"
	if len(p.Harnesses) > 0 {
		harnesses = strings.Join(p.Harnesses, ", ")
	}
	return doctor.Component{Name: name, Status: doctor.ComponentOK, Message: fmt.Sprintf("responde (hello); %s; %d jobs", harnesses, p.Jobs)}
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

// failedRemovals counts the leftovers of 0.4 that --fix could not remove.
func failedRemovals(components []doctor.Component) int {
	n := 0
	for _, c := range components {
		if strings.HasPrefix(c.Name, "legacy:") && c.Status == doctor.ComponentFail {
			n++
		}
	}
	return n
}
