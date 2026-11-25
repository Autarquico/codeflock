---
title: Getting started
description: Install cflk on your machines, join them in a network and run your first Job on another one.
---

Code Flock home: [autarquico.github.io/codeflock](https://autarquico.github.io/codeflock/).

This page takes you from nothing to a Job running on another machine of yours. You need two machines on the same [Tailscale](https://tailscale.com) tailnet, and a Git repository.

:::note[What has been verified]
This guide describes the behaviour of the 0.5.0 betas. Verified with the real CLIs: a Claude Code session moved to another machine, in another directory, is resumed and remembers the conversation; the same for Codex across directories on one machine. Checked by hand on two Macs and a Linux box: a Claude Code Job run on another peer, sent to, pulled here and moved back, remembering the conversation; a Codex Job run on another peer, pulled here, finished with `pull --finish` and cleaned; Claude Code Jobs moved in both directions between the Linux box and the second Mac; `tui` listing the Jobs of the three machines; `cflk mcp install --claude`; `attach` from a Mac to a Job on the Linux box. Not yet checked by hand: entering a Job from `tui`, `cflk mcp install --codex`, a Codex Job moved back, and moves between the two Macs.
:::

## What a machine needs

- `cflk`, `git` and `tmux`.
- At least one harness: `claude` or `codex`, logged in on that machine. Each machine keeps its own login; nothing is shared.
- Tailscale, with ssh from each machine to the others working (plain ssh keys or Tailscale SSH). With Tailscale SSH, the default policy rule is `check`: it asks for a login in the browser, which a peer's ssh cannot answer, so the connection hangs. In the `ssh` section of the tailnet policy file, replace the default rule with these two, so that `check` no longer matches a non-root login (Tailscale puts `check` rules first). They let every member of the tailnet ssh into their own devices as any non-root user without the browser login; `root` keeps it. `autogroup:self` only covers machines owned by the same Tailscale user that you list in `peers = [...]`; peers tagged `tag:cflk` need a rule whose `dst` is `tag:cflk` instead, which has not been tried. Your access rules must still allow port 22 between them:

  ```json
  "ssh": [
    { "action": "accept", "src": ["autogroup:member"], "dst": ["autogroup:self"], "users": ["autogroup:nonroot"] },
    { "action": "check",  "src": ["autogroup:member"], "dst": ["autogroup:self"], "users": ["root"] }
  ]
  ```
- Linux or macOS (amd64, arm64). Windows is not supported.

## Install

On every machine of the network:

```sh
curl -fsSL https://github.com/Autarquico/codeflock/releases/download/v0.5.0-beta.2/install.sh | CFLK_VERSION=v0.5.0-beta.2 sh
```

`releases/latest` skips prereleases, so while 0.5.0 is a beta, pin the version as above. The installer puts `cflk` in `~/.local/bin`. Two variables change that:

| Variable | Meaning |
| --- | --- |
| `CFLK_VERSION` | The version to install, such as `v0.5.0-beta.2`. |
| `CFLK_INSTALL_DIR` | Where to put `cflk`. Default: `~/.local/bin`. |

Before it installs anything, the script checks an OpenSSH signature on the list of checksums, then the checksum of the archive. It needs OpenSSH 8.2 or later (`ssh-keygen`). If a check fails, nothing is written. The installer and its signing chain are tested in CI against a throwaway key; it has not yet been verified on a clean machine against a published release.

```sh
cflk version
```

Coming from the 0.4.0 betas with a Controller? Read the [retire runbook](https://github.com/Autarquico/codeflock/blob/main/docs/runbooks/retire-controller.md) too: `cflk doctor --fix` removes the old launchd or systemd units on the machine you run it on.

## Join the network

A peer is a machine of your tailnet that has `cflk`. Admitting one is giving it the Tailscale tag `tag:cflk` and ssh access from your other machines. There is no other authorisation: every peer trusts every other one fully, and what a peer can do on another is what you could do by ssh, including starting agents that run with their permission prompts off. Add only machines you would let do that.

`cflk` finds peers with `tailscale status --json` (the nodes tagged `tag:cflk`). Machines without the tag can be listed in `~/.config/cflk/config.toml`, under `peers = [...]`; `cflk peers --help` shows the entries it accepts. A peer is up when `cflk peer hello` answers over ssh.

```sh
cflk peers
cflk doctor
```

`cflk peers` lists the peers, whether they answer, their harnesses and how many Jobs they have. `cflk doctor` checks this machine: `git`, `tmux`, harnesses, and anything left from an older install.

### Configure the peers

Without Tailscale tags, list the machines in `~/.config/cflk/config.toml` (or `$XDG_CONFIG_HOME/cflk/config.toml`). Each entry is a host name that `ssh` can reach with a key, no password prompt: a short name, an mDNS name or a MagicDNS one.

```toml
peers = ["mac-mini.local", "omarch01"]
```

`cflk` reaches each one with `ssh <host> .local/bin/cflk peer`: ssh starts in your home, and `install.sh` puts `cflk` in `~/.local/bin`, which a non-interactive ssh does not have in its `PATH`. If `cflk` is somewhere else on a peer, say where, relative to its home or absolute:

```toml
[cflk_path]
"mac-mini.local" = "bin/cflk"
```

On the peer, `cflk peer` adds the `PATH` of your login shell and the usual directories (`~/.local/bin`, Homebrew, mise) to its own, so `tmux`, `git`, `claude` and `codex` are found as in your terminal. Check it from here:

```sh
cflk peers    # the PROBLEM column says why a peer does not answer
cflk doctor   # also asks each peer for a hello
```

Each peer needs this file too, listing the others: when a Job moves, the peer that has it talks to the destination with its own list and its own ssh. So ssh has to work between every pair of peers, not only from the machine you type on.

On a Mac, Claude Code keeps its login in the keychain, and a process started over ssh cannot read it: a Job another peer starts there would open as "Not logged in". `claude auth login` run over ssh is not enough: it still saves the login in the keychain. Give that Mac a login ssh sessions can read:

- with a Claude subscription, run `claude setup-token` on that Mac: it prints a token that lasts a year, for `CLAUDE_CODE_OAUTH_TOKEN`;
- with an API key, `ANTHROPIC_API_KEY` works the same way.

Export it in the file your shell reads in every ssh session: `~/.zshenv` for zsh, the default on macOS (`$ZDOTDIR/.zshenv` if you set `ZDOTDIR`). Every program started from a shell of yours then gets it in its environment. This makes the file private first, then asks for the token without showing it or leaving it in the shell history (don't run it with `set -x`):

With an API key, change `v=` on the first line to `v=ANTHROPIC_API_KEY`.

```sh
(v=CLAUDE_CODE_OAUTH_TOKEN; umask 077; f=${ZDOTDIR:-$HOME}/.zshenv; touch "$f" && chmod 600 "$f" || exit
 trap 'stty echo' EXIT INT; printf 'token: '; stty -echo || exit; read -r t; stty echo; echo
 [ -n "$t" ] || { echo 'no token: nothing written'; exit 1; }
 printf "\nexport %s='%s'\n" "$v" "$t" >> "$f")
```

With the token, Claude Code makes model requests but cannot start Remote Control, so a Job's screen may show "Remote Control disconnected"; the Job works. Until one of these is there, that peer does not offer `claude` to the others and refuses to start it for them. Codex keeps its login in a file and has no such problem.

The first time an agent starts in a new directory, Claude Code and Codex ask whether you trust the folder. For a Job, `cflk` marks its worktree as trusted before it starts the agent.

## Run a Job on another machine

From a repository, on the branch you are working on:

```sh
cflk run --on mac-mini -- "fix the failing tests"
```

On `mac-mini`, `cflk` starts the agent in tmux, on a branch `cflk/<id>` that contains your commits, including the ones not pushed to the origin. `--on any` picks the peer with the fewest Jobs; without `--on` the Job runs here. `--harness codex` chooses the agent.

```sh
cflk ps
```

lists the Jobs of all peers: peer, repo, branch, harness and state. A peer that is off shows as `unreachable`.

## Look at it, answer it, enter it

```sh
cflk screen <job>             # what its terminal shows now
cflk send <job> "yes, go on"  # type into the agent
cflk attach <job>             # enter it
```

A Job that stopped to ask something shows as `waiting`. Claude Code reports it through its hooks and Codex through its notify program; a harness that cannot never shows `waiting`. `cflk attach --read-only <job>` watches without typing. To leave, press Ctrl+] (on a keyboard where `]` needs Option, closing the terminal tab works too): the agent keeps running. In an attach that is not `--read-only`, Ctrl+C reaches the agent; the usual tmux keys do nothing.

Without a terminal on its input, `cflk attach` opens the Job in a new window when it runs inside tmux, iTerm2 or Terminal.app. That is what `jobs_attach` does from Claude Code, see [Use cflk from Claude Code](../mcp/). The first time on macOS, the system asks whether your terminal may control iTerm2 or Terminal.app; allow it.

## Move it, bring it back

```sh
cflk move <job> --to omarch01
```

The agent stops, `cflk` commits what it has (tracked files and the untracked ones that are not ignored), sends the code with git and the conversation as an export of the session, and resumes the agent on `omarch01`. The conversation never goes into git. The origin keeps the Job as `moved` until you run `cflk clean <job>`.

If anything fails after the Job is reserved, or the destination does not answer, the Job stays on the origin as `moving` and nothing is decided for you: `cflk move --retry <job>` repeats it, `cflk move --abort <job>` cancels it, and `cflk resume --force <job>` resumes it where it was, knowing there may be two copies.

```sh
cflk pull <job>            # move it to this machine and keep it running
cflk pull <job> --finish   # stop it and bring its branch to this repository
```

`--finish` fetches the branch `cflk/<id>` into the repository of the current directory without touching your checkout, and tells you how to merge it. Add `--push` to also push the branch to the origin; the session is never pushed.

After a reboot a Job whose agent died shows as `lost`; nothing relaunches it. `cflk resume <job>` brings it back with its session.

## See everything in one screen

```sh
cflk
```

In a terminal, bare `cflk` (or `cflk tui`) opens a terminal UI with the Jobs of every peer and the screen of the one you select. `s` sends, `a` attaches, `x` stops, `u` resumes, `q` quits.

## From Claude Code

```sh
cflk mcp install --claude
```

Open a new Claude Code session and ask for the same things: see [Use cflk from Claude Code](../mcp/). The full list of commands is in the [CLI reference](../cli/), and the vocabulary in [Concepts](../concepts/).
