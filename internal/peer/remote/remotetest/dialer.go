// Package remotetest holds the Dialer tests use: it answers on this machine, so
// two peers (each with a state directory of its own) can coexist in one test.
package remotetest

import (
	"context"
	"fmt"
	"io"

	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/remote"
)

// ServeFunc is a peer's answer to one stream: server.Server.Serve, or a process
// that runs `cflk peer`.
type ServeFunc func(ctx context.Context, in io.Reader, out io.Writer) error

// Dialer reaches peers by the name of their discovery entry. Every dial is
// recorded, so a test can check which peer was asked, with what argv.
type Dialer struct {
	Peers map[string]ServeFunc
	// Dialed is the argv (after `cflk peer`) of each dial, in order, by peer name.
	Dialed []Dial
}

// Dial is one call of Dial.
type Dial struct {
	Peer string
	Args []string
}

// Dial runs the peer's ServeFunc over pipes, as ssh would run `cflk peer`.
func (d *Dialer) Dial(ctx context.Context, peer discovery.Peer, args ...string) (remote.Conn, error) {
	serve, ok := d.Peers[peer.Name]
	if !ok {
		return remote.Conn{}, fmt.Errorf("remotetest: no peer %q", peer.Name)
	}
	d.Dialed = append(d.Dialed, Dial{Peer: peer.Name, Args: args})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := serve(ctx, inR, outW)
		_ = inR.Close()
		_ = outW.Close()
		done <- err
	}()
	return remote.Conn{In: inW, Out: outR, Wait: func() error { return <-done }}, nil
}
