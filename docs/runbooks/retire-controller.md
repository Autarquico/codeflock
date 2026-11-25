# Retire the Controller (beta.8 and earlier)

Code Flock home: [autarquico.github.io/codeflock](https://autarquico.github.io/codeflock/).

The peer network (`v0.5.0-beta.1` and later) has no Controller and no Worker daemon. `v0.4.0-beta.8` is the last version that has them, and 0.5 cannot read their state. Nothing removes them for you: installing or upgrading `cflk` leaves the Controller and the Worker units running. You retire them, machine by machine, when you have checked that the peer network does what you need. Read a block before you paste it.

Irreversible steps are marked **IRREVERSIBLE**. Everything else you can undo by reinstalling beta.8 and starting the services again. Nothing here touches your repositories or the branches `cflk/…` that Runs already pushed.

## 0. The order

| When | On | What | Section |
| --- | --- | --- | --- |
| Before installing 0.5 | The Controller machine | check that no Run is alive, optional backup (they need the beta.8 `cflk`, which 0.5 replaces) | 1 |
| Install | Every machine | install 0.5; the Controller keeps running | 2 |
| You validate the peer network | Every machine | `cflk peers`, delegate, attach, move ([Getting started](../guide/getting-started.md)) | - |
| After validating | Every machine | `cflk doctor --fix`: removes the Controller and Worker services and the `cf` binaries | 3 |
| After `--fix` | The Controller machine (`cf-controller`) | data, certificate authority, credentials | 4 |
| After `--fix` | Tailscale admin and your DNS | what only the Controller used | 5 |

## 1. Before installing 0.5: no Run alive, backup

With the beta.8 `cflk` and the Controller still running:

```sh
cflk ps
cflk inbox
```

`cflk ps` must list no live Run, and `cflk inbox` nothing that needs an answer. For each live Run you want to keep, bring its work out first:

```sh
cflk task pull <task>
```

or attach and let it finish. If you want a copy of the Controller's state before you delete it in step 4, take a backup now. It is the only way back to it, and 0.5 has no command for it:

```sh
cflk controller backup --help
```

Keep the file outside the data directory of step 4.

## 2. Install 0.5

On every machine:

```sh
curl -fsSL https://github.com/Autarquico/codeflock/releases/download/v0.5.0-beta.2/install.sh | CFLK_VERSION=v0.5.0-beta.2 sh
```

This replaces `cflk` only. The Controller and the Worker services of beta.8 stay as they are (they keep running the old binary until you stop them). Then follow the [getting started guide](../guide/getting-started.md) and use the peer network until you trust it.

## 3. `cflk doctor --fix`, on every machine

`cflk doctor` lists what 0.4 left on the machine, as warnings with the fix: the Controller and Worker services (launchd agents on macOS, systemd user units on Linux), the `cf` and `cf.prev` binaries of `~/.local/bin`, and the data and configuration directories of the old names. It changes nothing.

```sh
cflk doctor
```

When you have validated the peer network, remove the services and the binaries:

```sh
cflk doctor --fix
```

It stops each unit, removes its file, and removes `~/.local/bin/cf` and `~/.local/bin/cf.prev` when they are ours (a `cf` that is not a Code Flock binary is left alone). It is safe to run again: a clean machine reports nothing to do. It never deletes data or credentials; those it lists with the command to run (steps 4 and 5). A LaunchDaemon of the system (`/Library/LaunchDaemons/com.codeflock.*`) needs root, so `doctor` prints the `sudo` commands instead of running them.

A Controller started by hand with `cflk controller run` is not a service: stop it with Ctrl-C or `pkill -f 'cflk controller run'`.

The Runs' tmux sessions can outlive the Worker. Look at them, and kill the ones you do not want:

```sh
tmux ls
tmux kill-session -t <name>
```

## 4. The Controller machine: data and certificates

After `--fix` has run on `cf-controller` (the machine that hosted the Controller). Check it is down: nothing must answer on the Controller's port (7443 by default) and `systemctl --user list-units 'codeflock-*' 'cf-*'` (Linux) or `launchctl list | grep -E 'codeflock|codefabric'` (macOS) must list nothing.

First look at what is there, and make sure you have the backup of step 1 if you want one:

```sh
ls -la ~/.local/share/codeflock ~/.local/share/codefabric ~/.config/codeflock ~/.config/codefabric 2>/dev/null
```

The data directory holds the SQLite database, the blobs and the certificate authority (`pki/`). The config directory holds the credentials of the machine. Worker machines keep their own copy of their credentials and certificate.

**IRREVERSIBLE.** Without a backup, deleting these loses the history of Tasks and Runs, the logs, the checkpoints and the certificate authority:

```sh
rm -rf ~/.local/share/codeflock ~/.local/share/codefabric
```

**IRREVERSIBLE** (the credentials, the certificates and the client identity of every machine; beta.8 stops working after this):

```sh
rm -rf ~/.config/codeflock ~/.config/codefabric
```

On the other machines, the same two commands remove their certificates and credentials, and the worktrees of finished Runs, which live under their data directory: if a Run left work you did not pull, take it out first. The configuration of the peer network is `~/.config/cflk/config.toml`; it is a different directory and these commands do not touch it.

Logs, harmless to keep:

```sh
rm -rf ~/Library/Logs/codeflock ~/Library/Logs/codefabric
```

## 5. Tailscale and DNS

Only what was created for the Controller and is used by nothing else:

- In the Tailscale admin console (Access controls), the ACL or grant that opened the Controller's port (7443 by default) and any tag used only for the Controller or the Workers. The peer network uses `tag:cflk`: keep it, or create it when you join machines (see [Getting started](../guide/getting-started.md)).
- The machine's name in Tailscale, if you created a machine only for the Controller: remove it from the Machines list. **IRREVERSIBLE** for that node's key: to use the machine again it joins the tailnet anew.
- Any DNS record or MagicDNS name you pointed at the Controller, and any firewall rule that opened its port.
- If you shut down a machine or a cloud service that hosted only the Controller, delete it there. **IRREVERSIBLE** with its disk.
