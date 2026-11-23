# Retire the Controller (beta.8 and earlier)

The peer network (`v0.5.0`) has no Controller and no Worker daemon. `v0.4.0-beta.8` is the last version that has them, and 0.5 cannot read their state. This runbook is run by you, by hand, on the machine that hosts the Controller and then on each Worker. Read a block before you paste it.

Irreversible steps are marked **IRREVERSIBLE**. Everything else you can undo by reinstalling beta.8 and starting the services again. Nothing here touches your repositories or the branches `cflk/…` that Runs already pushed.

## 0. What goes where

| On | What to do | Section |
| --- | --- | --- |
| The Controller machine | check, stop, uninstall, delete data and certificates | 1 to 5 |
| Each Worker machine | stop and uninstall the Worker service | 4 |
| Tailscale admin and your DNS | remove what only the Controller used | 6 |
| Every machine | install 0.5.0 | 7 |

## 1. Check that no Run is alive

Do it with the beta.8 `cflk`, with the Controller still running.

```sh
cflk ps
cflk inbox
```

`cflk ps` must list no live Run, and `cflk inbox` nothing that needs an answer. For each live Run you want to keep, bring its work out first:

```sh
cflk task pull <task>
```

or attach and let it finish. When a Run dies with the Controller its tmux session on the Worker may stay alive; step 4 lists them.

If you want a copy of the Controller's state before deleting it, take a backup now. It is the only way back to it:

```sh
cflk controller backup --help
```

Keep the file outside the data directory of step 5.

## 2. Stop the Controller

On the Controller machine. First find out which service you have. One of these exists:

- Linux, systemd user service: `codeflock-controller` (`cf-controller` on a beta.6 or earlier).
- macOS, launchd agent: `com.codeflock.controller` (`com.codefabric.controller` on a beta.6 or earlier).

Linux:

```sh
systemctl --user stop codeflock-controller
systemctl --user disable codeflock-controller
```

macOS:

```sh
launchctl bootout "gui/$(id -u)/com.codeflock.controller"
```

If the Controller was started by hand with `cflk controller run`, stop that process with Ctrl-C or `pkill -f 'cflk controller run'`.

Check it is down: `cflk status` must now fail to reach it.

## 3. Uninstall the Controller service

Linux:

```sh
rm -f ~/.config/systemd/user/codeflock-controller.service ~/.config/systemd/user/cf-controller.service
systemctl --user daemon-reload
```

macOS:

```sh
rm -f ~/Library/LaunchAgents/com.codeflock.controller.plist ~/Library/LaunchAgents/com.codefabric.controller.plist
```

If you installed it as a LaunchDaemon (`/Library/LaunchDaemons/com.codeflock.controller.plist`), unload it with `sudo launchctl bootout system/com.codeflock.controller` and remove that file with `sudo rm`.

## 4. Stop and uninstall every Worker

On each Worker machine, including the Controller's if it was also a Worker. Services are `codeflock-worker` (systemd) or `com.codeflock.worker` (launchd), with a suffix if you ran several; `cf-worker` and `com.codefabric.worker` on a beta.6 or earlier.

Linux:

```sh
systemctl --user stop 'codeflock-worker*' 'cf-worker*'
systemctl --user disable 'codeflock-worker*' 'cf-worker*'
rm -f ~/.config/systemd/user/codeflock-worker*.service ~/.config/systemd/user/cf-worker*.service
systemctl --user daemon-reload
```

macOS:

```sh
for label in $(launchctl list | awk '{print $3}' | grep -E '^com\.(codeflock|codefabric)\.worker'); do
  launchctl bootout "gui/$(id -u)/$label"
  rm -f "$HOME/Library/LaunchAgents/$label.plist"
done
```

For a LaunchDaemon at `/Library/LaunchDaemons/com.codeflock.worker.plist`, use `sudo launchctl bootout system/com.codeflock.worker` and `sudo rm` that file.

The Runs' tmux sessions can outlive the daemon. Look at them, and kill the ones you do not want:

```sh
tmux ls
tmux kill-session -t <name>
```

Instead of steps 3 and 4, the 0.5.0 `cflk` can do the service part itself: `cflk doctor --fix` removes the Controller and Worker units of the machine it runs on. It does not delete data.

## 5. Delete the Controller's data

On the Controller machine. First look at what is there, and make sure you have the backup of step 1 if you want one:

```sh
ls -la ~/.local/share/codeflock ~/.local/share/codefabric ~/.config/codeflock ~/.config/codefabric 2>/dev/null
```

The data directory holds the SQLite database, the blobs and the certificate authority. The config directory holds the credentials of the machine. Worker machines keep their own copy of their credentials and certificate.

**IRREVERSIBLE.** Without a backup, deleting these loses the history of Tasks and Runs, the logs, the checkpoints and the certificate authority:

```sh
rm -rf ~/.local/share/codeflock ~/.local/share/codefabric
```

**IRREVERSIBLE** (the credentials, the certificates and the client identity of every machine; beta.8 stops working after this):

```sh
rm -rf ~/.config/codeflock ~/.config/codefabric
```

On the Workers, the same two commands remove their certificates and credentials. The worktrees of finished Runs are under their data directory too; if a Run left work you did not pull, take it out first.

Logs, harmless to keep:

```sh
rm -rf ~/Library/Logs/codeflock ~/Library/Logs/codefabric
```

## 6. Tailscale and DNS

Only what was created for the Controller and is used by nothing else:

- In the Tailscale admin console (Access controls), the ACL or grant that opened the Controller's port (7443 by default) and any tag used only for the Controller or the Workers. The peer network uses `tag:cflk`: keep it, or create it when you join machines (see [Getting started](../guide/getting-started.md)).
- The machine's name in Tailscale, if you created a machine only for the Controller: remove it from the Machines list. **IRREVERSIBLE** for that node's key: to use the machine again it joins the tailnet anew.
- Any DNS record or MagicDNS name you pointed at the Controller, and any firewall rule that opened its port.
- If you shut down a machine or a cloud service that hosted only the Controller, delete it there. **IRREVERSIBLE** with its disk.

## 7. Install 0.5.0

On every machine:

```sh
curl -fsSL https://github.com/Autarquico/codeflock/releases/download/v0.5.0-beta.1/install.sh | CFLK_VERSION=v0.5.0-beta.1 sh
cflk doctor --fix
cflk peers
```

Then follow the [getting started guide](../guide/getting-started.md). Your repositories, and the branches that Runs pushed, are untouched.
