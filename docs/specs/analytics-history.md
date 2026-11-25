---
codename: codefabric
title: Analytics e historial
type: task
status: draft
priority: P2
size: S
issue: null
prs: []
related_issues: []
sub_issues: []
depends_on: [evaluators, usage-attribution]
supersedes: []
related_specs: [codefabric-spec, phase-7-experiments]
created: 2026-10-10
updated: 2026-11-25
---

# Analytics e historial

> **Pendiente de reescribir para la red de peers (v0.5, sin Controller).** Lo que se escribió aquí supone un Controller. La versión p2p: `cflk stats` a partir del historial de Jobs (`history`/`gen`) de todos los peers, por harness, peer o repo. Ver [la hoja de ruta](../ROADMAP.md).

## Contexto / problema

Milestone de la [Fase 7 — Experiments](archive/2026-11-25-phase-7-experiments.md). Implementa §39 de `docs/codefabric-spec.md`.

Owner previsto en el bucle Develop: backend-eng.

## Scope

**Entra:**
- `cf run stats --by workflow|model|harness|project [--since]`

**No entra:**
- Nada fuera de lo indicado en la epic.

## Criterios de aceptación

Cada scenario se implementa como un test `TestScenario_*`.

### Scenario: Success rate
- **Given** un fixture de Runs
- **When** se calcula por workflow
- **Then** es correcto

### Scenario: Queue wait
- **Given** eventos de cola
- **When** se calcula
- **Then** se deriva de los eventos

### Scenario: Muestra
- **Given** muestra insuficiente
- **When** se muestra
- **Then** marcada como tal

## Notas técnicas / riesgos

- Respetar las reglas de capas (ADR 0001) y las decisiones D1–D7 (ADR 0002).
- Transacción única para estado, evento e idempotencia (§27.4); reloj inyectable en todo timeout.

## Referencias

- `docs/codefabric-spec.md` §39
