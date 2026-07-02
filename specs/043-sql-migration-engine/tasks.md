# F043 tasks — versioned SQL migrations via infobloxopen/migrate (WS-022 P1)

Each task tagged **[S]** (simple/mechanical → Sonnet-eligible) or **[C]** (complex → Opus). Gate:
build + `go test ./...` + `-race` on real Postgres (testcontainers) + `check-graph-isolation` +
`GOWORK=off` all green before "done"; scope diffed against the spec's acceptance criteria.

## T0 — Derisk (do FIRST, report before proceeding)
- **T0.1 [C]** Prove Atlas can diff the **ent** framework schema **and** the **GORM** framework models
  to the **same** `0001` DDL for Postgres. If ent-native diff and `atlas-provider-gorm` cannot converge
  on identical DDL, decide per-backend baselines (`0001_framework_init.pg.ent.sql` vs `.gorm.sql`) and
  record the deviation in `spec.md` before building further. Blocks T2.

## T1 — Engine wired through the seam (AC-1, AC-3, AC-5)
- **T1.1 [C]** Add `github.com/golang-migrate/migrate/v4` + `replace => infobloxopen/migrate/v4`
  (`ib`-branch pin) to the module that already carries the DB drivers (NOT the root module — keep the
  root dependency-light; `check-graph-isolation` must stay green). Confirm module placement.
- **T1.2 [C]** `persistence/migrate` applier mirroring devedge `internal/migrate/ForkApplier` (no
  cross-repo import): `New("file://"+dir, dbURL)`, target `maxVersion` of the FS, `WithDirtyStateConfig`
  (dirty recovery + persisted down-store), `pgx5` scheme normalize (`toPgxURL`).
- **T1.3 [C]** Safe migration connection: append `options=-c lock_timeout=2s -c statement_timeout=60s`
  (overridable via `DatabaseDescriptor`) + set `search_path` to the module schema so `schema_migrations`
  lands per-module. Reconcile locking → single authority = SDK `fnv64a(moduleID)` advisory lock (do not
  double-lock with migrate's own).
- **T1.4 [C]** Wire the applier into `servicekit.HostConfig.Migrate` reading
  `DatabaseDescriptor.Migrations fs.FS`; runs once per module before Register (parity with the
  AutoMigrate path). SQLite/dev → AutoMigrate fast-path (D-2); PG/MySQL → SQL engine.

## T2 — Framework baseline via Atlas (AC-2)
- **T2.1 [C]** Build-time-only Atlas generator: isolated tool package (`//go:build ignore` or a tool
  sub-module) that runs `atlas migrate diff` (ent-native + `atlas-provider-gorm`) → emits
  `0001_framework_init.{up,down}.sql` into an SDK-owned migrations FS composed **ahead of** the module
  FS. **Neither Atlas nor the provider may enter the service binary / root runtime graph** —
  `check-graph-isolation` green is the gate.
- **T2.2 [C]** Baseline includes the WS-008 outbox `event_seq`/`event_epoch` + partition columns,
  idempotency, cursor/dead-letter, tenant-fence. Native drift gate: `atlas migrate diff` returns
  **empty** against the committed baseline in CI.
- **T2.3 [S]** `make generate` (or a `go generate` target) regenerates the baseline; wire into the
  repo's generate flow + CI.

## T3 — Fixtures on real Postgres (AC-4, AC-6)
- **T3.1 [C]** testcontainers Postgres fixture: apply `0001` + a domain migration to `maxVersion`;
  assert `schema_migrations`; `down` reverses. Both ent + GORM host paths (AC-1).
- **T3.2 [S]** `CONCURRENTLY` proof: a single-statement `CREATE INDEX CONCURRENTLY` migration applies
  with no "cannot run inside a transaction block" error; a multi-statement CONCURRENTLY file fails loud
  (AC-4).
- **T3.3 [C]** Dirty-state recovery fixture: a mid-apply failure leaves a recoverable dirty state that
  the next corrected run auto-recovers; persisted down-store enables rollback of a removed file (AC-6).
- **T3.4 [C]** Namespace/no-race: 2-module composed host — each module's migrations land in its own
  schema/prefix with a per-module `schema_migrations`; two concurrent replicas apply exactly once
  (advisory lock); one module's failure doesn't corrupt the other (AC-5).

## T4 — Docs (AC-7)
- **T4.1 [S]** Reconcile `persistence/SHAPES.md` + `docs/content/docs/reference/persistence.md` to the
  real engine (versioned SQL, `0001` baseline, safe-connection defaults, CONCURRENTLY rule, dirty-state
  recovery); cross-reference the `change-database-schema` skill.

## Out of scope (other WS-022 / WS-023 phases)
`de migrate` CLI, squawk/sequence linter, Helm-hook Job hardening (devedge); scaffold thin-Makefile +
`de sync` (WS-023). The `ib`-branch per-migration no-tx directive is **conditional P1b** — pursue only
if T3.2 shows the single-statement rule is too sharp.
