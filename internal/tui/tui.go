// Package tui is the terminal UI of Code Flock: the Jobs of every peer in one
// list, with what the selected one shows on its screen. Like the CLI it holds no
// business logic and no network code: it asks the same use cases (cli.Jobs).
package tui

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Autarquico/codeflock/internal/cli"
)

// Jobs is what the TUI asks of the fleet; cli.Jobs satisfies it.
type Jobs interface {
	List(ctx context.Context, peer string) (cli.JobList, error)
	Screen(ctx context.Context, id string) (string, error)
	Send(ctx context.Context, id, text string) (cli.JobView, error)
	Stop(ctx context.Context, id string) (cli.JobView, error)
	Resume(ctx context.Context, id string) (cli.JobView, error)
	Attach(ctx context.Context, id string, readOnly bool) error
}

// DefaultInterval is how often the list and the screen are read again.
const DefaultInterval = 3 * time.Second

// Options compose the model. The zero value reads every DefaultInterval.
type Options struct {
	Interval time.Duration
}

// Run opens the TUI over jobs and returns when the person quits or ctx ends.
func Run(ctx context.Context, jobs Jobs, opts Options) error {
	if _, err := tea.NewProgram(New(ctx, jobs, opts), tea.WithContext(ctx)).Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
