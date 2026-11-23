// Package remote is the client side of the wire: a Dialer starts `cflk peer` on
// a peer and Call speaks one request over its streams. How the process is
// started (ssh, or a process on this machine for a test) is the Dialer's.
package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/wire"
)

// Conn is a running `cflk peer`.
type Conn struct {
	// In is its stdin: the request goes in, and closing it says the request is complete.
	In io.WriteCloser
	// Out is its stdout: the response.
	Out io.Reader
	// Wait ends the process and returns how it went.
	Wait func() error
}

// Dialer starts `cflk peer <args...>` on a peer. The command is always that
// fixed argv: what the user typed never travels in it, only in the stream. The
// peer comes from discovery, never from the command line.
type Dialer interface {
	Dial(ctx context.Context, peer discovery.Peer, args ...string) (Conn, error)
}

// maxPeerText bounds the message of an error a peer sent.
const maxPeerText = 1024

// Call sends one request with its body to the peer and returns its response and
// body. What went wrong is one of three things, told apart by the error:
//   - the peer answered with an error: a *domain.Error with its code, the code
//     and the text cleaned of anything that could move a terminal;
//   - the peer could not be reached (ssh failed, cflk is not there) or did not
//     answer in time: a *domain.Error with CF_PEER_UNREACHABLE or CF_PEER_NO_ANSWER,
//     whose message is what ssh said;
//   - the peer answered something that is not the wire: a wire.ErrProtocol.
func Call(ctx context.Context, d Dialer, peer discovery.Peer, req wire.Request, body []byte) (wire.Response, []byte, error) {
	conn, err := d.Dial(ctx, peer)
	if err != nil {
		return wire.Response{}, nil, transportError(ctx, peer, err)
	}
	resp, respBody, err := exchange(conn, req, body)
	waitErr := conn.Wait()
	switch {
	case resp.Error != nil:
	case waitErr != nil:
		// The process failed: what ssh said is the cause, whatever the stream
		// looked like when it closed.
		return wire.Response{}, nil, transportError(ctx, peer, waitErr)
	case err != nil && ctx.Err() != nil:
		return wire.Response{}, nil, transportError(ctx, peer, err)
	case err != nil:
		return wire.Response{}, nil, fmt.Errorf("peer %s: %w", peer.Name, err)
	}
	if resp.Error != nil {
		return wire.Response{}, nil, peerError(resp.Error)
	}
	return resp, respBody, nil
}

// transportError is the failure to talk to a peer at all.
func transportError(ctx context.Context, peer discovery.Peer, err error) error {
	if ctx.Err() != nil {
		return domain.NewError(domain.CodePeerNoAnswer, fmt.Sprintf("peer %s did not answer in time", peer.Name), nil)
	}
	return domain.NewError(domain.CodePeerUnreachable, fmt.Sprintf("peer %s: %s", peer.Name, cleanText(err.Error())), nil)
}

// peerError is an error a peer reported: its code is one of the known shape and
// its text is bounded and free of control characters.
func peerError(e *wire.Error) error {
	return domain.NewError(domain.Code(e.Code), cleanText(e.Message), nil)
}

// cleanText drops control characters and bounds the length of text from outside.
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxPeerText {
		s = strings.ToValidUTF8(s[:maxPeerText], "") + "..."
	}
	return s
}

func exchange(conn Conn, req wire.Request, body []byte) (wire.Response, []byte, error) {
	writeErr := wire.WriteRequest(conn.In, req, body)
	closeErr := conn.In.Close()
	if writeErr != nil || closeErr != nil {
		// The peer may have refused the request and said why before it stopped
		// reading; what it said is the better error.
		if resp, b, err := wire.ReadResponse(conn.Out); err == nil && resp.Error != nil {
			return resp, b, nil
		}
		return wire.Response{}, nil, errors.Join(writeErr, closeErr)
	}
	return wire.ReadResponse(conn.Out)
}
