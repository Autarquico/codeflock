# Code Flock

Code Flock (`cflk`) keeps your coding agents (Claude Code, Codex) working on another machine of your own network while you use this one for something else. You can enter them to see what they do, answer them, move them to another machine, and bring the work back. All from Claude Code.

It is a network of equal peers over [Tailscale](https://tailscale.com), ssh, tmux and git. There is no Controller and no daemon: every machine with `cflk` can launch a Job, hand it to another, receive one, and see the Jobs of all.

Landing page: <https://autarquico.github.io/codeflock/>

## Status

Pre-release. `v0.5.0-beta.1` is the first release of the peer network, and `v0.4.0-beta.8` is the last one with a Controller. The 0.5 betas have no compatibility with the Controller.

What has been checked with the real CLIs: a Claude Code session exported on one machine and imported on another, in a different directory, is resumed and remembers the conversation; a Codex session does the same across directories on one machine. Moving a Job between machines, entering it, and bringing it back are in development and have not yet been checked by hand on a set of real machines. Codex between two machines has not been tried.

## Install

On every machine of the network:

```bash
curl -fsSL https://github.com/Autarquico/codeflock/releases/download/v0.5.0-beta.1/install.sh | CFLK_VERSION=v0.5.0-beta.1 sh
```

`releases/latest` skips prereleases: pin the version, as above, while 0.5.0 is a beta. A machine needs `git`, `tmux` and at least one harness (`claude`, `codex`). Then follow the [getting started guide](docs/guide/getting-started.md).

## Commands

| CLI | MCP tool | What it does |
| --- | --- | --- |
| `cflk peers` | `peers_list` | Peers, whether they answer, their harnesses and number of Jobs |
| `cflk ps [--peer p]` | `jobs_list` | Jobs of all peers (or one) |
| `cflk show <job>` | `jobs_get` | One Job |
| `cflk run [--on p\|any] [--harness h] -- "prompt"` | `jobs_run` | Start a Job on the current branch |
| `cflk screen <job>` | `jobs_screen` | What its terminal shows now |
| `cflk send <job> "text"` | `jobs_send` | Type into the agent |
| `cflk attach <job> [--read-only]` | `jobs_attach` | Enter it, local or remote |
| `cflk stop <job>` / `cflk resume <job>` | `jobs_stop` / `jobs_resume` | Stop / resume on the same peer |
| `cflk clean <job> [--force]` | `jobs_clean` | Delete what a `moved` or `done` Job left on this peer (`--force` for uncommitted work, CLI only) |
| `cflk move <job> --to p` | `jobs_move` | Move it and resume it on another peer |
| `cflk pull <job> [--finish [--push]]` | `jobs_pull` | Bring it to this peer |
| `cflk tui` (or `cflk` alone in a terminal) | - | Terminal UI over the Jobs of every peer |
| `cflk doctor [--fix]` | - | Check this machine; `--fix` removes what 0.4 left |

Details in the [CLI reference](docs/guide/cli.md).

## Configuration

Peers without the Tailscale tag `tag:cflk` go in `~/.config/cflk/config.toml`:

```toml
peers = ["mac-mini.local", "omarch01"]

[cflk_path]
"mac-mini.local" = "bin/cflk"   # only if cflk is not in ~/.local/bin there
```

See [Getting started](docs/guide/getting-started.md).

## From Claude Code

```bash
cflk mcp install --claude
```

registers the MCP server and the `codeflock` skill, so Claude Code can use the commands above as tools. See [Use cflk from Claude Code](docs/guide/mcp.md).

## What is not there any more

The Controller, Projects, Workflows, Environments, the scheduler and centrally stored secrets are gone from 0.5.0. Each machine keeps its own Claude and Codex login and its own git credentials. Upgrading from 0.4: run `cflk doctor --fix` on each machine, and see [the retire runbook](docs/runbooks/retire-controller.md) for what to stop and delete.

## Documentation

- Guide: [`docs/guide/`](docs/guide/)
- Specification of the peer network: [`docs/specs/p2p-peers.md`](docs/specs/p2p-peers.md)
- Architecture decisions: [`docs/adr/`](docs/adr/)

## Build

Requires Go (see `go.mod`) and, for integration tests, `git` and `tmux`.

```bash
make build          # bin/cflk, static, no cgo
make test           # unit tests
make lint           # golangci-lint v2, including layering rules
make cross          # linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
```

```bash
./bin/cflk version
```

## License

[Apache-2.0](LICENSE)
