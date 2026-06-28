package persistence

import (
	"context"
	"time"
)

// DefaultMaxOutboxAttempts is the F033 poison cutoff used by the in-process
// dispatcher: after this many failed attempts on the cursor's HEAD event the
// dispatcher dead-letters it (in the sidecar) and advances PAST it, so a permanently
// failing event causes only bounded head-of-line blocking. It is sidecar state — the
// outbox table itself has no attempts column (the outbox is WRITE-ONLY).
const DefaultMaxOutboxAttempts = 5

// OutboxRecord is one row of the transactional outbox: a durable, account-scoped
// record of a domain event. It is written in the SAME backend transaction as the
// aggregate change that produced it (via [OutboxStore.Append] through the ctx tx
// handle), so the event commits atomically with the aggregate write and is discarded
// on rollback — the transactional-outbox guarantee that prevents dual-write loss
// (update A, notify B, crash between).
//
// F033 WRITE-ONLY model: the outbox table is write-only. The ONLY writes to it are
// the producer's transactional Append (this row, atomic with the aggregate change)
// and the retention DDL that drops whole aged partitions ([OutboxRetention]). NOTHING
// ever UPDATEs or DELETEs an individual outbox row — there is no delivered_time,
// attempts, or leased_until column and no claim/lease/mark machinery. The in-process
// [events.Dispatcher] consumes the table as a FORWARD CURSOR (see [OutboxStore.ReadAfter]
// and [OutboxCursorStore]); a separate external consumer may tail it via the WAL/binlog
// ([OutboxCDCConsumer]). Both keep their own position OUTSIDE the outbox table.
//
// Payload is opaque bytes (a marshalled proto or JSON event body) so the seam stays
// backend- and codec-neutral. AccountID scopes the row to a tenant for isolation;
// AggregateType/AggregateID record which aggregate emitted it (events reference
// aggregates by ID only, F031).
type OutboxRecord struct {
	ID            string
	AccountID     string
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       []byte
	// CreatedTime is immutable and is the partition key (F033): RANGE partitions on
	// created_time make retention an O(1) DROP PARTITION rather than a per-row DELETE.
	// It is also the primary sort key of the forward cursor — events are read and
	// delivered in (created_time, id) order, which is commit/created order.
	CreatedTime time.Time

	// EventSeq is a per-tenant strictly-increasing, gap-free sequence number used by
	// cell-based development: a downstream consumer orders and dedups a tenant's
	// events by (AccountID, EventSeq) so a replay across a tenant move merges
	// deterministically. The producing store allocates it in the SAME transaction as
	// the business write when it is left 0 on Append (see the backend's outbox store),
	// so it is monotonic per tenant without a clock. It is backend-neutral here: a
	// store that does not allocate sequences leaves it 0.
	EventSeq int64
	// EventEpoch fences events to the route epoch at which they were produced, so a
	// consumer can discard an event from a superseded epoch after a tenant moves. The
	// producing store stamps it from the writer's admitted route epoch when it is left
	// 0 on Append. 0 means "unfenced" (never cell-routed), which is safe on existing
	// rows and for services that have not adopted cell-based development.
	EventEpoch int64
}

// OutboxCursor is a position in the forward scan of the WRITE-ONLY outbox: the
// (created_time, id) of the last event the consumer has processed. The next scan
// returns rows strictly greater than this in (created_time, id) lexicographic order.
// The zero value (CreatedTime zero, ID empty) is "before the first row" — a fresh
// consumer starts there and reads from the beginning. id breaks ties when two events
// share a created_time so the scan is total and never skips or repeats a row.
type OutboxCursor struct {
	CreatedTime time.Time
	ID          string
}

// IsZero reports whether c is the start-of-stream position (read from the beginning).
func (c OutboxCursor) IsZero() bool { return c.CreatedTime.IsZero() && c.ID == "" }

// OutboxStore is the pluggable persistence seam for the transactional outbox (F032
// G-2), refined by F033 into a WRITE-ONLY contract. It is intentionally backend-neutral
// — no broker, ORM, or driver import leaks across it, mirroring [lro.Store] and the
// dedup store — so the in-memory dev default and an ent/SQL-backed store satisfy the
// same contract and a message-broker adapter can be added outside the core later.
//
// The whole point of Append is that it MUST write the row through the transaction
// handle carried on ctx (see [TxFromContext]), so the outbox row and the aggregate
// change share one commit. A store that wrote on a separate connection would
// reintroduce the dual-write it exists to prevent.
//
// F033 WRITE-ONLY: the only methods are Append (the producer's transactional insert)
// and ReadAfter (a non-mutating forward scan the dispatcher consumes). There is NO
// claim, NO lease, NO delivered-mark, and NO per-row delete on this interface — the
// dispatcher NEVER mutates an outbox row; it advances its own cursor in a SIDECAR
// ([OutboxCursorStore]). Retention is a separate seam ([OutboxRetention]) that drops
// whole partitions, and an integrator who tails the WAL/binlog implements
// [OutboxCDCConsumer].
type OutboxStore interface {
	// Append durably records rec inside the ctx transaction. It is the operation a
	// [Publish] call makes from inside [TxRunner.Atomically]; the row becomes visible
	// only when that transaction commits (and vanishes on rollback). Implementations
	// resolve the tx-or-client from ctx exactly as a tx-aware repository does. This is
	// the ONLY write the producer makes to the outbox.
	Append(ctx context.Context, rec *OutboxRecord) error

	// ReadAfter returns up to limit events strictly after cursor in (created_time, id)
	// order — the forward scan the in-process dispatcher consumes. It does NOT mutate
	// any outbox row (no claim, no lease, no mark): the cursor lives in the SIDECAR
	// [OutboxCursorStore], not in the outbox table, so a delivered event is never
	// re-touched or re-written. A zero cursor starts from the beginning of the stream.
	//
	// Reading in (created_time, id) order delivers events in commit/created order
	// (per-aggregate ordering is therefore guaranteed; no global total order across
	// aggregates is promised beyond the created_time tiebreak). Implementations MUST
	// order deterministically by (created_time, id) so the cursor never skips or
	// repeats a row.
	ReadAfter(ctx context.Context, cursor OutboxCursor, limit int) ([]*OutboxRecord, error)
}

// OutboxCursorStore is the F033 SIDECAR state seam for the in-process dispatcher. The
// outbox table is WRITE-ONLY, so the dispatcher cannot record its progress there;
// instead it keeps a forward CURSOR (the (created_time, id) of the last delivered
// event) plus a small head-of-line poison counter and a dead-letter list in a
// separate sidecar table the dispatcher owns. Advancing the cursor is the ONLY way the
// dispatcher records delivery — it never writes the outbox.
//
// It is a SEPARATE interface from [OutboxStore] precisely so the outbox stays
// write-only: all dispatcher bookkeeping lives here. A single dispatcher instance per
// service owns one named cursor (the SDK assumes one in-process dispatcher per service;
// the external WAL/queue consumer is the scale-out path and tracks its own position).
//
// The sidecar must be writable in the dispatcher's own context (it is updated outside
// the handler transaction); a backend implementation persists it in its own table.
type OutboxCursorStore interface {
	// LoadCursor returns the saved forward position for the named dispatcher cursor and
	// its current head-of-line failure count (how many consecutive times the event now
	// AT the cursor head has failed delivery). A name that has never been saved returns
	// the zero cursor (start of stream) and zero failures, nil error.
	LoadCursor(ctx context.Context, name string) (cursor OutboxCursor, headFailures int, err error)

	// SaveCursor durably records the named cursor's forward position and head-of-line
	// failure count. The dispatcher calls it to ADVANCE the cursor after a successful
	// delivery (resetting headFailures to 0) or to bump headFailures after a failed
	// delivery of the head event (without advancing). It must be idempotent and safe to
	// re-save the same position.
	SaveCursor(ctx context.Context, name string, cursor OutboxCursor, headFailures int) error

	// DeadLetter records a poison event (one that failed delivery maxAttempts times at
	// the cursor head) in the sidecar, so it is auditable after the dispatcher advances
	// PAST it. It records the event id, type, and the position so an operator can find
	// and replay it. The outbox row itself is untouched (write-only); only the sidecar
	// records the poison verdict.
	DeadLetter(ctx context.Context, name string, rec *OutboxRecord, reason string) error
}

// OutboxRetention is the F033 retention seam: it drops aged outbox data WITHOUT
// per-row deletes. On a partitioned SQL backend it detaches/drops whole RANGE
// partitions whose created_time window is entirely older than t — an O(1) DDL
// operation, not a write+delete-heavy mass DELETE that bloats the table and stresses
// vacuum. SQLite and the in-memory dev store, which have no declarative partitioning,
// model the same contract as "forget rows older than t".
//
// It is a SEPARATE interface from [OutboxStore] so the write-only store stays free of
// any per-row delete path: only a retention task (which the service schedules — the
// SDK does not own a cron loop, see the store's RunRetention helper) ever drops data,
// and it does so a partition at a time.
//
// IMPORTANT (dispatcher safety): retention drops are storage reclamation for the
// in-process dispatcher's benefit and MUST NOT drop a partition the dispatcher has not
// yet consumed. A caller (the retention task) drops only partitions older than the
// retention window AND fully behind the dispatch cursor, so an in-process dispatcher
// never loses an undelivered event. (The external WAL consumer tracks its own WAL
// position; partition drops for storage reclamation are independent of it.)
type OutboxRetention interface {
	// DropPartitionsBefore drops every outbox partition (or, on a non-partitioned dev
	// backend, every row) whose data is strictly older than t, and returns how many
	// partitions (or rows) were dropped. Partitions that overlap t are kept so an
	// in-window row is never lost. It is O(number of partitions), not O(rows).
	DropPartitionsBefore(ctx context.Context, t time.Time) (dropped int, err error)
}

// OutboxCDCConsumer is the F033 CDC/WAL seam — an INTERFACE ONLY, with no engine
// shipped in the core, and the documented integration point for a SEPARATE,
// out-of-SDK project that tails the outbox and publishes to message queues
// (cross-server delivery is out of scope for this SDK). The built-in in-process
// dispatcher consumes the write-only outbox as a forward cursor for same-DB
// cross-aggregate reactions; an integrator who instead tails the database's change
// stream (PostgreSQL logical replication / WAL, MySQL binlog, or a Debezium connector)
// implements this interface against their replication client and feeds each new outbox
// row to handler.
//
// It is deliberately NOT implemented here: a full in-process logical-replication /
// binlog engine is heavy and would pull a CDC dependency into the clean core, which
// devedge-sdk forbids in persistence/authz/grpcauthz/events. Shipping the seam (and
// documenting it) honors the "consume via WAL if possible" intent without burdening the
// core — the write-only partitioned outbox is the substrate, the in-process dispatcher
// is the built-in same-DB reactor, and the WAL/CDC path is the pluggable cross-server
// alternative an integrator wires OUTSIDE this module.
//
// Contract: Consume tails the change stream of the outbox table and invokes handler
// once per committed row, in commit order, blocking until ctx is cancelled. handler
// returning an error signals the consumer to retry that row (at-least-once); the
// integrator's implementation is responsible for cursor/offset durability (the WAL
// slot / binlog position) so a restart resumes rather than replays from the beginning.
// Because the outbox is write-only, a CDC consumer never has to react to UPDATEs or
// DELETEs of existing rows — only INSERTs and partition drops.
//
// External-consumer learnings (captured from the F033 spike, for whoever builds it):
//   - A PARTITIONED outbox needs `CREATE PUBLICATION ... WITH (publish_via_partition_root = true)`
//     so logical replication publishes inserts as the parent table, not per-partition.
//   - A PostgreSQL logical-replication slot PINS WAL while the consumer is down — if the
//     consumer stalls, WAL accumulates and can fill the disk (a real outage hazard).
//     Monitor slot lag (pg_replication_slots.confirmed_flush_lsn vs current LSN).
//   - MySQL binlog FLIPS TO EVENT-LOSS once binlogs expire/purge (binlog_expire_logs_seconds):
//     a consumer down longer than the retention loses events — size retention to the
//     worst-case downtime and alert on it.
//   - The stream is AT-LEAST-ONCE; downstream consumers MUST be idempotent (dedup on the
//     event id), exactly as the in-process dispatcher relies on [events.IdempotencyStore].
type OutboxCDCConsumer interface {
	Consume(ctx context.Context, handler func(*OutboxRecord) error) error
}
