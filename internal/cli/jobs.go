package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codeflock/internal/domain"
)

// JobView is a Job as the CLI shows it.
type JobView struct {
	ID        string `json:"id"`
	Peer      string `json:"peer"`
	Repo      string `json:"repo"`
	Branch    string `json:"branch"`
	Harness   string `json:"harness"`
	State     string `json:"state"`
	Gen       int    `json:"gen"`
	SessionID string `json:"session_id,omitempty"`
	// Terminal is the tmux session of the current launch, if there is one.
	Terminal string `json:"terminal,omitempty"`
	// Problem is why the Job could not be read up to date; the other fields are
	// then what was last recorded.
	Problem string `json:"problem,omitempty"`
}

// UnreachablePeer is a peer that did not answer a listing, and why.
type UnreachablePeer struct {
	Peer    string `json:"peer"`
	Problem string `json:"problem"`
}

// JobList is what `cflk ps` lists: the Jobs of the peers that answered, and
// apart from them the peers that did not.
type JobList struct {
	Jobs        []JobView         `json:"jobs"`
	Unreachable []UnreachablePeer `json:"unreachable"`
}

// RunRequest asks for a new Job on the current repo.
type RunRequest struct {
	// Harness is the user's name for it ("claude", "codex"); empty is the default.
	Harness string
	// Branch is the branch or commit the Job starts from; empty is the current commit.
	Branch string
	// On is the peer that runs it; empty is this one.
	On string
	// ID is the id of a delegation that failed or whose answer was lost: asking
	// again with it starts nothing twice. Empty draws a new one.
	ID     string
	Prompt string
}

// PeerView is a peer as `cflk peers` shows it.
type PeerView struct {
	Name string `json:"name"`
	// This is true for the machine the command runs on.
	This      bool     `json:"this,omitempty"`
	Reachable bool     `json:"reachable"`
	Harnesses []string `json:"harnesses,omitempty"`
	Jobs      int      `json:"jobs"`
	// Problem is why the peer did not answer.
	Problem string `json:"problem,omitempty"`
}

// Jobs is what the job commands need of a peer. cmd/cflk composes it (ADR 0001):
// the CLI knows neither the runner nor the adapters. Every method that reads a Job
// reconciles it first.
type Jobs interface {
	Run(ctx context.Context, req RunRequest) (JobView, error)
	// List is the Jobs of every peer, or of the one named; a peer that does not
	// answer is in Unreachable, not among the Jobs.
	List(ctx context.Context, peer string) (JobList, error)
	// Peers lists the peers and whether they answer.
	Peers(ctx context.Context) ([]PeerView, error)
	Show(ctx context.Context, id string) (JobView, error)
	Stop(ctx context.Context, id string) (JobView, error)
	Resume(ctx context.Context, id string) (JobView, error)
	Screen(ctx context.Context, id string) (string, error)
	Send(ctx context.Context, id, text string) (JobView, error)
	// Attach enters the terminal of the Job and returns when the person leaves it.
	// Without a terminal it opens a window for it.
	Attach(ctx context.Context, id string, readOnly bool) error
}

func jobsOf(opts Options) (Jobs, error) {
	if opts.Jobs == nil {
		return nil, domain.NewError(domain.CodeNotImplemented, "this build has no Jobs", nil)
	}
	return opts.Jobs, nil
}

// jobCmd builds a command whose argument is a Job id.
func jobCmd(opts Options, use, short string, extra cobra.PositionalArgs, do func(cmd *cobra.Command, jobs Jobs, args []string) error) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: short, Args: extra,
		RunE: func(cmd *cobra.Command, args []string) error {
			jobs, err := jobsOf(opts)
			if err != nil {
				return err
			}
			return do(cmd, jobs, args)
		},
	}
}

func emitJob(cmd *cobra.Command, v JobView) error {
	return emit(cmd, v, func(w io.Writer) error { return printJob(w, v) })
}

func newJobCmds(opts Options) []*cobra.Command {
	run := jobCmd(opts, "run [--harness claude|codex] [--branch B] [--on PEER [--id JOB]] -- PROMPT",
		"Start a Job: an agent working on a new branch cflk/<id> of this repo", cobra.ExactArgs(1),
		func(cmd *cobra.Command, jobs Jobs, args []string) error {
			f := cmd.Flags()
			var req RunRequest
			req.Harness, _ = f.GetString("harness")
			req.Branch, _ = f.GetString("branch")
			req.On, _ = f.GetString("on")
			req.ID, _ = f.GetString("id")
			req.Prompt = args[0]
			v, err := jobs.Run(cmd.Context(), req)
			if err != nil {
				return err
			}
			return emitJob(cmd, v)
		})
	run.Flags().String("harness", "", "the agent: claude (default) or codex")
	run.Flags().String("branch", "", "start from this branch or commit instead of the current one")
	run.Flags().String("on", "", "the peer that runs it: its name, or any for the one with fewest Jobs (default: this one)")

	run.Flags().String("id", "", "retry a delegation with the id its failure named (needs --on PEER): the peer answers the Job it already has instead of starting another")

	ps := jobCmd(opts, "ps [--peer PEER]", "List the Jobs of every peer (or of one)", cobra.NoArgs,
		func(cmd *cobra.Command, jobs Jobs, _ []string) error {
			only, _ := cmd.Flags().GetString("peer")
			list, err := jobs.List(cmd.Context(), only)
			if err != nil {
				return err
			}
			if list.Jobs == nil {
				list.Jobs = []JobView{}
			}
			if list.Unreachable == nil {
				list.Unreachable = []UnreachablePeer{}
			}
			return emit(cmd, list, func(w io.Writer) error { return printJobs(w, list) })
		})

	ps.Flags().String("peer", "", "only the Jobs of this peer")

	peers := jobCmd(opts, "peers", "List the peers, whether they answer, their harnesses and their Jobs", cobra.NoArgs,
		func(cmd *cobra.Command, jobs Jobs, _ []string) error {
			list, err := jobs.Peers(cmd.Context())
			if err != nil {
				return err
			}
			if list == nil {
				list = []PeerView{}
			}
			return emit(cmd, list, func(w io.Writer) error { return printPeers(w, list) })
		})

	show := jobCmd(opts, "show JOB", "Show a Job", cobra.ExactArgs(1),
		func(cmd *cobra.Command, jobs Jobs, args []string) error {
			v, err := jobs.Show(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emitJob(cmd, v)
		})

	stop := jobCmd(opts, "stop JOB", "Stop the agent of a Job; if a process survives the Job stays stopping", cobra.ExactArgs(1),
		func(cmd *cobra.Command, jobs Jobs, args []string) error {
			v, err := jobs.Stop(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emitJob(cmd, v)
		})

	resume := jobCmd(opts, "resume JOB", "Start the agent of a stopped or lost Job again, in the same session", cobra.ExactArgs(1),
		func(cmd *cobra.Command, jobs Jobs, args []string) error {
			v, err := jobs.Resume(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emitJob(cmd, v)
		})

	screen := jobCmd(opts, "screen JOB", "Show what the terminal of the Job's agent shows now", cobra.ExactArgs(1),
		func(cmd *cobra.Command, jobs Jobs, args []string) error {
			text, err := jobs.Screen(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, struct {
				Screen string `json:"screen"`
			}{text}, func(w io.Writer) error { _, err := io.WriteString(w, text); return err })
		})

	send := jobCmd(opts, "send JOB TEXT", "Type TEXT into the agent of a Job and press Enter", cobra.ExactArgs(2),
		func(cmd *cobra.Command, jobs Jobs, args []string) error {
			v, err := jobs.Send(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return emitJob(cmd, v)
		})

	attach := jobCmd(opts, "attach [--read-only] JOB", "Enter the terminal of a Job (a new window when there is no terminal here)", cobra.ExactArgs(1),
		func(cmd *cobra.Command, jobs Jobs, args []string) error {
			readOnly, _ := cmd.Flags().GetBool("read-only")
			return jobs.Attach(cmd.Context(), args[0], readOnly)
		})
	attach.Flags().Bool("read-only", false, "what you type does not reach the agent")

	return []*cobra.Command{run, ps, peers, show, stop, resume, screen, send, attach, newPeerCmd(opts), newInternalCmd(opts)}
}

func printJobs(w io.Writer, list JobList) error {
	if len(list.Jobs) == 0 {
		if _, err := fmt.Fprintln(w, "no Jobs"); err != nil {
			return err
		}
	} else {
		t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(t, "JOB\tPEER\tSTATE\tHARNESS\tREPO\tBRANCH")
		for _, j := range list.Jobs {
			state := printable(j.State)
			if j.Problem != "" {
				state += "?"
			}
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", dash(printable(j.ID)), printable(j.Peer), state, dash(printable(j.Harness)), dash(printable(j.Repo)), dash(printable(j.Branch)))
		}
		if err := t.Flush(); err != nil {
			return err
		}
	}
	for _, u := range list.Unreachable {
		if _, err := fmt.Fprintf(w, "unreachable: %s (%s)\n", printable(u.Peer), printable(u.Problem)); err != nil {
			return err
		}
	}
	return nil
}

func printPeers(w io.Writer, peers []PeerView) error {
	t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(t, "PEER\tSTATE\tHARNESSES\tJOBS\tPROBLEM")
	for _, p := range peers {
		name, state, harnesses, jobs := printable(p.Name), "reachable", printable(strings.Join(p.Harnesses, ",")), fmt.Sprint(p.Jobs)
		problem := dash(printable(p.Problem))
		if p.This {
			name += " (this)"
		}
		if !p.Reachable {
			state, harnesses, jobs = "unreachable", "-", "-"
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", name, state, harnesses, jobs, problem)
	}
	return t.Flush()
}

func printJob(w io.Writer, j JobView) error {
	t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	rows := [][2]string{
		{"job", j.ID},
		{"state", j.State},
		{"peer", j.Peer},
		{"repo", j.Repo},
		{"branch", j.Branch},
		{"harness", j.Harness},
		{"gen", fmt.Sprint(j.Gen)},
	}
	if j.SessionID != "" {
		rows = append(rows, [2]string{"session", j.SessionID})
	}
	if j.Terminal != "" {
		rows = append(rows, [2]string{"terminal", j.Terminal})
	}
	if j.Problem != "" {
		rows = append(rows, [2]string{"problem", j.Problem})
	}
	for _, r := range rows {
		fmt.Fprintf(t, "%s:\t%s\n", r[0], printable(r[1]))
	}
	return t.Flush()
}

// newInternalCmd groups the commands cflk runs on itself: the wrapper under which
// a launch runs and the program Codex notifies. They have a fixed argv, written by
// the runner, and are not for people.
func newInternalCmd(opts Options) *cobra.Command {
	wrapper := &cobra.Command{
		Use: "wrapper LAUNCH_DIR", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if opts.Wrapper == nil {
				return domain.NewError(domain.CodeNotImplemented, "this build has no wrapper", nil)
			}
			return opts.Wrapper(args[0])
		},
	}
	notify := &cobra.Command{
		Use: "notify MARKER SESSION_FILE PAYLOAD", Args: cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			if opts.Notify == nil {
				return domain.NewError(domain.CodeNotImplemented, "this build has no notify handler", nil)
			}
			return opts.Notify(args[0], args[1], args[2])
		},
	}
	internal := &cobra.Command{Use: "internal", Hidden: true}
	internal.AddCommand(wrapper, notify)
	return internal
}

// newPeerCmd is `cflk peer`, what other peers run over ssh. It has a fixed argv:
// the operation and its data come in on stdin (a header and a body, see
// internal/peer/wire), so nothing a person or a peer wrote is ever an argument
// of a shell. It is not for people.
func newPeerCmd(opts Options) *cobra.Command {
	attach := &cobra.Command{
		Use: "attach [--read-only] JOB", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.PeerAttach == nil {
				return domain.NewError(domain.CodeNotImplemented, "this build does not serve peers", nil)
			}
			readOnly, _ := cmd.Flags().GetBool("read-only")
			return opts.PeerAttach(cmd.Context(), args[0], readOnly)
		},
	}
	attach.Flags().Bool("read-only", false, "what the person types does not reach the agent")
	peer := &cobra.Command{
		Use: "peer", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.PeerServe == nil {
				return domain.NewError(domain.CodeNotImplemented, "this build does not serve peers", nil)
			}
			return opts.PeerServe(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	peer.AddCommand(attach)
	return peer
}
