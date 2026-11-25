# Hoja de ruta — Code Flock (red de peers)

Desde la 0.5 Code Flock es una red de peers iguales sobre Tailscale, ssh, tmux y git, sin Controller ni daemon (ver [la spec de la red de peers](specs/p2p-peers.md)). Lo que dependía del Controller (scheduler, Projects, Tasks, Workflows, Environments, secretos centralizados, presupuestos, auditoría central) se descartó el 2026-11-25: sus specs están en [`specs/archive/`](specs/archive/) como `abandoned`. La [especificación técnica](codefabric-spec.md) describe el producto con Controller y se conserva como registro histórico.

## v0.5.0 — cerrar la red de peers (#222)

- **Fallos de la prueba de la flota** ([runbook](runbooks/release.md), «Prueba real de la flota»): el chequeo de login de Claude ignora que haya caducado; registros `moved` huérfanos en `ps`; `cflk mcp install` no sustituye su propio registro de la 0.4; el texto «Worker dedicado» de `doctor`; `~/.local/bin` fuera del PATH de ssh en un Mac (que `doctor` lo avise); el aviso de `doctor` sobre el symlink de `CLAUDE.md`; `cflk clean` remoto responde `CF_NOT_FOUND` aunque `show` encuentra el Job; un Job parado no se puede limpiar.
- **Terminar la retirada:** borrar el código muerto (`internal/domain/{lease,project,workflow,plan,task}.go`, `internal/worker/host` si solo lo usan tests) y lo que queda de #177.
- **Probar en la flota lo que falta:** entrar en un Job desde `tui`, `cflk mcp install --codex`, devolver un Job de Codex, moves entre los dos Macs, omarch02.

## v0.6 — mantener la flota

- `cflk peers upgrade [peer|--all]` con el `install.sh` firmado ([binary-distribution-upgrade](specs/binary-distribution-upgrade.md)).
- Avisos cuando un Job pasa a `waiting` o `done` ([notifications-integrations](specs/notifications-integrations.md)).
- Pendientes de la release y del instalador (#143) y del servidor MCP (#141).

## v0.7 — experimentos sin Controller

- `cflk fork` y `cflk diff` ([fork-compare](specs/fork-compare.md)).
- `cflk eval` en el worktree del Job ([evaluators](specs/evaluators.md)).
- `pull --finish --pr`/`--merge` ([promote](specs/promote.md)).

## v0.8 — visibilidad

- Tokens por Job ([usage-attribution](specs/usage-attribution.md)), cuota por peer ([provider-openrouter-google-harness-quota](specs/provider-openrouter-google-harness-quota.md)), `cflk models` ([provider-anthropic-openai](specs/provider-anthropic-openai.md)) y un panel de uso en el tui ([models-tui](specs/models-tui.md)).
- `cflk transcript` ([run-insights](specs/run-insights.md)) y `cflk stats` ([analytics-history](specs/analytics-history.md)).

## Después

- Adaptadores de OpenCode y Pi, con export y resume de la sesión para que `move` funcione ([opencode-pi-adapters](specs/opencode-pi-adapters.md)).
- Entrada estructurada de Codex, solo si su `notify` no ve las preguntas ([codex-structured-input](specs/codex-structured-input.md)).

Las specs de v0.6 en adelante están escritas para el Controller y llevan al principio la versión p2p que hay que escribir antes de implementarlas.
