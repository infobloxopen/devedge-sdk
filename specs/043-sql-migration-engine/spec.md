# F043 — Versioned SQL migrations via `infobloxopen/migrate` through the embedded migrations seam (WS-022 P1)

**Status**: CLARIFIED — all ★ forks locked 2026-07-02 (ready for Plan) · WS-022 **P1 / keystone**
**Engine**: `infobloxopen/migrate` (org fork of `golang-migrate/migrate/v4`; `ib` branch adds
persisted-down + dirty-state recovery) · **DBs**: PostgreSQL (primary), MySQL (ent/GORM path),
SQLite (dev/test only)

> **Guiding principle (locked, per SDK convention):** clean implementation, **no backward
> compatibility** — the SDK is pre-1.0 with no real users. The versioned-SQL path supersedes
> AutoMigrate for the schema-of-record; do not preserve superseded patterns beyond a dev fast-path.

**Extends**: F027 (generated repository adapter, `BatchRepository`), WS-005 (canonical schema),
`persistence/gormtx/migrate.go` (`MigrateModule` — advisory-locked, namespace-aware AutoMigrate),
`servicekit` host (`HostConfig.Migrate`, `DatabaseDescriptor.Migrations fs.FS`, per-module namespace
allocation), WS-008 cell tables (outbox `event_seq`/`event_epoch`, tenant-fence).
**Reference model (another repo — do not import):** devedge `internal/migrate/ForkApplier` already
drives the fork correctly (targets `maxVersion`, `WithDirtyStateConfig`, persisted down-store, `pgx5`
scheme, Helm-hook `migrate up`). This feature brings the SDK to parity via the seam it already
exposes.
**Origin**: WS-022 (hub `specs/database-migration-dx-proposal.md`) — the SDK's SQL-migration seam is
plumbed (`//go:embed migrations`, `DatabaseDescriptor.Migrations`, `HostConfig.Migrate`) but **inert**:
framework tables are materialized by GORM `AutoMigrate` + ent `client.Schema.Create`, and
`infobloxopen/migrate` is not in `go.mod`. Docs claiming "migrations use `infobloxopen/migrate`" are
aspirational. This feature makes them true.

---

## Problem statement

A devedge service's schema-of-record must evolve through **versioned, sequentially-numbered SQL
migrations** driven by `infobloxopen/migrate` — the org standard — applied as a **pre-deploy step**,
**near-zero-downtime**, and **safe unattended on large live databases**. Today the SDK does none of
this for the production path:

1. **Framework tables** (outbox, idempotency, cursor/dead-letter, tenant-fence/event-seq) are created
   by `gormtx.MigrateModule` → `AutoMigrate` (and ent `Schema.Create`). AutoMigrate is convenient but
   **not versioned, not reviewable, and not safe on a large live table** (it can issue table-locking
   DDL with no `lock_timeout`, and its behavior drifts with the model).
2. The **SQL-migration seam is inert.** `DatabaseDescriptor.Migrations fs.FS` and
   `HostConfig.Migrate` exist, and the scaffold ships an empty `migrations/` with a `//go:embed`, but
   **nothing drives `infobloxopen/migrate`** over that FS — the dependency isn't even present.
3. There is **no framework baseline in SQL**, so a service adopting versioned migrations has no
   `0001` to build forward from.

This feature (WS-022 P1, SDK side) wires the engine, generates the framework baseline, and makes the
migration connection safe by default. It does **not** ship the `de migrate` CLI, the `squawk`/sequence
linter, or the Helm-hook Job hardening — those are WS-022's devedge-side phases.

---

## Goals

- **G-1 (drive the engine over the seam).** The host applies a module's embedded
  `db/migrations/*.sql` through `infobloxopen/migrate`, reading `DatabaseDescriptor.Migrations`,
  targeting the highest sequential version, with `WithDirtyStateConfig` dirty-state recovery and a
  persisted down-store — parity with devedge's `ForkApplier`.
- **G-2 (framework baseline in SQL).** Framework tables are materialized by a **generated**
  `0001_framework_init.up.sql` / `.down.sql` derived from the framework model set (single source of
  truth = the models), dialect-aware, replacing AutoMigrate on the Postgres/MySQL path. The baseline
  **includes** the WS-008 outbox `event_seq`/`event_epoch` + partition columns (guard the regression).
- **G-3 (safe migration connection).** The migration connection sets `lock_timeout` and
  `statement_timeout` (DSN `options=-c lock_timeout=… -c statement_timeout=…`) so a contended
  migration **fails fast** instead of queueing behind live queries. Distinct from the app pool.
- **G-4 (`CONCURRENTLY` works).** `CREATE INDEX CONCURRENTLY` in its own single-statement migration
  file applies **without** the "cannot run inside a transaction block" error — proven by a fixture.
- **G-5 (namespace + advisory lock preserved).** SQL migrations run within the module's schema
  (`search_path`) / prefix, the `schema_migrations` version table lands per-module, one migrator runs
  at a time (advisory lock), and a co-resident module cannot race or collide — parity with
  `MigrateModule` under tenant + module-namespace isolation.
- **G-6 (real-DB verification).** All migration behavior is verified against **real PostgreSQL**
  (testcontainers), not SQLite; MySQL parity where the framework supports it.

## Non-goals

- **`de migrate {new,lint,up,verify,status}` CLI** — WS-022 devedge phase (P2).
- **The `squawk` + sequence-gap linter** — WS-022 (surfaces via `de migrate lint`); this feature only
  guarantees the engine and file convention the linter checks.
- **Helm pre-install/pre-upgrade hook Job hardening** (backoff/deadline/verify/down-store PVC) —
  devedge deploy phase.
- **Extending the fork's `ib` branch** (per-migration no-tx directive + first-class `lock_timeout`
  options) — a **conditional** upstream deliverable (P1b), pursued only if the single-statement
  workaround proves too sharp; tracked in hub WS-022.
- **Timestamp numbering** — rejected by mandate.
- **App-boot auto-migrate of the schema-of-record** — migrations are a pre-deploy step; app boot may
  *assert* the schema is current, not mutate it.
- **A bespoke migration engine** — `infobloxopen/migrate` is the standard; we drive/extend it.

---

## Design decisions (★ = confirm in Clarify)

- **D-1 (applier home).** Add a `persistence/migrate` package in the SDK (a `ForkApplier`-equivalent)
  that the host invokes from `HostConfig.Migrate`, reading `DatabaseDescriptor.Migrations`. Mirrors
  devedge's `internal/migrate` rather than importing it (no cross-repo dependency). ★ Confirm: new
  package vs. extending `servicekit`'s existing `MigrationRunner` wiring.
- **D-2 (AutoMigrate disposition) — LOCKED (Clarify 2026-07-02: keep SQLite dev fast-path).** On
  Postgres/MySQL the generated `0001` SQL baseline **replaces** AutoMigrate for framework tables;
  AutoMigrate is retained **only** as the **SQLite dev/test fast-path** (SQLite is ephemeral,
  dialect-limited, not a production target). Dev engine differs from prod, but the safety-critical
  migration path is exercised against **real Postgres in CI** (testcontainers, G-6) — no SQLite-dialect
  baseline is emitted.
- **D-3 (baseline generation) — LOCKED (Clarify 2026-07-02: Atlas).** Generate
  `0001_framework_init.{up,down}.sql` with **Atlas** (`ariga.io/atlas`): `atlas migrate diff` from the
  ent schema (ent's native Atlas integration) and from GORM models (via `ariga.io/atlas-provider-gorm`),
  dialect-aware (Postgres required; MySQL where the framework supports it), emitted into an SDK-owned
  migrations FS that composes **ahead of** the module's `db/migrations`. **Atlas is a build-time /
  `go generate` tool only** — invoked via its CLI (like `buf`/`entc`); the GORM provider generator
  lives in an isolated tool package (`//go:build ignore` or a separate tool module) so **neither Atlas
  nor the provider enters the service binary or the root module runtime graph** (`check-graph-isolation`
  stays green — the load-bearing dependency-light principle). **Drift detection is native**:
  `atlas migrate diff` must return an **empty diff** against the committed baseline in CI (replaces a
  hand-rolled "== AutoMigrate output" assertion). ★→resolved.

  **T0 DERISK OUTCOME (2026-07-02) — SINGLE baseline, GORM-sourced (better than per-backend).** Ran
  Atlas v1.2.4 against real docker Postgres for both paths. Finding: the SDK's framework tables have
  exactly ONE source of truth — the **gormtx** framework model set (`OutboxRow`, `OutboxCursorRow`,
  `OutboxDeadLetterRow`, `IdemMarker`, `TenantFenceRow`, `TenantEventSeqRow`, `TenantEventPolicyRow`),
  which carry the composite PK `(id, created_time)`, the WS-008 `event_seq`/`event_epoch` columns, and
  the cell tables. The **default (GORM) scaffold** materializes these through `gormtx` (host-run
  `MigrateModule`), so the GORM-derived DDL IS the canonical framework schema. So the framework
  baseline is generated from the gormtx models via `atlas-provider-gorm` (v0.6.1 — v0.5.x is skewed
  against gorm 1.31 and emits ALTER-without-CREATE) and is backend-agnostic at the physical-SQL level;
  the ent/GORM host paths drive the SAME baseline, and DOMAIN tables (ent `Schema.Create` or domain
  `.sql` at 0002+) layer on top. The ent-native diff WAS run (iam fixture) to confirm non-convergence:
  ent produces structurally different DDL — **pluralized** table names (`outboxes`, `idem_markers`,
  `outbox_cursors`, `outbox_dead_letters`), single-`id` PK, NO `event_seq`/`event_epoch`, NO cell
  tables, ent-style index names — which also confirmed those per-service ent schemas are a *stale
  fixture*, NOT the SDK source of truth. Hence NOT per-backend baselines (`.ent.sql`/`.gorm.sql`) and
  NOT byte-identical convergence: **one canonical `0001_framework_init.{up,down}.sql`** generated from
  GORM. Baseline generator kept build-time-only in a **separate tool module** `persistence/migrate/
  schemagen` (own go.mod) — `//go:build ignore` alone would still pull `atlas-provider-gorm` into the
  applier module's graph via `go mod tidy`, so a distinct module is required for isolation. Atlas dir
  uses `format = golang-migrate` (emits `.up.sql` + `.down.sql`); the timestamped version is normalized
  to `0001` (D-7). Native drift gate: regenerate + `git diff --exit-code` on `baseline/` (deterministic
  because the filename is normalized), since `atlas migrate diff` writes an incremental file rather than
  exiting non-zero on drift. **Engine is backend-agnostic** (applies SQL to a DSN), so the ent and GORM
  host paths drive the SAME applier; the applied schema is consumable by both a gorm client and a raw
  pgx/`database/sql` connection (proven by the AC-1 fixture). **Scope caveat (P1):** the versioned-SQL
  framework baseline is wired on the **GORM scaffold** (its `moduleMigrate` now branches SQLite→
  AutoMigrate dev fast-path / Postgres→SQL engine). The **ent scaffold** still materializes framework
  tables ent-natively via `client.Schema.Create` (ent-shaped `outboxes`/…); converging ent services
  onto the GORM-shaped versioned baseline is a documented follow-on, not P1 — the engine and baseline
  are ready for it.
- **D-4 (safe connection).** The applier normalizes the DSN to the `pgx5` scheme (parity with
  `ForkApplier`/`toPgxURL`) and appends `options=-c lock_timeout=<t> -c statement_timeout=<t>` on the
  **migration** connection only. Defaults ★ (proposal: `lock_timeout=2s`, `statement_timeout=60s`),
  overridable per `DatabaseDescriptor`.
- **D-5 (CONCURRENTLY rule).** Zero-fork: `CREATE INDEX CONCURRENTLY` must be the **only** statement in
  its file (no `SET` line — hence D-4 puts timeouts on the connection). Base golang-migrate has no
  per-file transaction control (`notx_`/`WithDisabledTransactionFor` is a *sql-migrate* feature).
  P1 ships this rule; the ergonomic `ib`-branch directive is **P1b** (conditional).
- **D-6 (namespace + lock) ★.** Set the migration connection's `search_path` to the module schema
  (Postgres) or use the table prefix (parity with `NamespacedPostgresDSN` / `MigrateModule`), so
  `schema_migrations` lands per-module. Hold a single migrator via advisory lock. ★ Confirm:
  reconcile `infobloxopen/migrate`'s own advisory lock with the SDK's `fnv64a(moduleID)` lock — one
  authority, not two.
- **D-7 (sequential numbering + width).** Enforce `NNNN_<desc>.{up,down}.sql`, zero-padded, no gaps.
  ★ Confirm width (proposal: **4-digit** `0001` for headroom; the fork's regex `^[0-9]+_` accepts
  either — this is a convention the scaffold + linter enforce, standardizing devedge's 3-digit vs the
  SDK README's 4-digit).

---

## Acceptance criteria

- **AC-1 (engine drives the seam).** A module whose `DatabaseDescriptor.Migrations` embeds
  `0001_*.up.sql` (+ more) applies through `infobloxopen/migrate` against **real Postgres**
  (testcontainers) to the highest version; `schema_migrations` reflects it; the matching `down`
  reverses cleanly. Verified for both the ent and GORM host paths.
- **AC-2 (framework baseline parity).** Framework tables (outbox incl. `event_seq`/`event_epoch` +
  partitions, idempotency, cursor/dead-letter, tenant-fence) are created by the generated `0001`
  baseline — **not** AutoMigrate — and a drift test asserts the resulting schema matches what
  AutoMigrate produced. Existing outbox/cell fixtures stay green.
- **AC-3 (safe connection).** The migration connection carries `lock_timeout` + `statement_timeout`;
  a test asserts a deliberately blocking `ALTER` **fails fast** (lock timeout) rather than hanging,
  and that the app pool is unaffected.
- **AC-4 (CONCURRENTLY).** A single-statement `CREATE INDEX CONCURRENTLY` migration applies with **no**
  "cannot run inside a transaction block" error; a multi-statement file containing `CONCURRENTLY`
  fails **loud** (documented, and the WS-022 linter will catch it pre-commit).
- **AC-5 (namespace + no race).** In a composed 2-module host, each module's SQL migrations land in
  its own schema/prefix with a per-module `schema_migrations`; concurrent startup of two replicas
  applies each module's migrations **exactly once** (advisory lock), and one module's failed migration
  does not corrupt the other's — parity with `MigrateModule` under module-namespace isolation.
- **AC-6 (dirty-state recovery).** A migration that fails mid-apply leaves a recoverable dirty state
  that the **next corrected run auto-recovers** (`WithDirtyStateConfig`) without manual cleanup;
  the persisted down-store lets a rollback run even when the image no longer ships the down file.
- **AC-7 (docs reconciled).** `persistence/SHAPES.md` + `docs/content/docs/reference/persistence.md`
  describe the **real** engine (versioned SQL via `infobloxopen/migrate`, the `0001` baseline, the
  safe-connection defaults, the `CONCURRENTLY` rule, dirty-state recovery), replacing the aspirational
  text; the `change-database-schema` skill cross-references it.

## Failure modes to cover

- **Multi-statement `CONCURRENTLY` file** → transaction-block error → **loud failure**, not a silent
  wrap or a hang (AC-4). Documented; linter (WS-022) prevents pre-commit.
- **Baseline drift** (models change, `0001` doesn't) → the drift test (AC-2) fails CI; regeneration is
  the fix.
- **SQLite/dev path** → PG-specific SQL (`CONCURRENTLY`, `NOT VALID`) must not run against SQLite; the
  dev fast-path uses AutoMigrate (D-2) — a test asserts the dev path still boots.
- **MySQL dialect** → the baseline for MySQL uses online-DDL-safe forms; if a framework table is
  Postgres-only (e.g. partitioned outbox), MySQL support is **fail-loud "unsupported"**, not silently
  wrong.
- **Two replicas / co-resident modules racing** → advisory lock (AC-5); a single authority (D-6), not
  migrate's lock *and* the SDK's lock fighting.
- **Missing down file on rollback** → persisted down-store (AC-6); without it, rollback of a removed
  migration fails loud.
- **Numbering gap / duplicate** → out of scope for the engine (the linter enforces), but the applier
  must apply in strict numeric order and error on a malformed name (regex `^[0-9]+_.+\.(up|down)\.sql$`).

---

## Phasing (detailed in tasks.md during Plan; each task tagged `[S]`/`[C]`)

1. **[C]** Add `infobloxopen/migrate` (go.mod + `replace`); `persistence/migrate` applier
   (`ForkApplier`-equivalent: `maxVersion` target, `WithDirtyStateConfig`, persisted down-store,
   `pgx5` scheme, connection `search_path` + `lock_timeout`/`statement_timeout`); wire it into
   `HostConfig.Migrate` reading `DatabaseDescriptor.Migrations`. — AC-1, AC-3, AC-5.
2. **[C]** Generate `0001_framework_init` (dialect-aware) from the framework models via **Atlas**
   (`atlas migrate diff`; ent-native + `atlas-provider-gorm`, isolated build-time tool package —
   check-graph-isolation stays green); native `atlas migrate diff`-empty drift gate in CI incl. the
   WS-008 outbox columns; make AutoMigrate the SQLite/dev fast-path only. — AC-2.
3. **[S/C]** Fixtures: single-statement `CONCURRENTLY` proof + dirty-state recovery, on real Postgres
   (testcontainers); GORM + ent parity. — AC-4, AC-6.
4. **[S]** Docs reconcile (`SHAPES.md`, `persistence.md`) + skill cross-reference. — AC-7.

**Upstream coordination (P1b, conditional):** if the single-statement `CONCURRENTLY` rule (D-5) is too
sharp for authors, extend the fork's **`ib` branch** with a per-migration no-transaction directive +
first-class `lock_timeout`/`statement_timeout` options, land via a `go.mod replace` bump, then
simplify the applier + the (WS-022) linter to use the directive. Tracked in hub WS-022; does **not**
block phases 1–4.

**Clarify — RESOLVED 2026-07-02 (all ★ locked):** D-1 new `persistence/migrate` applier; D-2 keep
SQLite AutoMigrate dev fast-path (migrations verified on real Postgres in CI, no SQLite baseline); D-3
Atlas (`ariga.io/atlas`) build-time-only baseline gen (ent-native + `atlas-provider-gorm`, isolated so
the runtime graph stays dependency-light) with native `atlas migrate diff`-empty drift gate; D-4
`lock_timeout=2s`/`statement_timeout=60s` overridable; D-5 zero-fork `CONCURRENTLY`-per-file in P1
(`ib`-branch directive = conditional P1b); D-6 single advisory-lock authority = SDK `fnv64a(moduleID)`;
D-7 4-digit numbering width. **Next gate:** Plan — `/speckit.plan` → `tasks.md` with each task tagged
`[S]`/`[C]`, then `/speckit.analyze` for cross-artifact consistency.
