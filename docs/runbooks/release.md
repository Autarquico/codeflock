# Runbook: publicar una release

Code Flock: [autarquico.github.io/codeflock](https://autarquico.github.io/codeflock/).

Spec: [release-installer-site](../specs/release-installer-site.md). Todo lo que sigue es manual salvo lo que
hacen `release.yml` y `pages.yml` una vez empujado el tag. Los pasos 1 a 4 bloquean el tag `v0.2.0`, no los PRs.

Convenciones fijas: principal `release@codefabric`, namespace `codefabric-release`, repo `Autarquico/codeflock`.

## 1. Generar la clave y custodiarla

```sh
cd "$(mktemp -d)"   # fuera del repo: que ningún git add pueda recoger la clave
ssh-keygen -t ed25519 -N '' -C release@codefabric -f codefabric-release
```

- Usa una máquina de confianza. El secret de GitHub necesita la clave sin passphrase. Para la copia de custodia,
  protégela con `ssh-keygen -p -f <copia>` antes de archivarla (o guárdala en el gestor de contraseñas) y borra
  los ficheros de trabajo al terminar.
- `codefabric-release` (privada) se guarda offline o en el gestor de contraseñas del owner. Es la única copia
  fuera del secret del paso 3. No va al repo, ni a otro secret, ni a otro workflow.
- `codefabric-release.pub` es pública. Se embebe en `install.sh` (paso 2).

## 2. Sustituir el placeholder de `install.sh`

Hasta este paso `install.sh` falla cerrado (sale con 5): la raíz de confianza es un placeholder.

En `install.sh`, entre los marcadores exactos:

```
# BEGIN allowed_signers
release@codefabric namespaces="codefabric-release" ssh-ed25519 PLACEHOLDER
# END allowed_signers
```

reemplaza la línea del medio por:

```
release@codefabric namespaces="codefabric-release" <tipo> <clave en base64>
```

con `<tipo> <clave>` = los dos primeros campos de `codefabric-release.pub` (`ssh-ed25519 AAAA...`). No
toques los marcadores ni el bloque `revoked_keys`. Abre un PR; CI (`installer`) debe quedar en verde y el
placeholder ya no debe existir (`grep -c 'ssh-ed25519 PLACEHOLDER' install.sh` da 0; el comentario de la
línea 10 de `install.sh` menciona la palabra PLACEHOLDER y no cuenta). Mergea antes de etiquetar: el tag
debe apuntar a un commit que ya lleve la clave real, porque `release.yml` verifica contra el `install.sh` de ese commit.

## 3. Environment `release` y su secret

En GitHub, Settings > Environments > New environment, nombre `release`:

- Required reviewers: el owner.
- Deployment branches and tags: regla de tag `v*` (y ninguna rama).
- Desactiva «Allow administrators to bypass configured protection rules» y crea un ruleset de tags `v*` que
  restrinja crear, actualizar y borrar al owner.
- Secret `CF_RELEASE_SIGNING_KEY`: contenido completo de la clave privada `codefabric-release`
  (con las líneas `-----BEGIN/END OPENSSH PRIVATE KEY-----`).

Solo el job `sign-publish` de `release.yml` referencia este environment y este secret; el job
`workflow-policy` de `ci.yml` lo comprueba.

## 4. Activar Pages

Settings > Pages > Source: **GitHub Actions**. `pages.yml` publica el sitio cuando `release` termina bien en un tag `v*`.

## 5. Hacer el tag

Con el commit ya en `main` y todos los checks requeridos de `ci.yml` en verde (`packaging/release/required-checks.txt`):

```sh
git switch main && git pull --ff-only
git tag vX.Y.Z
git push origin vX.Y.Z
```

Un tag con `-` (`v0.2.0-rc.1`) sale como prerelease. `latest` ignora prereleases y no hay fórmula para ellas.

`release.yml` hace, en orden:

1. `gates`: todos los checks de `required-checks.txt` deben estar en `success` en el commit del tag; si no, no sigue.
2. `build`: GoReleaser, sin secretos ni escritura; registra el digest de los tarballs.
3. `sign-publish` (pide la aprobación del owner en el environment `release`): comprueba que el tag está en
   `main` (`tag-in-main.sh`), que el artefacto coincide con el digest del build, escribe y firma
   `checksums.txt` e `install.sh` (`finalize.sh`), verifica contra la raíz embebida en `install.sh` (`check.sh`),
   crea la release en borrador, la verifica tal como quedó publicada (`verify-release.sh`) y solo entonces la publica.
   Si la verificación falla, borra el borrador.

Aprueba el despliegue solo tras comprobar que el tag y el commit son los esperados.

### v0.5.0-beta.1 (la red de peers, sin Controller)

Es la primera release sin Controller ni Worker. El flujo, en orden, con los pasos 1 a 4 ya hechos:

1. La rama de trabajo es `next`. Comprueba antes que la prueba real de la flota está documentada y que
   [retire-controller](retire-controller.md) está al día con `cflk doctor --fix`.
2. Abre el PR `next` → `main` (`gh pr create --base main --head next`). Todos los checks de
   `packaging/release/required-checks.txt` deben quedar en verde en el commit que va a ser el tag. El tag se pone
   sobre `main` después del merge (`tag-in-main.sh` lo exige).
3. `git switch main && git pull --ff-only && git tag v0.5.0-beta.1 && git push origin v0.5.0-beta.1`. Lleva `-`:
   sale como prerelease y `latest` no cambia.
4. `release.yml` se detiene en `sign-publish` esperando la aprobación del environment `release`. Abre el run
   (`gh run list --workflow release.yml --limit 1`), comprueba que el tag y el commit son los esperados y aprueba.
5. Verifica la release publicada (paso 6) en una máquina limpia y en otra con el Controller 0.4 todavía en marcha:
   el instalador sustituye `cflk` y **no** toca el Controller ni el Worker, y `cflk doctor` los lista como avisos.
6. El Controller sigue en marcha hasta que valides la red de peers. Cuando lo hagas, cada máquina ejecuta
   `cflk doctor --fix` y el host del Controller sigue [retire-controller](retire-controller.md).

#### Prueba real de la flota (2026-11-24, `next` en 392d36d)

Con `cflk` de `next` instalado como `~/.local/bin/cflk-next` (y `[cflk_path]` apuntando a él) en un MacBook
(darwin-arm64), omarch01 (linux-amd64) y un Mac mini (darwin-amd64), sobre el repo de pruebas `cflk-sandbox`:

| Paso | Resultado |
| --- | --- |
| `cflk peers` | los tres `reachable` |
| `run --on omarch01` (Claude), `screen`, `send` | ok |
| `pull` (omarch01 → MacBook) y `move --to omarch01` | ok: la conversación y los ficheros sin commit viajan en los dos sentidos |
| `run --on omarch01 --harness codex`, `pull`, `pull --finish`, `clean` | ok: la rama `cflk/<job>` llega al repo sin tocar el checkout |
| `cflk mcp serve`: `tools/list`, `jobs_list` | ok |

Lo que encontró y se arregló en #230:

- Un Job que volvía a un peer donde ya había estado se quedaba en `moving`.
- Claude, arrancado por ssh en un Mac, abría "Not logged in": su login está en el llavero. La guía explica el
  remedio.

Sin probar en la flota:

- moves entre el Mac mini y omarch01: sin ssh entre ellos;
- devolver un Job de Codex al peer del que vino;
- `attach`, `tui` y `cflk mcp install`;
- omarch02.

#### Prueba real de la flota (2026-11-25, v0.5.0-beta.1)

Con `cflk 0.5.0-beta.1` en el MacBook, omarch01 y el Mac mini, y ssh entre todos los pares:

| Paso | Resultado |
| --- | --- |
| ssh omarch01 ↔ Mac mini | ok tras dos ajustes: la clave de omarch01 en el Mac mini, y en Tailscale SSH una regla `accept` para usuarios no root (la `check` por defecto se queda esperando un login en el navegador; ver la guía) |
| Claude: Job lanzado en omarch01, `move --to mac-mini` desde omarch01 | ok: recuerda la conversación (FALCON) |
| Claude: Job lanzado en el Mac mini, `move --to omarch01` desde el Mac mini | ok: recuerda la conversación (BADGER) |
| `pull --finish` de un Job en el Mac mini, y `clean` en cada peer | ok, cuando el Mac mini tiene `peers` en su config y ssh al MacBook |
| `tui` | ok: lista los Jobs de los tres peers |
| `cflk mcp install --claude`, y `peers_list` y `jobs_run` desde Claude Code | ok; una registración de la 0.4 hay que quitarla antes a mano |
| `cflk doctor --fix` en los tres | ok: retira el Worker de omarch01 y los binarios `cf`/`cf.prev` |

Lo que encontró, sin arreglar todavía:

- `doctor` y la lista de peers ofrecen `claude` con un login caducado: solo miran que exista.
- `cflk ps` sigue listando como `moved` Jobs ya limpiados.
- `mcp install` no sustituye su propia registración de la 0.4.
- `doctor` aún recomienda "un Worker dedicado".
- En el Mac mini, `~/.local/bin` no está en el `PATH` de un shell de login.

Sin probar en la flota: `attach`, devolver un Job de Codex al peer del que vino, y omarch02.

La release publica solo `cflk` para linux y darwin, amd64 y arm64 (`.goreleaser.yaml`); no hay binario `cf`.

Después `pages.yml` construye y despliega el sitio con la versión del tag.

## 6. Verificar una release a mano

Con la release ya publicada:

```sh
curl -fsSL https://github.com/Autarquico/codeflock/releases/download/vX.Y.Z/install.sh | CFLK_VERSION=vX.Y.Z sh
```

Para revisar antes de ejecutar y comprobar las firmas sin instalar:

```sh
repo=/ruta/al/checkout-en-el-commit-del-tag   # raíz de confianza: nunca el install.sh descargado
d=$(mktemp -d) && cd "$d"
for f in install.sh install.sh.sig checksums.txt checksums.txt.sig; do
  curl -fsSLO "https://github.com/Autarquico/codeflock/releases/download/vX.Y.Z/$f"
done
"$repo/packaging/release/verify-asset.sh" checksums.txt "$repo/install.sh"
"$repo/packaging/release/verify-asset.sh" install.sh "$repo/install.sh"
```

El conjunto completo se comprueba, con el mismo `repo=` definido arriba, con
`"$repo/packaging/release/verify-release.sh" vX.Y.Z "$repo/install.sh"`
(necesita `gh`; el segundo argumento es el `install.sh` del repo, no el descargado).
`verify-asset.sh` sale con 5 si la firma no verifica. `verify-release.sh` y `check.sh` salen con 1 en cualquier
fallo (inventario, versión, firma, checksum de un archivo) y con 2 si se usan mal: cualquier salida distinta de 0 =
no instales.

## 7. Publicar la fórmula de Homebrew

CI no tiene token del tap; es un paso humano. Si `autarquico/homebrew-tap` no existe, créalo (repo público con
una carpeta `Formula/`).

```sh
repo=/ruta/al/checkout-en-el-commit-del-tag   # raíz de confianza: nunca el install.sh descargado
f=$(mktemp -d)
gh release download vX.Y.Z -R Autarquico/codeflock -p 'checksums.txt*' --dir "$f"
"$repo/packaging/release/brew-formula.sh" "$f/checksums.txt" "$repo/install.sh" vX.Y.Z > "$f/codeflock.rb"
echo "$f/codeflock.rb"
```

`checksums.txt` y `checksums.txt.sig` deben estar juntos; `$repo/install.sh` es el del repo en el commit del tag
(nunca el de la release; tras una revocación, el de `main`). El script
verifica la firma antes de leer nada y rechaza versiones con sufijo, plataformas que faltan o repetidas, y una
cabecera distinta de `vX.Y.Z`. No emite nada si falla.

Después, en el tap. `brew audit` e `install` resuelven la fórmula desde el tap instalado, así que se trabaja
dentro de ese checkout para probar el fichero recién copiado:

```sh
rb=/ruta/absoluta/al/codeflock.rb   # la que imprimió el bloque anterior
brew tap autarquico/tap
cd "$(brew --repository autarquico/tap)"
cp "$rb" Formula/codeflock.rb
brew audit --strict --online autarquico/tap/codeflock
brew install autarquico/tap/codeflock && brew test autarquico/tap/codeflock
git add Formula/codeflock.rb
```

Ese bloque deja la fórmula en el índice y no hace commit ni push. Después, un único commit:

- **Versión normal:** `git commit -m "codeflock X.Y.Z" && git push`.
- **La primera vez tras el rename (la primera versión estable):** en ese mismo commit, sin ningún commit previo solo con la fórmula, se borra `Formula/codefabric.rb` y se copia `packaging/homebrew/formula_renames.json` a la raíz del tap, para que
`brew upgrade` de quien tenga `codefabric` instale `codeflock` (ver `packaging/homebrew/README.md`). El índice
tiene que incluir los tres cambios:

```sh
repo=/ruta/al/checkout-en-el-commit-del-tag
git rm Formula/codefabric.rb
cp "$repo/packaging/homebrew/formula_renames.json" formula_renames.json
git add Formula/codeflock.rb formula_renames.json
git status --short   # A codeflock.rb, D codefabric.rb, A formula_renames.json
git commit -m "codeflock X.Y.Z" && git push
```

## 8. Rotación y revocación de la clave

- **Rotación** (sin sospecha de compromiso): genera la clave nueva (paso 1), añade su línea a
  `allowed_signers` en `install.sh` junto a la anterior, actualiza el secret y haz release. Cuando las
  versiones firmadas con la anterior ya no hagan falta, quita su línea.
- **Revocación** (clave comprometida): añade la clave pública, en una línea (`<tipo> <clave>`), dentro del
  bloque `# BEGIN revoked_keys` / `# END revoked_keys` de `install.sh` y retírala de `allowed_signers`. Mergea por
  PR, rota el secret de `release` y publica una release nueva. Todos los verificadores (`install.sh`,
  `check.sh`, `verify-asset.sh`, `brew-formula.sh`) pasan `-r revoked_keys`, así que lo firmado con esa clave deja de
  verificar con el `install.sh` nuevo. Un `install.sh` antiguo descargado antes no se entera: avisa a los usuarios. Revisa también la fórmula ya
  publicada en `autarquico/homebrew-tap`: vuelve a derivarla con `brew-formula.sh` usando el `install.sh` de `main`
  como TRUST; si falla, republícala desde la release nueva.
- `latest` no está firmado (la redirección de `releases/latest`): quien pueda alterarla puede servir una
  release anterior, firmada y válida. Se acepta hasta que haya canales firmados; fijar `CFLK_VERSION` lo evita.

## Trazabilidad: scenario a test o job

Los tests viven en `packaging/release/test/` (`make test-installer`; el job `installer` de `ci.yml` los ejecuta
e imprime `--- PASS: TestScenario_*`).

| Scenario de la spec | Tests (`TestScenario_...`) | Job |
|---------------------|----------------------------|-----|
| Instalación verificada | `VerifiedInstall`, `InstallTarget_*`, `LatestVersion_*`, `PortableAwk_*`, `VerifiedInstall_replacesPreviousBinary`, `_hintsWhenDirNotOnPath`, `_armLinux`, `_darwinArm64`, `_darwinAmd64`, `_realBinary` (`installer_test.sh`) | `installer` (`ci.yml`) |
| Firma o checksum inválidos | `BadSignatureOrChecksum_tamperedChecksums`, `_signedByOtherKey`, `_archiveShaMismatch`, `_missingSignature`, `_revokedKey`, `_wrongPrincipal`, `_wrongNamespace`; endurecimiento en `Hardening_*` (`installer_test.sh`) | `installer` |
| Cabecera de otra versión | `HeaderOtherVersion` (`installer_test.sh`) | `installer` |
| Plataforma no soportada | `UnsupportedPlatform_freebsd`, `_unknownArch` (`installer_test.sh`) | `installer` |
| Placeholder sin clave | `Placeholder` (`installer_test.sh`) | `installer` |
| Puertas de la release | `ReleaseGates_*` (`gates_test.sh`: `require-checks.sh` y `tag-in-main.sh`) | `gates` y `sign-publish` (paso «tag must point into main») de `release.yml`; `installer` ejecuta los tests |
| Conjunto publicado | `PublishedSet_*` (`published_test.sh`: `check.sh` y `verify-release.sh`) | `sign-publish` de `release.yml` (`check.sh`, `verify-release.sh`); `installer` ejecuta los tests |
| Fórmula derivada | `FormulaDerivada_valid`, `_validWithoutExplicitVersion`, `_invalidSignature`, `_tamperedChecksums`, `_suffixedVersion`, `_suffixedRequestedVersion`, `_missingPlatform`, `_duplicatedPlatform`, `_invalidDigest`, `_headerVersionMismatch` (`brew_test.sh`) | `installer` |
| Sitio desde la release | `TestScenario_SitioDesdeLaRelease` (nombre de los jobs): tests unitarios de `site/scripts`, build, comandos de instalación (sin publicar en PRs, con el tag en la release), axe (serious/critical) sobre la landing y una página de docs | `site` de `site.yml` (en PRs) y `build` de `pages.yml` (en la release) |

Lo que no se ha ejercitado con una release real: la firma con la clave del owner, el environment `release` y el
despliegue de Pages. La cadena completa solo se ha probado con una clave efímera de tests.
