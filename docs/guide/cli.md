---
title: CLI reference
description: Every cflk command for the peer network, with its flags.
---

Code Flock home: [autarquico.github.io/codeflock](https://autarquico.github.io/codeflock/).

This page follows the specification of the peer network. For the flags of any command run `cflk <command> --help`; that output is the authority. `--json` prints JSON, and `--debug` shows the details of an error.

A `<job>` is an id like `job-abcde23456`. Commands on a remote Job carry the generation the caller expects, and the peer rejects them if the Job has moved.

| Command | What it does |
| --- | --- |
| `cflk peers` | Peers, whether they answer, harnesses and number of Jobs |
| `cflk ps [--peer p]` | Jobs of all peers, or one |
| `cflk show <job>` | One Job |
| `cflk run [--on p\|any] [--harness h] -- "prompt"` | Start a Job on the current branch |
| `cflk screen <job>` | What its terminal shows now |
| `cflk send <job> "text"` | Type into the agent |
| `cflk attach <job> [--read-only]` | Enter it |
| `cflk stop <job>`, `cflk resume <job>` | Stop it, resume it on the same peer |
| `cflk clean <job>` | Delete what a `moved` or `done` Job left here |
| `cflk move <job> --to p` | Move it and resume it on another peer |
| `cflk pull <job> [--finish] [--push]` | Bring it to this peer |
| `cflk doctor [--fix]` | Check this machine |
| `cflk mcp install\|uninstall\|serve` | The MCP server, see [Use cflk from Claude Code](../mcp/) |
| `cflk version` | The version |

## `cflk peers`

Lists the machines tagged `tag:cflk` in `tailscale status --json`, plus those in `peers = [...]` of `~/.config/cflk/config.toml`. A peer answers if `cflk peer hello` replies over ssh. `cflk ps` and `cflk peers` show an unreachable peer as `unreachable`, in under 5 seconds.

## `cflk run`

Starts a Job on the branch you are on, with your commits including those not pushed. `--on <peer>` picks the machine, `--on any` the one with the fewest Jobs, no `--on` runs it here. `--harness` is `claude` or `codex`. The prompt follows `--`.

## `cflk attach`

Local: `tmux attach`. Remote: `ssh -t <peer> cflk peer attach <job>`. The id is validated against `^job-[a-z2-7]{10}$` at both ends, and the host comes from the peers `cflk` discovered, never from what you type. `--read-only` watches without sending your keys. Without a terminal on its input, it opens the Job in a new window when it runs in tmux, iTerm2 or Terminal.app, and otherwise prints the command to run.

## `cflk stop` and `cflk resume`

`stop` signals the launch's process group and waits until all of it is gone (30 seconds, then it kills and waits 10 more). If something remains, the Job stays `stopping` until another `cflk stop` clears it. `resume` restarts the agent with its session on the same peer; it is how a `lost` or `stopped` Job comes back. `resume --force` resumes a `moving` Job where it is, and warns that there may be two copies.

## `cflk move`

`cflk move <job> --to <peer>`: stop, commit, send, resume there. Repeating an order with the same transfer id returns its earlier result. If a step after the reservation fails or the peer does not answer, the Job stays `moving` on the origin, without a process:

- `cflk move --retry <job>` repeats the preparation and activation of the same transfer.
- `cflk move --abort <job>` cancels it and the Job returns to `stopped`. If the destination already started it, the abort fails and the Job becomes `moved`. If the destination does not answer, the abort is refused.

A Job with a dirty submodule is not moved. After a move the origin keeps its worktree, which may hold ignored files that did not travel, until `cflk clean <job>`.

## `cflk pull`

`cflk pull <job>` is moving the Job to this peer: it keeps running here. `cflk pull <job> --finish` stops it where it is, brings the exact commit as the branch `cflk/<id>` into the repository of the current directory (it does not change your checkout) and marks the Job `done`. It refuses if the repo is not the Job's or `cflk/<id>` already exists here at another commit. If something fails the Job stays `finishing`: repeat the command, or use `--finish --abort` to return it to `stopped`. `--push` also pushes the branch to the origin; only the code, never the session.

## `cflk doctor`

Checks this machine for `git`, `tmux`, harnesses and leftovers of a previous install. `--fix` removes the launchd or systemd units of a 0.4 Controller or Worker found on this machine.
