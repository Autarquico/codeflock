package remote

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/Autarquico/codeflock/internal/peer/discovery"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/wire"
)

// PushTarget is where `git push` sends the code of a delegation and how its ssh
// is configured.
type PushTarget struct {
	URL string
	// Config are `git -c` settings.
	Config []string
}

// Transport is how this machine reaches its peers: the stream of `cflk peer`,
// the place the code is pushed to, and the command that enters a terminal. SSH
// is the real one; a test has its own.
type Transport interface {
	Dialer
	PushTarget(peer discovery.Peer, mirror string) (PushTarget, error)
	AttachArgv(peer discovery.Peer, id string, readOnly bool) ([]string, error)
}

// SSH reaches a peer with the ssh client. Every argv it builds is fixed: the
// host comes from discovery (and is checked again here), the only operand is a
// Job id checked against its pattern, and what a person typed travels in stdin.
type SSH struct {
	// ConnectTimeout bounds the connection of each ssh; below a second is 5 s.
	ConnectTimeout time.Duration
}

func (s SSH) options() []string {
	secs := int(s.ConnectTimeout / time.Second)
	if secs < 1 {
		secs = 5
	}
	return []string{
		"-o", "BatchMode=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", secs),
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
}

func checkHost(p discovery.Peer) error {
	if !wire.ValidName(p.Host) {
		return fmt.Errorf("%q is not a host name ssh may connect to", p.Host)
	}
	return nil
}

// argv is `ssh [-t] <options> -- <host> <cflk> peer <operands...>`. The cflk is
// where install.sh puts it, relative to $HOME (ssh starts there), unless the peer
// says otherwise: a non-interactive ssh does not have ~/.local/bin in its PATH.
func (s SSH) argv(p discovery.Peer, tty bool, operands ...string) ([]string, error) {
	if err := checkHost(p); err != nil {
		return nil, err
	}
	cflk := cmp.Or(p.Cflk, discovery.DefaultCflk)
	if !discovery.ValidCflkPath(cflk) {
		return nil, fmt.Errorf("%q is not a plain path to cflk on %s", cflk, p.Name)
	}
	argv := []string{"ssh"}
	if tty {
		argv = append(argv, "-t")
	}
	argv = append(argv, s.options()...)
	argv = append(argv, "--", p.Host, cflk, "peer")
	return append(argv, operands...), nil
}

// ServeArgv is the command that answers one request: `ssh <host> .local/bin/cflk peer`.
func (s SSH) ServeArgv(p discovery.Peer) ([]string, error) { return s.argv(p, false) }

// AttachArgv is `ssh -t <host> .local/bin/cflk peer attach [--read-only] <job>`.
func (s SSH) AttachArgv(p discovery.Peer, id string, readOnly bool) ([]string, error) {
	if !job.ValidID(id) {
		return nil, fmt.Errorf("%q is not a Job id", id)
	}
	operands := []string{"attach"}
	if readOnly {
		operands = append(operands, "--read-only")
	}
	return s.argv(p, true, append(operands, id)...)
}

// maxStderr bounds the text of ssh's complaint that reaches an error.
const maxStderr = 512

// Dial starts `ssh <host> .local/bin/cflk peer`. It takes no operands: the request is in stdin.
func (s SSH) Dial(ctx context.Context, p discovery.Peer, args ...string) (Conn, error) {
	if len(args) > 0 {
		return Conn{}, fmt.Errorf("cflk peer takes its request in stdin, not as arguments")
	}
	argv, err := s.ServeArgv(p)
	if err != nil {
		return Conn{}, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // a fixed argv built by argv(): checked host, constant words
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{buf: &stderr, room: maxStderr}
	in, err := cmd.StdinPipe()
	if err != nil {
		return Conn{}, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return Conn{}, err
	}
	if err := cmd.Start(); err != nil {
		return Conn{}, err
	}
	wait := func() error {
		if err := cmd.Wait(); err != nil {
			if msg := strings.Join(strings.Fields(stderr.String()), " "); msg != "" {
				return fmt.Errorf("%w: %s", err, msg)
			}
			return err
		}
		return nil
	}
	return Conn{In: in, Out: out, Wait: wait}, nil
}

type limitedBuffer struct {
	buf  *bytes.Buffer
	room int
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	if n := min(len(p), w.room); n > 0 {
		w.buf.Write(p[:n])
		w.room -= n
	}
	return len(p), nil
}

var mirrorPathPattern = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// PushTarget is `ssh://<host><mirror>`, with ssh configured as for the rest.
// The mirror is what the peer answered, so it is checked: an absolute path of
// plain characters.
func (s SSH) PushTarget(p discovery.Peer, mirror string) (PushTarget, error) {
	if err := checkHost(p); err != nil {
		return PushTarget{}, err
	}
	if !mirrorPathPattern.MatchString(mirror) || path.Clean(mirror) != mirror {
		return PushTarget{}, fmt.Errorf("peer %s answered a mirror path that is not a plain absolute path", p.Name)
	}
	ssh := "ssh " + strings.Join(s.options(), " ")
	return PushTarget{URL: "ssh://" + p.Host + mirror, Config: []string{"core.sshCommand=" + ssh}}, nil
}
