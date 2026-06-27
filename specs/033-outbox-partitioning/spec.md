# F033 — Write-only partitioned outbox + forward-cursor dispatcher + drop-partition retention (+ external WAL/CDC seam)

**Status**: design locked (revised — supersedes the poll+lease+delivered approach shipped in v0.22.0). Depends on F032 (outbox/events) + the SQL stores (gormtx, ent iam).
**Origin / user directive (verbatim intent)**: "use the outbox pattern in postgres and mysql using the WAL pattern if possible. don't do a SQL output pattern when you read stuff and delete it from the outbox. if you can't do the WAL (debezium like) pattern, use partitions so we don't end up with write+delete heavy patterns, we just drop partitions. actually that should be part of the outbox pattern."

## Problem
The v0.22.0 outbox was poll-and-mark/lease with per-row state churn (`MarkDelivered` wrote `delivered_time`; a claim bumped `attempts` + a lease; cleanup would be mass `DELETE`). On a high-throughput outbox that is write+delete-heavy (PG vacuum/bloat, MySQL churn) and it left a window of per-poll re-lease `UPDATE`s. The user wants the outbox **WRITE-ONLY**: the only writes are the producer's transactional `INSERT` and `DROP PARTITION` retention. Consumption must NOT mutate the table.

## Decision (revised, confirmed design)
- **The outbox table is WRITE-ONLY.** The only writes are the producer's transactional `INSERT` (atomic with the aggregate change) and `DROP PARTITION` retention DDL. **Removed `leased_until`, `attempts`, `delivered_time` columns** and all claim/lease/mark machinery. The dispatcher NEVER `UPDATE`s or `DELETE`s an outbox row.
- **Cross-server delivery is OUT OF SCOPE for the SDK.** A separate project tails the outbox (WAL/Debezium) and publishes to queues. The SDK does **not** build a WAL/CDC consumer; it keeps `OutboxCDCConsumer` as the documented seam for that external consumer.
- **Keep the in-process dispatcher for same-DB cross-aggregate reactions** (e.g. iam `UserSuspended → revoke the user's API keys`), reimplemented as a **FORWARD CURSOR**: the dispatcher keeps its OWN sidecar state table (`outbox_dispatch_cursor`), scans the outbox forward (`WHERE (created_time, id) > cursor ORDER BY created_time, id LIMIT n`), delivers each event to handlers, and advances the cursor in the SIDECAR — never touching outbox rows.
- **Exactly-once** stays guarded by the existing `events.IdempotencyStore` (handler-side; the marker commits in the handler tx). A crash between deliver and cursor-advance re-delivers → idempotency no-ops. At-least-once + idempotency = exactly-once effect (robust even if cursor concurrency is imperfect). The SDK assumes a **single dispatcher instance per service** for the cursor (documented); the external queue project is the scale-out path.
- **Poison handling** without an outbox `attempts` column: per-cursor head-of-line failure count tracked in the SIDECAR; after `maxAttempts` consecutive failures on the head event, record it to a sidecar **dead-letter** and advance the cursor PAST it (bounded head-of-line blocking).
- **Retention** = `DropPartitionsBefore` (drop whole old partitions, PG + MySQL, the dialect-aware DDL). CONSTRAINT: the in-process dispatcher must not lose events — drop only partitions older than the retention window **AND fully behind the dispatch cursor**. (The external WAL consumer tracks its own WAL position; partition-drop for storage reclamation is independent for it.)

## Locked defaults
Monthly RANGE partitions on `created_time`; ~30-day retention window; 5 max attempts (poison → dead-letter cutoff); retention = a **skeleton helper** the service wires into its own scheduler/cron (SDK doesn't own cron); CDC seam = interface + docs only (no engine). Validate on **Postgres AND MySQL** via testcontainers (+ sqlite/memory for functional).

## Design

### Interface (`persistence/outbox.go`)
- `OutboxStore`: **`Append`** (the producer's transactional insert) + **`ReadAfter(cursor, limit)`** (a non-mutating forward scan, `(created_time, id) > cursor`). **Dropped `ClaimUndelivered`, `MarkDelivered`, `Release`** and all lease machinery. The store never `UPDATE`s/`DELETE`s a row.
- New `OutboxCursor{CreatedTime, ID}` position type.
- New `OutboxCursorStore` sidecar seam: `LoadCursor(name) → (cursor, headFailures)`, `SaveCursor(name, cursor, headFailures)`, `DeadLetter(name, rec, reason)`. This is where the dispatcher records ALL progress (the outbox stays write-only).
- `OutboxRetention`: unchanged — `DropPartitionsBefore(t) → (dropped, err)`; whole-partition drops.
- `OutboxCDCConsumer`: unchanged interface, no impl — the documented seam for the **external** cross-server consumer (with spike learnings in the doc comment).
- `OutboxRecord`: `id, account_id, aggregate_type, aggregate_id, event_type, payload, created_time`. **Removed `DeliveredTime`, `Attempts`.** `created_time` is immutable, the partition key, and the forward-cursor sort key.

### Stores
- **gormtx.GormOutboxStore** + **ent iam EntOutboxStore**: `Append` writes through the ctx tx; `ReadAfter` is a keyset forward scan; no per-row mutation. Plus `DropPartitionsBefore` + partitioned-table DDL (PG declarative RANGE; MySQL RANGE on `TO_DAYS(created_time)`), with the bookkeeping columns removed from the parent DDL. **New** `GormOutboxCursorStore` / `EntOutboxCursorStore` sidecars (cursor table + dead-letter table).
- **MemoryOutboxStore**: write-only slice; `ReadAfter` sorts by `(created_time, id)`; `DropPartitionsBefore` forgets old records. **New** `MemoryOutboxCursorStore` sidecar.

### Dispatcher (`events/dispatcher.go`)
- `NewDispatcher(store, cursors, tx, idem, opts...)` — now takes the `OutboxCursorStore`.
- `RunOnce`: load cursor → `ReadAfter(cursor, limit)` → deliver each in order → advance cursor in the sidecar. The head event blocks (head-of-line) on failure; after `maxAttempts` it is dead-lettered and the cursor advances past it. No per-row outbox mutation anywhere.

### Schema (dialect-aware, write-only)
- PG: `... PRIMARY KEY (id, created_time) ... PARTITION BY RANGE (created_time)`; index on `created_time` for the keyset scan. No bookkeeping columns.
- MySQL: `PRIMARY KEY (id, created_time) ... PARTITION BY RANGE (TO_DAYS(created_time))`. No bookkeeping columns.
- SQLite/memory: no declarative partitioning (dev/test) — write-only + the helper models drop as a windowed delete-of-old (acceptable for the dev backend only).
- Sidecar `cursor_time` must match the outbox `created_time` precision (PG `timestamptz` / MySQL `datetime(6)`) so the keyset round-trips the consumed head event exactly.

## Acceptance criteria
- **AC-1 (write-only):** the dispatch path never `UPDATE`s or `DELETE`s an outbox row; delivery is tracked solely by the sidecar cursor + the idempotency marker. Verified by asserting `ReadAfter` is non-mutating and the row count only grows until a partition drop (memory + sqlite + PG + MySQL).
- **AC-2 (drop-partition retention):** `DropPartitionsBefore` removes old events via partition DDL (PG `DROP PARTITION`, MySQL `ALTER TABLE … DROP PARTITION`) — O(1), no per-row DELETE — proven on **PG and MySQL** (testcontainers); rows in the current window survive.
- **AC-3 (forward-cursor delivery + exactly-once):** `ReadAfter` + the cursor dispatcher deliver in commit/created order; exactly-once (idempotency) holds under re-delivery (cursor rewind) and a concurrent double-delivery; a poison head event is dead-lettered and the cursor advances past it after `maxAttempts`. On PG, MySQL, sqlite, memory.
- **AC-4 (CDC seam = external):** `OutboxCDCConsumer` interface exists + is documented as the external cross-server seam (with WAL/binlog spike learnings); no engine/heavy dep added; clean core intact (no broker/CDC/ORM/driver dep in persistence/authz/grpcauthz/events).
- **AC-5 (no regression):** existing outbox/events tests green; atomic-enlist (rollback discards) still holds.
- **AC-6 (docs):** `concepts/events.md` documents write-only + forward-cursor + partition-drop retention as the pattern, the retention-helper wiring (drop only behind the cursor), the single-dispatcher assumption, the poison dead-letter, and the external CDC seam + its learnings.

## Notes / risks
- Breaking change vs v0.22.0: `ClaimUndelivered`/`MarkDelivered`/`Release` removed; `OutboxRecord` loses `DeliveredTime`/`Attempts`; `NewDispatcher` gains a cursor-store arg. Pre-1.0, clean rewrite acceptable per the repo's guiding principle (no real users yet).
- MySQL partitioning requires the partition column in every unique key (PK = `(id, created_time)`); confirmed against the generated/hand DDL.
- AutoMigrate doesn't create partitioned tables — tests/fixtures create the partitioned table via explicit DDL, then exercise append/read/drop.
- Sidecar precision: the cursor `cursor_time` must store at the same precision as the outbox `created_time` (ent: `SchemaType datetime(6)`/`timestamptz`; gorm MySQL deployments must migrate `cursor_time` as `datetime(6)`), else a consumed head event re-reads as "after" the cursor (a re-delivery loop). The idempotency marker still makes that harmless, but it wastes work.
- Clean core: partitioning/CDC DDL + drivers stay in the stores/testdata; the `persistence` interfaces stay ORM/driver-neutral.
