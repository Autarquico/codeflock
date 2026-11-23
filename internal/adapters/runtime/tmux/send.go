package tmux

import (
	"context"
	"fmt"
	"strings"
	"time"

	cfruntime "github.com/Autarquico/codeflock/internal/adapters/runtime"
)

// submitDelay is how long the pasted text has to settle before Enter: an
// interactive agent that sees the paste and the Enter at once may take the
// Enter as part of it.
const submitDelay = 150 * time.Millisecond

// SendText types text into the active pane of the session of h and presses
// Enter. The text is pasted (bracketed, if the program asked for it) rather
// than sent key by key, so that it is never read as tmux key names and a
// multi-line text arrives as one input. A session that is not there is an error.
func (r *Runtime) SendText(ctx context.Context, h cfruntime.Handle, text string) error {
	session, err := r.session(h)
	if err != nil {
		return err
	}
	target, buffer := "="+session+":", "cflk-send-"+session
	if _, stderr, err := r.exec(ctx, "set-buffer", "-b", buffer, "--", text); err != nil {
		return fmt.Errorf("tmux: staging the input for %s: %w (%s)", session, err, strings.TrimSpace(stderr))
	}
	if _, stderr, err := r.exec(ctx, "paste-buffer", "-p", "-d", "-b", buffer, "-t", target); err != nil {
		return fmt.Errorf("tmux: pasting into %s: %w (%s)", session, err, strings.TrimSpace(stderr))
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(submitDelay):
	}
	if _, stderr, err := r.exec(ctx, "send-keys", "-t", target, "Enter"); err != nil {
		return fmt.Errorf("tmux: submitting to %s: %w (%s)", session, err, strings.TrimSpace(stderr))
	}
	return nil
}
