# F033 — Append-only partitioned outbox + drop-partition retention (+ WAL/CDC seam)

**Status**: DRAFT — design locked. Depends on F032 (outbox/events) + the SQL stores (gormtx, ent iam).
**Origin / user directive (verbatim intent)**: "use the outbox pattern in postgres and mysql using the WAL pattern if possible. don't do a SQL output pattern when you read stuff and delete it from the outbox. if you can't do the WAL (debezium like) pattern, use partitions so we don't end up with write+delete heavy patterns, we just drop partitions. actually that should be part of the outbox pattern."

## Problem
The current outbox is poll-and-mark/lease with per-row state churn (`MarkDelivered` writes `delivered_time`; cleanup would be mass `DELETE`). On a high-throughput outbox that is write+delete-heavy (PG vacuum/bloat, MySQL churn). The user wants: consume via WAL/CDC if feasible; otherwise an **append-only, partitioned** outbox where retention is **`DROP PARTITION`** (O(1) DDL), never per-row deletes — and this should be intrinsic to the pattern.

## Decision (from design)
- **WAL/CDC is NOT a built-in default** — a full in-process Debezium-like engine (PG logical replication / MySQL binlog) is heavy and tensions with clean-core. Ship a **`OutboxCDCConsumer` interface seam** (no engine) so an integrator can plug logical-replication/Debezium; document it. This honors "WAL if possible" without burdening core.
- **Built-in default = append-only partitioned outbox + drop-partition retention.** No per-row DELETE; delivery truth is the **idempotency marker** (already SQL-backed on ent+gorm), not a `delivered_time` row write.

## Locked defaults
Monthly RANGE partitions on `created_time`; 30-day retention window; 5 max attempts (poison cutoff); retention = a **skeleton helper** the service wires into its own scheduler/cron (SDK doesn't own cron); CDC seam = interface + docs only (no engine). Validate on **Postgres AND MySQL** via testcontainers (+ sqlite/memory for functional).

## Design

### Interface (`persistence/outbox.go`)
- `OutboxStore`: keep `Append`; `ClaimUndelivered(ctx, maxAttempts, limit)` (claim by `attempts < maxAttempts` + lease, NOT `delivered_time IS NULL`); keep `Release`; **`MarkDelivered` becomes a no-op / cursor-bump** (delivery truth = idempotency marker). Outbox rows are append-only; never DELETEd by the store.
- New `OutboxRetention` seam: `DropPartitionsBefore(ctx, t time.Time) (dropped int, err error)` — drops whole partitions older than `t` (no row deletes). Plus a `PartitionDDL`/ensure-partition helper. SQLite/memory model this as "forget old rows."
- New `OutboxCDCConsumer` seam (optional, no impl): `Consume(ctx, handler func(*OutboxRecord) error) error` — for integrators who tail the WAL/binlog instead of polling. SDK ships the interface + docs only.
- `OutboxRecord`: `created_time` is immutable + the partition key; `Attempts` is the dispatch counter (terminal at maxAttempts). `DeliveredTime` deprecated (idempotency marker is truth).

### Stores
- **gormtx.GormOutboxStore** + **ent iam outbox store**: `Append` unchanged (writes through ctx tx); `ClaimUndelivered` filters on attempts+lease; `MarkDelivered`→no-op; add `DropPartitionsBefore` + partitioned-table DDL (PG declarative RANGE; MySQL RANGE) as dialect-aware DDL. Tables become append-only.
- **MemoryOutboxStore**: append-only ring; `DropPartitionsBefore` forgets old records.

### Dispatcher (`events/dispatcher.go`)
- Stop relying on `MarkDelivered` row-write as delivery truth; the idempotency marker (in the handler tx) is the source of truth. Keep lease/attempts for retry; honor `maxAttempts` (poison cutoff). No per-row DELETE anywhere.

### Schema (dialect-aware)
- PG: `... PRIMARY KEY (id, created_time) ... PARTITION BY RANGE (created_time)`; partial index on `(leased_until, attempts) WHERE attempts < max`.
- MySQL: `PRIMARY KEY (id, created_time) ... PARTITION BY RANGE (...)` on a created_time-derived value.
- SQLite: no declarative partitioning (dev/test) — append-only + the helper models drop as a windowed delete-of-old (acceptable for the dev backend only).

## Acceptance criteria
- **AC-1 (append-only):** the dispatch path never DELETEs or row-marks outbox rows; delivery is tracked solely by the idempotency marker. Verified by asserting row count only grows until a partition drop.
- **AC-2 (drop-partition retention):** `DropPartitionsBefore` removes old delivered events via partition DDL (PG `DETACH/DROP PARTITION`, MySQL `ALTER TABLE ... DROP PARTITION`) — O(1), no per-row DELETE — proven on **PG and MySQL** (testcontainers); rows in the current window survive.
- **AC-3 (delivery still works):** `ClaimUndelivered(maxAttempts, limit)` + dispatch + exactly-once (idempotency) still hold on PG, MySQL, sqlite, memory; a poison event stops after maxAttempts.
- **AC-4 (CDC seam):** `OutboxCDCConsumer` interface exists + is documented; no engine/heavy dep added; clean core intact (no broker/CDC dep in persistence/authz/grpcauthz/events).
- **AC-5 (no regression):** existing outbox/events tests green; atomic-enlist (rollback discards) still holds.
- **AC-6 (docs):** `concepts/events.md` documents append-only + partition-drop retention as the pattern, the retention-helper wiring, and the CDC seam.

## Phasing
1. **P1**: interface rework (`ClaimUndelivered(maxAttempts)`, MarkDelivered→no-op, `OutboxRetention` + `OutboxCDCConsumer` seams) + dispatcher change + memory store + gorm/ent stores append-only + PG partitioning DDL + retention helper. Validate PG + sqlite + memory (testcontainers PG exists). 
2. **P2**: MySQL — testcontainers mysql:8, MySQL RANGE partitioning DDL + drop-partition, prove AC-2/AC-3 on MySQL (honors "postgres and mysql"). CI adds MySQL.
3. Docs (AC-6).

## Notes / risks
- Breaking change: `MarkDelivered` no longer the delivery truth; mitigate with a clear migration note (no real users pre-1.0 — clean rewrite acceptable per repo guiding principle).
- MySQL partitioning requires the partition column in every unique key (PK = `(id, created_time)`); confirm against the generated schema.
- AutoMigrate doesn't create partitioned tables — tests/fixtures create the partitioned table via explicit DDL, then exercise append/claim/drop.
- Clean core: partitioning/CDC DDL + drivers stay in the stores/testdata; the `persistence` interfaces stay ORM/driver-neutral.
