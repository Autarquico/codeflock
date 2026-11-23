package remote_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/remote"
	"github.com/Autarquico/codeflock/internal/peer/wire"
)

// closedDialer is a peer whose process ends without a word and with this result.
type closedDialer struct{ waitErr error }

func (d closedDialer) Dial(context.Context, discovery.Peer, ...string) (remote.Conn, error) {
	return remote.Conn{
		In:   nopWriteCloser{io.Discard},
		Out:  strings.NewReader(""),
		Wait: func() error { return d.waitErr },
	}, nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestAProcessThatFailedIsUnreachableWithWhatSSHSaidNotAProtocolError(t *testing.T) {
	d := closedDialer{errors.New("exit status 255: ssh: connect to host mac-mini.tail1234.ts.net port 22: Connection refused")}
	_, _, err := remote.Call(t.Context(), d, mini, wire.Request{Op: wire.OpPS}, nil)
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != domain.CodePeerUnreachable {
		t.Fatalf("error = %v", err)
	}
	for _, want := range []string{"Connection refused", "mac-mini"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q lacks %q", err, want)
		}
	}
	if errors.Is(err, wire.ErrProtocol) {
		t.Errorf("an unreachable peer is not a protocol error: %v", err)
	}
}

func TestAProcessThatEndedCleanlyWithNoAnswerIsAProtocolError(t *testing.T) {
	_, _, err := remote.Call(t.Context(), closedDialer{}, mini, wire.Request{Op: wire.OpPS}, nil)
	if !errors.Is(err, wire.ErrProtocol) || !strings.Contains(err.Error(), "mac-mini") {
		t.Errorf("error = %v", err)
	}
}

func TestAPeerThatIsKilledByTheDeadlineDidNotAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	<-ctx.Done()
	_, _, err := remote.Call(ctx, closedDialer{errors.New("signal: killed")}, mini, wire.Request{Op: wire.OpPS}, nil)
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != domain.CodePeerNoAnswer {
		t.Errorf("error = %v", err)
	}
}
