---
title: Use cflk from Claude Code or Codex
description: Register the cflk MCP server in Claude Code or Codex, choose a profile, and drive Jobs from the model.
---

Code Flock home: [autarquico.github.io/codeflock](https://autarquico.github.io/codeflock/).

`cflk mcp serve` is an MCP server that lets Claude Code or Codex use the peer network from inside their own session: see the Jobs of every machine, start one, read its screen, answer it, enter it, move it, bring it back. The model gets a fixed set of tools, each the same as a `cflk` command. It has no shell. You normally do not run the server yourself; the tool starts it.

## Install

```sh
cflk mcp install --claude
cflk mcp install --codex
```

Each command registers the server with the CLI of that tool (`claude mcp add`, `codex mcp add`), so `claude` or `codex` must be on your `PATH`; if it is not, the command fails before writing anything. It also writes the `codeflock` skill, which tells the model when to use Code Flock and how. Running it again replaces the registration and leaves a current skill alone.

| Flag | Meaning |
| --- | --- |
| `--claude`, `--codex` | Which tool to set up. |
| `--scope user\|project` | `user` (default): this machine. `project`: this repository. |
| `--profile read\|write` | What the model may do. |
| `--dry-run` | Print what would run and what would be written, and change nothing. |

Claude Code and Codex load MCP servers and skills when a session starts: open a new session after an install, or after changing `--profile`. The skill is embedded in the `cflk` binary; after updating `cflk`, run `cflk mcp install` again to refresh it. For Codex, the command also sets `tool_timeout_sec` in `$CODEX_HOME/config.toml`, because Codex cuts a tool call at 60 seconds unless told otherwise.

## Uninstall

```sh
cflk mcp uninstall --claude
cflk mcp uninstall --codex
```

It removes the registration and the `SKILL.md` it wrote, and the `codeflock` directory only if it is then empty. Other skills are not touched.

## Tools

On the wire the tools are named with an underscore.

| Tool | Command | In `read` |
| --- | --- | --- |
| `peers_list` | `cflk peers` | yes |
| `jobs_list` | `cflk ps` | yes |
| `jobs_get` | `cflk show` | yes |
| `jobs_screen` | `cflk screen` | yes |
| `jobs_attach` | `cflk attach` | yes, always read-only |
| `jobs_run` | `cflk run` | no |
| `jobs_send` | `cflk send` | no |
| `jobs_stop`, `jobs_resume` | `cflk stop`, `cflk resume` | no |
| `jobs_clean` | `cflk clean` | no |
| `jobs_move` | `cflk move` | no |
| `jobs_pull` | `cflk pull` | no |

In the `read` profile the tools that change something do not exist. `jobs_attach` opens the Job in a new window of the terminal the server runs in (tmux, iTerm2 or Terminal.app) and, with `read`, always read-only, even if the model asks otherwise. If no window can be opened, it fails with the command to run by hand.

A typical turn: the model calls `jobs_list`, sees a Job `waiting`, reads `jobs_screen` to see what the agent asks, answers with `jobs_send`, and later `jobs_pull` to bring the work here.

## Limits

- **Profiles are a local restriction on the model, not a security boundary.** They apply to `cflk mcp serve` on your machine. They do not limit the peers: every peer trusts every other one fully, and anyone who can run commands as you can just use `cflk`.
- **There is no shell.** The server only runs the `cflk` operations above.
- **`write` lets the model send input to agents**, which run with their permission prompts off. Choose `read` if you want the model to watch only.
- **`read` already shows the screen of Jobs** through `jobs_screen`. Do not give it to a model that should not see what your Jobs print.

## What is tested, and what is not

The install and uninstall commands are tested in CI with fake `claude` and `codex` programs. They have not been run against every version of the real tools. The peer tools are in development: they have not yet been checked by hand on a set of real machines.

## Running the server by hand

```sh
cflk mcp serve --profile read
```

Standard output carries only the MCP protocol and diagnostics go to standard error, so do not pipe it through anything else. `--json` is rejected.
