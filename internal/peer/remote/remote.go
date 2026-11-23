// Package remote is the client side of the wire: a Dialer starts `cflk peer` on
// a peer and Call speaks one request over its streams. How the process is
// started (ssh, or a process on this machine for a test) is the Dialer's.
package remote

import (
	"context"
	"errors"
	"fmt"
	"io"

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

// Call sends one request with its body to the peer and returns its response and
// body. A response that carries an error is returned as a *domain.Error.
func Call(ctx context.Context, d Dialer, peer discovery.Peer, req wire.Request, body []byte) (wire.Response, []byte, error) {
	conn, err := d.Dial(ctx, peer)
	if err != nil {
		return wire.Response{}, nil, err
	}
	resp, respBody, err := exchange(conn, req, body)
	if waitErr := conn.Wait(); err == nil && waitErr != nil && resp.Error == nil {
		err = fmt.Errorf("peer %s: %w", peer.Name, waitErr)
	}
	if err != nil {
		return wire.Response{}, nil, err
	}
	if resp.Error != nil {
		return wire.Response{}, nil, domain.NewError(domain.Code(resp.Error.Code), resp.Error.Message, nil)
	}
	return resp, respBody, nil
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
