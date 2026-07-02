---
name: change-database-schema
description: Evolve a service's database safely — add/drop/rename a column, add an index, add or validate a constraint or foreign key, change a column type, make a field required or nullable, or backfill data. Use whenever a change requires the schema to change, instead of reaching for AutoMigrate or a bare ALTER TABLE. Produces a sequentially-numbered, near-zero-downtime migration safe to run unattended on a large, live database.
---

# Change a service's database schema safely

## When this fires

Any change that alters the database — a new field on a resource, an index, a constraint, a type
change, a rename, a backfill. The danger is that these feel invisible (AutoMigrate "just does it").
They are not: on a large, live table the wrong `ALTER` is an outage. Every schema change becomes a
**versioned, sequentially-numbered migration** applied by `infobloxopen/migrate` as a pre-deploy
step — never AutoMigrate for the schema-of-record, never a bare `ALTER` on a hot table.

## The non-negotiables

- **Sequential numbering.** `NNNN_<desc>.up.sql` + `NNNN_<desc>.down.sql`, zero-padded, no gaps, no
  timestamps. If your PR collides with another `NNNN`, renumber yours — the conflict is the *signal*
  that two changes race on the same schema, not a nuisance. (Timestamps hide that race and apply in
  clock order, not intent order.)
- **`lock_timeout` on the migration connection**, not per file (DSN `options=-c lock_timeout=2s -c
  statement_timeout=60s`). A contended migration then *fails fast* and the deploy Job retries in a
  quiet window, instead of queueing behind live queries and stalling the database.
- **Ship the inverse `down`.** One logical change per migration.

## Pick the safe recipe (Postgres)

`AccessExclusiveLock` (taken by `ALTER TABLE`, `DROP`, non-concurrent `CREATE INDEX`) queues **every**
new query behind it — a 30s change on a hot table is an outage even though the lock is "only" 30s.
Avoid it or hold it only for a catalog update (no scan):

| Intent | Safe recipe |
|--------|-------------|
| Add nullable column | `ADD COLUMN col TYPE` (no default) — instant |
| Add column w/ default | `ADD COLUMN col TYPE DEFAULT <constant>` (PG 11+, no rewrite); volatile default → backfill instead |
| Add index | `CREATE INDEX CONCURRENTLY IF NOT EXISTS ...` **alone in its own file** |
| Drop index | `DROP INDEX CONCURRENTLY IF EXISTS ...` alone in its file |
| Add FK / CHECK | `ADD CONSTRAINT ... NOT VALID` (fast, no scan) → a later migration `VALIDATE CONSTRAINT` (allows reads+writes) |
| Make `NOT NULL` | `ADD CONSTRAINT chk CHECK (col IS NOT NULL) NOT VALID` → backfill → `VALIDATE` → `SET NOT NULL` (PG 12+ skips the scan) → drop chk |
| Change column type | expand-contract: add new col, dual-write + backfill, switch reads, drop old — **never** bare `ALTER COLUMN TYPE` (full rewrite) |
| Rename column/table | expand-contract — **never** bare `RENAME` (instant app break) |
| Backfill | batched app code / one-shot Job (keyset cursor + `LIMIT` + short sleep) — **never** an unbounded `UPDATE` in a migration |
| Drop column | `DROP COLUMN` only **after** the app stops reading it (expand-contract on the app side) |

## The `CONCURRENTLY` transaction rule (read this)

`infobloxopen/migrate`'s Postgres driver runs a file in one `Exec`, and Postgres treats multiple
statements as **one implicit transaction** — so `CREATE INDEX CONCURRENTLY` errors with "cannot run
inside a transaction block" unless it is the **only statement in the file** (and therefore has **no
`SET` line** — that would be a second statement). Put each `CONCURRENTLY` statement in its own
migration file; that's why `lock_timeout` lives on the connection, not in the file.

> If the fork gains a per-migration no-transaction directive (an `ib`-branch capability), prefer it —
> it makes `CONCURRENTLY` ergonomic and lint-detectable. Until then, one statement per file is the
> rule.

## MySQL (ent / gorm MySQL path)

No `CONCURRENTLY` / `NOT VALID`. Use online DDL and **assert** it so an accidental table copy fails
loudly:

```sql
ALTER TABLE t ADD INDEX idx_t_col (col), ALGORITHM=INPLACE, LOCK=NONE;
```

`ADD COLUMN` is `INSTANT` (8.0.12+). Type changes, PK changes, and fulltext/spatial indexes are
`COPY` — reach for `gh-ost` / `pt-online-schema-change`, and watch **replica lag** (the tools
throttle on it). squawk is Postgres-only, so on MySQL the `ALGORITHM/LOCK` assertion + the
online-schema-change tool are your linter.

## Author + verify

- **Create:** `de migrate new <name>` scaffolds the up/down pair at the next number. (No `de migrate`
  yet? Add `db/migrations/NNNN_<name>.up.sql` + `.down.sql` by hand, same convention.)
- **Lint before commit:** `de migrate lint` runs `squawk` (Postgres) + the sequence-gap check.
  squawk flags blocking `CREATE INDEX`, missing `NOT VALID`, `ADD COLUMN NOT NULL` without a safe
  default, `ALTER COLUMN TYPE`, and `RENAME`. Suppress a vetted exception with
  `-- squawk:ignore-next-statement reason="small lookup table, < 1000 rows"`. (No `de migrate lint`
  yet? Run `squawk db/migrations/*.up.sql` directly.)
- **Test against real Postgres** (testcontainers), not SQLite — SQLite silently accepts DDL Postgres
  rejects and hides lock behavior.
- **Dry-run:** `de migrate up` against a scratch DB; confirm `de migrate status` shows the new
  version and that `down` reverses cleanly.

## How it runs unattended (design for it)

Migrations run as a **pre-deploy Job** (Helm pre-install/pre-upgrade hook), not from app boot: one
migrator at a time (advisory lock); dirty state auto-recovers on the next run and the down-store
survives image upgrades (both from the `infobloxopen/migrate` fork); the Job has a backoff + deadline.
Make every statement **idempotent** — `IF NOT EXISTS` / `IF EXISTS`, and guard `ADD CONSTRAINT` with
`DO $$ BEGIN ... EXCEPTION WHEN duplicate_object THEN NULL; END $$;`.

## Known gotchas

- A failed `CREATE INDEX CONCURRENTLY` leaves an **INVALID** index — the `down` must
  `DROP INDEX CONCURRENTLY IF EXISTS ...` and you re-run.
- `SET NOT NULL` without a prior *validated* `CHECK` does a full-table `AccessExclusive` scan — always
  go through the `CHECK ... NOT VALID → VALIDATE → SET NOT NULL` dance on a big table.
- **Framework tables (outbox / idempotency / tenant-fence) are materialized by the SDK's generated
  `0001_framework_init` baseline** (in `persistence/migrate/baseline/`, composed AHEAD of your module's
  migrations; generated from the canonical models with Atlas + drift-checked by
  `make check-migration-baseline`). `0001` is therefore RESERVED — **your first migration is `0002`**.
  Your migrations own only your domain tables; don't hand-migrate framework tables, and don't reuse
  `0001` (the engine fails loud on a duplicate version).
- For a 100M+ row rewrite that no recipe avoids, `pg-osc` (Postgres) / `gh-ost` (MySQL) shadow-table
  swap is the escape hatch — out of band from the normal migration path.

## Reference

- Engine: `infobloxopen/migrate` (org fork — dirty-state recovery + persisted down-store).
- `persistence/SHAPES.md`, `docs/content/docs/reference/persistence.md`.
- squawk rule reference (Postgres migration linter).
