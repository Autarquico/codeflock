// Package discovery finds the peers of this one: the nodes of the tailnet that
// carry tag:cflk (from `tailscale status --json`) plus the `peers` the user lists
// in ~/.config/cflk/config.toml. A peer is identified by its Tailscale node ID;
// its hostname is only for showing, only nodes of this tailnet count (a node
// shared in from another one is not the user's machine, tag or not), and the host to ssh to is never typed by the
// user at the command line: it comes from here.
package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/Autarquico/codeflock/internal/peer/wire"
)

// Tag is the Tailscale tag that admits a node as a peer.
const Tag = "tag:cflk"

// Source says where a peer was found.
type Source string

// The sources of a peer.
const (
	Tailscale Source = "tailscale"
	Config    Source = "config"
)

// Peer is another machine that can run Jobs.
type Peer struct {
	// ID is the stable identity: the Tailscale node ID, or "config:<host>" for a
	// peer that only the config file lists.
	ID string
	// Name is the hostname, to show and to name the peer in `--on`.
	Name string
	// Host is what ssh connects to.
	Host string
	// Online is what Tailscale says; a peer from the config file is assumed
	// online until it fails to answer.
	Online bool
	Source Source
	// Cflk is where cflk is on the peer, relative to the home ssh starts in (or
	// absolute); empty is DefaultCflk.
	Cflk string
}

// DefaultCflk is where install.sh puts cflk, relative to $HOME: a non-interactive
// ssh does not read the profile, so ~/.local/bin is not in its PATH. ssh starts in
// $HOME, so a relative path needs no tilde and no shell.
const DefaultCflk = ".local/bin/cflk"

var cflkPathPattern = regexp.MustCompile(`^/?[A-Za-z0-9._][A-Za-z0-9._/-]*$`)

// ValidCflkPath says whether s can be the cflk path of a peer: plain characters,
// never an option, a tilde or a way up out of the directory.
func ValidCflkPath(s string) bool {
	return cflkPathPattern.MatchString(s) && !slices.Contains(strings.Split(s, "/"), "..")
}

// Result is what discovery finds.
type Result struct {
	Self     Peer
	Peers    []Peer
	Warnings []string
}

// Runner runs a command with a fixed argv and returns its stdout.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner is the Runner that runs the real command.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // a fixed argv: `tailscale status --json`
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// Options are what Discover reads.
type Options struct {
	// Run runs `tailscale status --json`; nil is ExecRunner.
	Run Runner
	// ConfigPath is the config file; empty is none. A file that does not exist is not an error.
	ConfigPath string
}

// DefaultConfigPath is $XDG_CONFIG_HOME/cflk/config.toml, or ~/.config/cflk/config.toml.
func DefaultConfigPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" && filepath.IsAbs(dir) {
		return filepath.Join(dir, "cflk", "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "cflk", "config.toml"), nil
}

// Discover lists the peers. A Tailscale that cannot be asked is a warning, not a
// failure: the peers of the config file still count.
func Discover(ctx context.Context, opts Options) (Result, error) {
	run := opts.Run
	if run == nil {
		run = ExecRunner
	}
	var res Result
	raw, err := run(ctx, "tailscale", "status", "--json")
	if err != nil {
		res.Warnings = append(res.Warnings, "tailscale: "+err.Error())
	} else if err := res.addTailscale(raw); err != nil {
		res.Warnings = append(res.Warnings, "tailscale: "+err.Error())
	}
	cfg, err := ReadConfig(opts.ConfigPath)
	if err != nil {
		return res, err
	}
	res.addConfig(cfg.Peers)
	res.applyCflkPaths(cfg.CflkPath)
	return res, nil
}

type tsNode struct {
	ID       string
	HostName string
	DNSName  string
	Online   bool
	Tags     []string
	// ShareeNode is true for a node that another tailnet shared into this one.
	ShareeNode bool
}

type tsStatus struct {
	Self tsNode
	Peer map[string]tsNode
}

func (r *Result) addTailscale(raw []byte) error {
	var st tsStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("status is not JSON: %w", err)
	}
	if p, ok := peerOf(st.Self); ok {
		r.Self = p
	}
	byKey := make([]string, 0, len(st.Peer))
	for key := range st.Peer {
		byKey = append(byKey, key)
	}
	sort.Strings(byKey)
	for _, key := range byKey {
		n := st.Peer[key]
		if !hasTag(n.Tags) || (n.ID != "" && n.ID == st.Self.ID) {
			continue
		}
		if !ownNode(st.Self, n) {
			r.Warnings = append(r.Warnings, fmt.Sprintf("tailscale: node %q is not a node of this tailnet; skipped", n.HostName))
			continue
		}
		p, ok := peerOf(n)
		if !ok {
			r.Warnings = append(r.Warnings, fmt.Sprintf("tailscale: node %q has no usable name; skipped", n.HostName))
			continue
		}
		r.Peers = append(r.Peers, p)
	}
	return nil
}

// ownNode says whether n is a node of the tailnet of self. A tag proves nothing
// about whose machine it is: a node shared in from another tailnet, or one whose
// name is in another tailnet's domain, is somebody else's. When this node's own
// name is unknown the domain cannot be compared, and only the sharee mark counts.
func ownNode(self, n tsNode) bool {
	if n.ShareeNode {
		return false
	}
	mine := tailnetOf(self.DNSName)
	return mine == "" || tailnetOf(n.DNSName) == mine
}

// tailnetOf is the domain of a MagicDNS name: what follows its first label.
func tailnetOf(dnsName string) string {
	_, domain, _ := strings.Cut(strings.TrimSuffix(dnsName, "."), ".")
	return domain
}

func hasTag(tags []string) bool {
	for _, t := range tags {
		if t == Tag {
			return true
		}
	}
	return false
}

// peerOf reads a node: its host is its MagicDNS name, or its hostname.
func peerOf(n tsNode) (Peer, bool) {
	host := strings.TrimSuffix(n.DNSName, ".")
	if host == "" {
		host = n.HostName
	}
	// Tailscale may show the machine's own name ("Mac mini"): typed, it is "Mac-mini".
	name := strings.ReplaceAll(n.HostName, " ", "-")
	if name == "" {
		name, _, _ = strings.Cut(host, ".")
	}
	if n.ID == "" || !wire.ValidName(name) || !wire.ValidName(host) {
		return Peer{}, false
	}
	return Peer{ID: n.ID, Name: name, Host: host, Online: n.Online, Source: Tailscale}, true
}

// File is what discovery reads of the config file:
//
//	peers = ["mac-mini.local", "omarch01"]
//
//	[cflk_path]            # optional: where cflk is on a peer, from its $HOME
//	"mac-mini.local" = "bin/cflk"
type File struct {
	Peers    []string          `toml:"peers"`
	CflkPath map[string]string `toml:"cflk_path"`
}

// ReadConfig reads the config file at path; none is an empty File.
func ReadConfig(path string) (File, error) {
	if path == "" {
		return File{}, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the user's own config file
	if os.IsNotExist(err) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	var c File
	if err := toml.Unmarshal(raw, &c); err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// applyCflkPaths gives each peer the cflk path the config names for its host or name.
func (r *Result) applyCflkPaths(paths map[string]string) {
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !ValidCflkPath(paths[k]) {
			r.Warnings = append(r.Warnings, fmt.Sprintf("config: cflk_path of %q is not a plain path; ignored", k))
			continue
		}
		for i, p := range r.Peers {
			if p.Host == k || p.Name == k {
				r.Peers[i].Cflk = paths[k]
			}
		}
	}
}

func (r *Result) addConfig(hosts []string) {
	for _, h := range hosts {
		name, _, _ := strings.Cut(h, ".")
		if !wire.ValidName(h) || !wire.ValidName(name) {
			r.Warnings = append(r.Warnings, fmt.Sprintf("config: peer %q is not a valid host name; skipped", h))
			continue
		}
		if r.known(h) {
			continue
		}
		r.Peers = append(r.Peers, Peer{ID: "config:" + h, Name: name, Host: h, Online: true, Source: Config})
	}
}

// known says whether a peer already answers to the name or host h: the one
// Tailscale found is the one that counts, and this machine is not its own peer.
func (r *Result) known(h string) bool {
	short, _, _ := strings.Cut(h, ".")
	for _, p := range append([]Peer{r.Self}, r.Peers...) {
		if p.ID != "" && (sameName(p.Host, h) || sameName(p.Name, h) || sameName(p.Name, short)) {
			return true
		}
	}
	return false
}

// sameName compares host names the way DNS does: without regard to case.
func sameName(a, b string) bool { return strings.EqualFold(a, b) }

// Find returns the peer a person (or a peer, by its node ID) named: by ID, name
// or host, among those discovered.
func (r Result) Find(name string) (Peer, bool) {
	for _, p := range r.Peers {
		if p.ID == name {
			return p, true
		}
	}
	for _, p := range r.Peers {
		if sameName(p.Name, name) || sameName(p.Host, name) {
			return p, true
		}
	}
	return Peer{}, false
}
