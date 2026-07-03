---
title: Change the database schema
weight: 5
---

This page shows how to evolve a running service's PostgreSQL schema with a versioned, reversible
migration instead of `AutoMigrate` or a bare `ALTER`. You author a numbered `up`/`down` SQL pair with
`de migrate`, pick a recipe that does not lock a hot table, lint it, and apply it. The migration runs
unattended as a pre-deploy step, so it must be safe on a large, live database.

Use this page whenever a change alters the schema of record — a new field, an index, a constraint, a
type change, a rename, or a backfill. For how migrations are applied and composed with the framework
baseline, see the [persistence reference](../../../reference/persistence/#migrations). For the SQLite
dev fast-path (`AutoMigrate`), see [storage shapes](../storage-shapes/).

## Prerequisites

- A `devedge-sdk new service` project. Its migrations live in `module/migrations/` and are embedded
  into the binary through the module's `//go:embed migrations`.
- The `de` CLI on your `PATH`.
- A PostgreSQL DSN for testing (`postgres://…`). SQLite silently accepts DDL that PostgreSQL rejects,
  so validate against real PostgreSQL.

{{< callout type="warning" >}}
**`0001` is reserved for the framework baseline; your first migration is `0002`.** The SDK composes a
generated `0001_framework_init` (outbox, idempotency, tenant-fence tables) ahead of your module's
migrations. Do not reuse `0001` or hand-migrate framework tables — the engine fails loud on a
duplicate version.
{{< /callout >}}

## 1. Scaffold the migration pair

```bash
de migrate new add_tasks_table
```

`de migrate new` writes the next-numbered pair into `module/migrations/` (`0002_add_tasks_table.up.sql`
and `.down.sql`). It detects the scaffold layout automatically; pass `--dir` only for a non-standard
location. Ship the inverse `down`, and keep one logical change per migration.

## 2. Write a recipe that does not lock a hot table

`ALTER TABLE`, `DROP`, and a non-concurrent `CREATE INDEX` take an `AccessExclusiveLock` that queues
every new query behind them — a 30-second change on a busy table is an outage. Use the safe recipe for
your intent:

| Intent | Safe recipe |
|--------|-------------|
| Add nullable column | `ADD COLUMN col TYPE` (no default) — instant |
| Add column with default | `ADD COLUMN col TYPE DEFAULT <constant>` (PG 11+, no rewrite); a volatile default → backfill instead |
| Add index | `CREATE INDEX CONCURRENTLY IF NOT EXISTS …` — alone in its own file |
| Drop index | `DROP INDEX CONCURRENTLY IF EXISTS …` — alone in its own file |
| Add FK or CHECK | `ADD CONSTRAINT … NOT VALID` (fast, no scan), then a later migration `VALIDATE CONSTRAINT` |
| Make `NOT NULL` | `ADD CONSTRAINT chk CHECK (col IS NOT NULL) NOT VALID` → backfill → `VALIDATE` → `SET NOT NULL` → drop chk |
| Change column type | Expand-contract: add a new column, dual-write and backfill, switch reads, drop the old one — never a bare `ALTER COLUMN TYPE` |
| Rename column or table | Expand-contract — never a bare `RENAME`, which breaks the running app instantly |
| Backfill | Batched application code or a one-shot Job (keyset cursor + `LIMIT` + short sleep) — never an unbounded `UPDATE` in a migration |
| Drop column | `DROP COLUMN` only after the app stops reading it |

Make every statement idempotent (`IF NOT EXISTS` / `IF EXISTS`) so an unattended retry is safe.

{{< callout type="warning" >}}
**`CREATE INDEX CONCURRENTLY` must be the only statement in its file.** The migration driver runs a
file as one implicit transaction, and PostgreSQL rejects `CONCURRENTLY` inside a transaction block. A
file that mixes it with any other statement — including a `SET` line — fails. Keep the connection-level
`lock_timeout` out of the file; the engine sets it on the connection.
{{< /callout >}}

## 3. Lint before you commit

```bash
de migrate lint
```

`de migrate lint` checks numbering (no gaps, matched `up`/`down` pairs) and rejects a `CONCURRENTLY`
statement mixed with others. When `squawk` is on your `PATH`, it adds PostgreSQL checks for blocking
`CREATE INDEX`, a missing `NOT VALID`, `ADD COLUMN NOT NULL` without a safe default, `ALTER COLUMN
TYPE`, and `RENAME`.

## 4. Apply and verify against real PostgreSQL

```bash
de migrate up     --dsn postgres://…/mydb   # applies pending migrations
de migrate status --dsn postgres://…/mydb   # shows the applied version
de migrate verify --dsn postgres://…/mydb   # asserts the schema is at target and not dirty
```

Confirm `de migrate status` reports the new version and that the `down` reverses cleanly on a scratch
database.

## How it runs in production

Migrations run as a pre-deploy Job (a Helm pre-install/pre-upgrade hook), not from application boot. A
single advisory lock serializes concurrent replicas — one runs, the rest find the schema current. A
failed migration leaves a recoverable dirty state that the next corrected run auto-recovers, and the
persisted down-store lets a rollback run even after the image no longer ships the down file.

## See also

- [Persistence reference → Migrations](../../../reference/persistence/#migrations) — the engine, the
  framework baseline, and the safety guarantees.
- [Storage shapes](../storage-shapes/) — GORM vs ent, and the SQLite dev fast-path.
