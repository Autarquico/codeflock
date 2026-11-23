---
codename: codefabric
title: Red de peers (sin Controller)
type: feature
status: active
priority: P1
size: XL
issue: 222
prs: []
related_issues: [222]
depends_on: []
related_specs:
  - attach-from-here
  - checkpoint-capture
  - claude-handto
  - claude-code-adapter
  - codex-adapter
created: 2026-11-23
updated: 2026-11-23
---

# Red de peers (sin Controller)

## Contexto / problema

Lo que se quiere es simple: que mis agentes (Claude Code o Codex) sigan trabajando en otra máquina de mi red mientras
uso esta para otra cosa; entrar en esa máquina a ver qué hacen e interactuar con ellos; y traer el trabajo de vuelta.
Todo desde Claude Code.

Code Flock lo hace hoy con un Controller central: API HTTPS con PKI propia (`internal/controller/pki`), relay de git
(`internal/controller/relay`), hub de attach (`internal/controller/attachhub`), scheduler, leases y reconciliación
(`internal/app/{scheduler,leases,reconciliation}`), más Projects, Workflows, Environments y secretos centralizados.
Son 119.427 líneas de Go sin tests en el repo (medido el 2026-11-23); `internal/controller` (16.829), `internal/app`
(17.800) y `internal/client` (7.784) existen para coordinar. Tailscale ya da red e identidad, ssh ejecución remota,
tmux sesiones que sobreviven y en las que se puede entrar, y git el transporte del código. El Controller sobra.

## Scope

**Entra:**
- Una red de **peers iguales**: cada máquina con `cflk` puede lanzar un Job, delegarlo a otra, recibirlo, moverlo y
  ver los Jobs de todas. Sin Controller ni daemon.
- Mover un Job entre peers con git (el código) y el export de la sesión nativa (la conversación), y reanudarlo allí.
- Entrar en un Job (`attach`) local o remoto, también desde Claude Code (ventana nueva, como `attach-from-here`).
- Todo lo anterior desde Claude Code: servidor MCP y skill `codeflock` sobre los mismos comandos.
- **Retirar el Controller entero:** su código, sus servicios, su infraestructura dedicada, su documentación y su CI
  (ver «Retirada del Controller»).

**No entra:**
- Repartir Jobs solo (scheduler): el peer lo elige quien lanza, o `--on any` toma el que tenga menos Jobs.
- Secretos centralizados: cada peer tiene su login de Claude/Codex y sus credenciales de git.
- Projects, Workflows, Environments, presupuestos, auditoría central.
- Migrar el estado de un Controller: la beta.8 es la última versión con Controller.
- Submódulos con cambios (un Job con un submódulo sucio no se mueve) y Windows.

## Modelo

- **Peer:** una máquina de la tailnet con `cflk`, `git`, `tmux` y al menos un harness (`claude`, `codex`). Su
  identidad es el node ID estable de Tailscale; el hostname es solo para mostrar. Se descubren con
  `tailscale status --json` (nodos con `tag:cflk`) más `peers = [...]` en `~/.config/cflk/config.toml`. Un peer
  responde si `cflk peer hello` contesta por ssh.
- **Confianza:** todos los peers son máquinas del mismo usuario, y cada uno confía plenamente en los demás: lo que
  puede hacer un peer en otro es lo que podría hacer el usuario entrando por ssh (lanzar agentes, que corren con
  `bypassPermissions`, `internal/adapters/harness/claude/start.go:126`). Admitir un peer es darle la tag y acceso
  ssh; no hay otra autorización. El perfil `read` del MCP limita solo al modelo local, no a los peers.
- **Job:** un agente trabajando en una rama de un repo en un peer. Sustituye a Task y Run.
  - `id`: `job-<10 caracteres base32 aleatorios>`, generado por quien lo crea; sin contador ni nombre de peer.
  - `repo`: la URL del origin normalizada; su identidad es el SHA-256 de esa URL (los mirrors se guardan por él).
  - `branch`: `cflk/<id>`.
  - `harness`, `session_id` (fijado al lanzar: Claude con `--session-id`, `internal/adapters/harness/claude/sessionid.go:28`;
    Codex según «Preguntas abiertas»).
  - `gen`: la generación de propiedad, que sube en cada movimiento.
  - `peer`, `state`, `created_by`, `history` (lista de `{gen, peer, commit}`).
- **Estados:** `running` · `waiting` (espera a una persona) · `stopped` · `done` · `failed` · `lost` (sin proceso
  ni registro de salida, p. ej. tras un reinicio) · `moving` (transferencia en curso o sin confirmar) · `moved`
  (vive en otro peer; guarda cuál y con qué `gen`).
- **Estado local:** `~/.local/state/cflk/jobs/<id>/` con `job.json` (escrito con `support/atomicfile`), el worktree,
  los registros del wrapper y la sesión tmux `cflk-<id>-g<gen>`. Es la única fuente de verdad del Job en ese peer.
  Un Job lo posee el peer cuyo registro tiene la `gen` más alta y no está `moved`.

## Ejecución sin daemon

- Cada arranque de un agente es un **launch** con id aleatorio propio. Corre en la sesión tmux `cflk-<job>-<launch>`
  bajo un wrapper (lo que hoy es `internal/worker/execwrap`) que escribe en `jobs/<id>/launches/<launch>/` el registro
  `started` (pid, grupo de procesos e identidad, `worker/procident`) y, al terminar, `exit`. `job.json` apunta al
  launch actual; los registros de launches anteriores no se leen.
- Cualquier comando que lea un Job lo reconcilia en el momento, **solo si está en `running`, `waiting` o `lost`**:
  sesión viva → `running`/`waiting`; `exit` → `done` o `failed`; ni sesión ni `exit` → `lost`. `stopped`, `moving`,
  `moved`, `done` y `failed` no los cambia la reconciliación. Nada se reinicia solo: `lost` se reanuda con
  `cflk resume`.
- **`waiting`:** Claude, con hooks instalados solo para el Job: `Notification` pone el marcador `waiting` en el
  directorio del launch y `UserPromptSubmit` lo quita. Codex, con su programa `notify` (fin de turno) para ponerlo; lo
  quitan `cflk send`, un attach o que el rollout crezca después del marcador. Se valida contra los CLIs reales en la
  primera fase; si un harness no lo permite, ese harness no muestra `waiting`.
- **Session ID:** Claude lo fija al lanzar con `--session-id` (`internal/adapters/harness/claude/sessionid.go:28`).
  Codex no: tras lanzar, se toma el único rollout de `CODEX_HOME/sessions` cuyo cwd es el worktree del Job y que es
  posterior al `started`; ninguno o más de uno es un error del launch.

## Transporte

- Lo remoto es `ssh <peer> cflk peer` con argv fijo: la operación y los datos van en stdin. La única excepción es el
  attach, que necesita la terminal en stdin: `ssh -t <peer> cflk peer attach <job>`, con el id validado contra
  `^job-[a-z2-7]{10}$` en los dos extremos. El host sale de la lista de peers descubiertos, nunca de lo que escribe el
  usuario ni de lo que devuelve otro peer.
- El stream es una línea JSON de cabecera (máx. 64 KiB; `op` de una lista cerrada; `job`, `gen`, `transfer`,
  `commit`, tamaños) seguida, si hay sesión, del archivo de `SessionPorter.Export` con su longitud declarada. Un
  stream truncado o que no cuadra con la cabecera se rechaza entero. Los límites son los de `harness.SessionLimit`; el
  archivo se lee con `ReadSessionArchive` (`internal/adapters/harness/sessionarchive.go:113`).
- El código viaja con `git push` (argv, sin shell) a un mirror bare del destino
  (`~/.local/share/cflk/repos/<sha-repo>.git`), a una ref inmutable por movimiento: `refs/cflk/<job>/g<gen>`.
  **La conversación nunca entra en git**: va solo por el stream.

## Mover un Job

La regla es una: **el origen A conserva el Job hasta que B confirma que lo ha arrancado, y ante cualquier duda el Job
se queda en `moving` hasta que yo lo resuelva.** Nada se resuelve solo. Un candado por Job (`support/flock`) cubre en
cada peer lo que ese peer hace; `resume`, `stop`, `send`, `move` y `pull` lo comparten.

1. **Parar.** A escribe `stopped`, pide al harness que pare (`Adapter.Stop`) y espera a que **el grupo de procesos
   entero** haya desaparecido (no basta el `exit` del wrapper). Si a los 30 s sigue, lo mata y espera otros 10 s; si
   aun así queda algo, aborta: el Job queda `stopped` y A lo dice.
2. **Sellar.** A hace commit de lo rastreado y lo no rastreado que no esté ignorado (con las guardas de
   `internal/worker/gitops/seal.go`), anota el `commit` exacto y exporta la sesión.
3. **Reservar.** A escribe `moving` con `{to: B, gen: n+1, transfer: <aleatorio>, commit}`.
4. **Preparar.** A empuja `refs/cflk/<job>/g<n+1>` y manda `prepare` con la sesión. B, bajo su candado, escribe
   `prepared {transfer, gen, commit}`, crea el worktree en ese commit exacto y deja la sesión en un directorio de
   espera. **No arranca nada.** Repetir `prepare` con el mismo `transfer` no cambia nada.
5. **Activar.** A manda `activate {transfer}`. B solo acepta si tiene ese `transfer` en `prepared`: importa la sesión
   con un `session_id` nuevo, escribe el Job (`gen: n+1`, `running`, launch nuevo) **antes** de arrancar, arranca, y
   da el arranque por bueno si el agente sigue vivo a los 10 s (un `--resume` de una sesión que no existe hace salir
   al CLI). Si no arranca, B borra el Job, marca el `transfer` como abortado y responde error; A, al recibir ese
   error, vuelve a `stopped`. Repetir `activate` devuelve el mismo resultado sin arrancar dos veces.
6. **Confirmar.** Con el ok de B, A escribe `moved` → `{B, n+1}` y borra su worktree.

Cualquier otro error, o la falta de respuesta, desde el paso 3 deja el Job en A como `moving`, sin proceso. Lo resuelvo yo:

- `cflk move --retry <job>`: repite `prepare` y `activate` con el mismo `transfer`.
- `cflk move --abort <job>`: B marca el `transfer` como abortado (un `activate` que llegue tarde se rechaza) y A
  vuelve a `stopped`. Si B ya lo activó, el abort falla y A pasa a `moved`. Si B no contesta, el abort se niega.
- `cflk resume --force <job>`: reanuda en A aunque esté `moving`, avisando de que puede haber dos copias.

Al importar siempre con un `session_id` nuevo no hay colisiones: un Job que vuelve a un peer donde ya estuvo
(A→B→A) no toca su transcript viejo. Un import a medias (caída entre instalar y escribir el Job) deja un transcript
con un id que nadie referencia, sin efecto.

`ps`, `show` y `pull` siguen los `moved` hasta el peer con la `gen` más alta (A→B→C); un peer inalcanzable en la
cadena muestra el Job como «en `<peer>`, sin confirmar».

## Recuperar el trabajo aquí

- `cflk pull <job>` es mover el Job a este peer: sigue vivo aquí, en su worktree de `cflk`.
- `cflk pull <job> --finish` lo para y sella en el peer que lo tiene (pasos 1 y 2), trae ese commit exacto como rama
  `cflk/<id>` al repo del directorio actual (`git fetch`), y solo entonces el dueño lo marca `done`. No cambia mi
  checkout: dice cómo hacerlo. Rechaza si el repo del directorio no es el del Job o si `cflk/<id>` ya existe aquí con
  otro commit. Si el fetch falla, el Job queda `stopped` en su peer. Con `--push`, también empuja la rama al origin.

## Comandos

| CLI | MCP | Qué hace |
|---|---|---|
| `cflk peers` | `peers_list` | Peers, si responden, harnesses y nº de Jobs |
| `cflk ps [--peer p]` | `jobs_list` | Jobs de todos los peers (o de uno) |
| `cflk show <job>` | `jobs_get` | Un Job |
| `cflk run [--on p\|any] [--harness h] -- "prompt"` | `jobs_run` | Lanza un Job sobre la rama actual |
| `cflk screen <job>` | `jobs_screen` | Lo que muestra su terminal ahora (`tmux capture-pane`) |
| `cflk send <job> "texto"` | `jobs_send` | Escribe en el agente (`Adapter.SendInput`) |
| `cflk attach <job> [--read-only]` | `jobs_attach` | Entra (local: `tmux attach`; remoto: `ssh -t`) |
| `cflk stop <job>` / `cflk resume <job>` | `jobs_stop` / `jobs_resume` | Para / reanuda en el mismo peer |
| `cflk move <job> --to p` | `jobs_move` | Mueve y reanuda en otro peer |
| `cflk pull <job> [--finish] [--push]` | `jobs_pull` | Lo trae a este peer |

Las herramientas MCP se registran con guion bajo, como hoy (`internal/mcp/server.go:114`). El perfil `read` expone
`peers_list`, `jobs_list`, `jobs_get`, `jobs_screen` y `jobs_attach` forzado a solo lectura. La skill `codeflock` se
reescribe sobre estos comandos.

## Qué se reaprovecha

- `internal/adapters/harness` (Claude, Codex, `SessionPorter`), `internal/adapters/runtime/tmux`, `internal/adapters/pty`.
- `internal/worker/gitops` sin el relay ni la CA; `internal/worker/execwrap` como wrapper del Job.
- De `internal/worker/executor` (8.506 líneas): arrancar y parar un harness bajo tmux con su wrapper y la espera de
  salida (`executor/stop.go`); se separa del `journal`, que se borra.
- De `internal/client` antes de borrarlo: la gestión de sesiones repetidas (`client/session`) y las guardas de
  checkout (`client/taskpull`), portadas al peer con sus tests.
- `internal/attachwindow`, `internal/tui` (con el estado de los peers como fuente), `internal/worker/{doctor,host,
  sysinfo,procident}`, `internal/support/*`, y `internal/mcp` e `internal/cli` reescritos sobre los comandos.

El plan lista cada comportamiento que se porta o se abandona antes de borrar sus tests.

## Retirada del Controller

- **Código:** `internal/controller`, `internal/client` (tras portar lo de arriba), `internal/app` salvo lo que el plan
  rescate, `internal/worker/{daemon,journal,outputship,attachrelay,secretjit,applier,globalapply,workflowfetch,
  authsync}`, `internal/adapters/secrets`, `internal/api`, `internal/repoconfig` si no lo usa nada,
  `cmd/cf-transition`, y los comandos `cflk controller …` y `cflk worker join/drain`.
- **Servicios:** las unidades launchd/systemd del Controller y del Worker daemon (`internal/support/svcunit` se queda
  solo si algo lo usa; si no, se borra). `cflk` desinstala las suyas al actualizar a v0.5.0 (`cflk doctor --fix`).
- **Infraestructura dedicada:** la máquina o el servicio donde vive el Controller, su directorio de datos (SQLite,
  blobs, CA), sus certificados y las entradas de Tailscale/DNS que solo usa él. Apagarlo y borrarlo lo hace el usuario
  con un runbook (`docs/runbooks/retire-controller.md`), después de que no quede ningún Run vivo en la beta.8.
- **Documentación y web:** guías, conceptos y páginas del sitio que hablan de Controller, Projects, Workflows,
  Environments o secretos; specs de lo borrado pasan a `docs/specs/archive/` como `abandoned`.
- **CI:** los jobs y tests que solo existen para el Controller (integración del Controller, e2e de enrolamiento).

## Criterios de aceptación

### Scenario: Ver la flota
- **Given** tres peers, uno apagado
- **When** ejecuto `cflk ps`
- **Then** veo los Jobs de los dos que responden con peer, repo, rama, harness y estado, y el tercero como
  `unreachable`, en menos de 5 s.

### Scenario: Delegar un Job
- **Given** estoy en una rama de un repo con un commit sin empujar al origin
- **When** ejecuto `cflk run --on mac-mini -- "arregla los tests"`
- **Then** en `mac-mini` corre `claude` en tmux sobre `cflk/<id>`, que contiene ese commit, y `cflk ps` lo muestra
  `running`.

### Scenario: Se ve que espera
- **Given** un Job de Claude y otro de Codex en marcha
- **When** cada agente se para a preguntar algo
- **Then** `cflk ps` muestra los dos `waiting` (o el harness que no lo permita queda documentado como tal), y al
  responderles vuelven a `running`.

### Scenario: Interactuar sin terminal
- **Given** un Job `waiting`
- **When** el modelo llama a `jobs_screen` y luego a `jobs_send` con la respuesta
- **Then** ve lo que pregunta el agente y el agente recibe el texto y sigue.

### Scenario: Entrar desde Claude Code
- **Given** un Job `running` en otro peer y Claude Code en iTerm2, tmux o Terminal.app
- **When** pido a Claude que me deje entrar
- **Then** `jobs_attach` abre una ventana con el attach por ssh de ese Job.

### Scenario: Mover conserva la conversación
- **Given** un Job `running` en `omarch01` con cambios sin commit, uno de Claude y otro de Codex
- **When** muevo cada uno a `mac-mini` y le pregunto por algo que se habló antes
- **Then** el agente lo recuerda, el worktree tiene los cambios, y en `omarch01` el Job queda `moved` → `mac-mini`
  sin proceso vivo.

### Scenario: Un destino que falla no pierde nada
- **Given** un movimiento hacia un peer que no tiene el harness
- **When** falla la reanudación en el destino
- **Then** el Job sigue en el origen, `stopped`, con su rama y su sesión, y `cflk resume` lo reanuda allí.

### Scenario: Una respuesta perdida no duplica el Job
- **Given** un movimiento en el que el destino reanuda pero la conexión se corta antes de responder
- **When** consulto el Job en el origen y luego ejecuto `cflk move --retry`
- **Then** el origen lo muestra `moving` y se niega a reanudarlo sin `--force`; tras `--retry` queda `moved` sin que B
  arranque una segunda vez, y solo corre una copia.

### Scenario: Abortar a tiempo
- **Given** un Job `moving` cuyo destino recibió `prepare` pero no `activate`
- **When** ejecuto `cflk move --abort` y después llega un `activate` retrasado
- **Then** el Job vuelve a `stopped` en el origen y B rechaza el `activate`.

### Scenario: Dos movimientos a la vez
- **Given** un Job en A
- **When** lanzo a la vez `move --to B` y `move --to C`
- **Then** uno gana y el otro falla sin efectos; el Job corre en un solo peer.

### Scenario: Ida y vuelta
- **Given** un Job movido de A a B
- **When** lo muevo de B a A y luego de A a C
- **Then** cada vez reanuda la misma conversación, y `cflk ps` desde cualquier peer lo ve solo en C.

### Scenario: Reinicio
- **Given** un Job `running` en un peer que se reinicia
- **When** vuelve y ejecuto `cflk ps`
- **Then** el Job sale `lost`, nada lo ha relanzado, y `cflk resume` lo reanuda con su sesión.

### Scenario: Traerlo aquí y terminar
- **Given** un Job en otro peer
- **When** ejecuto `cflk pull <job> --finish` desde el repo del Job
- **Then** el agente está parado, la rama `cflk/<id>` está en mi repo sin haber cambiado mi checkout, y el Job queda
  `done`.

### Scenario: La conversación no sale de la red
- **Given** un movimiento o un `pull --push`
- **When** termina
- **Then** el origin del repo no tiene ninguna ref ni ningún objeto con la sesión.

### Scenario: Nada se interpreta
- **Given** un prompt, un nombre de rama o una respuesta de peer con comillas, `;`, `$()` o saltos de línea
- **When** viajan por ssh
- **Then** ninguno llega a una shell: el comando remoto es siempre `cflk peer` y lo demás va en stdin.

### Scenario: Todo desde Claude Code
- **Given** el servidor MCP en perfil `write`
- **When** el modelo encadena `jobs_run`, `jobs_list`, `jobs_screen`, `jobs_send`, `jobs_move` y `jobs_pull`
- **Then** cada uno hace lo mismo que su comando y devuelve el Job resultante; en perfil `read` las herramientas que
  cambian algo no existen y `jobs_attach` abre siempre en solo lectura, aunque se le pida otra cosa.

### Scenario: El Controller ya no está
- **Given** el repo tras este cambio
- **When** busco `internal/controller`, `cflk controller` o la unidad de servicio del Controller
- **Then** no existen, y el runbook de retirada lista los pasos para apagar y borrar su infraestructura.

## Preguntas abiertas

- **Nombre:** «Job» en lugar de Task/Run. ¿Otro?
- **Versión:** v0.5.0, sin compatibilidad con la beta.8.
