package persistence

import (
	"context"
	"time"
)

// DefaultMaxOutboxAttempts is the locked F033 poison cutoff: after this many
// dispatch attempts a row is no longer re-claimed (it is parked as poison). The
// dispatcher and stores use it when a caller does not specify one.
const DefaultMaxOutboxAttempts = 5

// OutboxRecord is one row of the transactional outbox: a durable, account-scoped
// record of a domain event awaiting delivery. It is written in the SAME backend
// transaction as the aggregate change that produced it (via [OutboxStore.Append]
// through the ctx tx handle), so the event commits atomically with the aggregate
// write and is discarded on rollback — the transactional-outbox guarantee that
// prevents dual-write loss (update A, notify B, crash between).
//
// The fields are the F032 D-2 schema, refined by F033 into an APPEND-ONLY model.
// Payload is opaque bytes (a marshalled proto or JSON event body) so the seam stays
// backend- and codec-neutral. AccountID scopes the row to a tenant for isolation;
// AggregateType/AggregateID record which aggregate emitted it (events reference
// aggregates by ID only, F031).
//
// F033 append-only invariant: the store NEVER DELETEs or delivered-marks a row on
// the dispatch path. CreatedTime is immutable and is the RANGE partition key;
// retention is whole-partition drops ([OutboxRetention]), never per-row deletes.
// Delivery truth is the idempotency marker recorded in the handler's own tx (see
// events.IdempotencyStore), NOT a row write — so the outbox table is a pure
// append-only log.
type OutboxRecord struct {
	ID            string
	AccountID     string
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       []byte
	// CreatedTime is immutable and is the partition key (F033): RANGE partitions on
	// created_time make retention an O(1) DROP PARTITION rather than a per-row DELETE.
	CreatedTime time.Time
	// Deprecated: F033 made the outbox append-only — delivery truth is the
	// idempotency marker recorded in the handler's transaction, not a row write.
	// DeliveredTime is retained only for backward field-compatibility and is no
	// longer written or read on the dispatch path; do not depend on it.
	DeliveredTime *time.Time
	// Attempts is the dispatch counter: each claim increments it. A row is terminal
	// (poison, no longer re-claimed) once Attempts reaches the dispatcher's
	// maxAttempts. This is what ClaimUndelivered filters on (NOT delivered_time).
	Attempts int
}

// OutboxStore is the pluggable persistence seam for the transactional outbox
// (F032 G-2), refined by F033 into an append-only contract. It is intentionally
// backend-neutral — no broker, ORM, or driver import leaks across it, mirroring
// [lro.Store] and the dedup store — so the in-memory dev default and an ent/SQL-
// backed store satisfy the same contract and a message-broker adapter can be added
// outside the core later.
//
// The whole point is Append: it MUST write the row through the transaction handle
// carried on ctx (see [TxFromContext]), so the outbox row and the aggregate change
// share one commit. A store that wrote on a separate connection would reintroduce
// the dual-write it exists to prevent.
//
// F033: the table is APPEND-ONLY. No method on this interface DELETEs a row, and
// MarkDelivered no longer writes delivery state (it is a no-op) — delivery truth is
// the idempotency marker, not a row write. Retention is a separate seam
// ([OutboxRetention]) that drops whole partitions, and an integrator who tails the
// WAL/binlog instead of polling implements [OutboxCDCConsumer].
type OutboxStore interface {
	// Append durably records rec inside the ctx transaction. It is the operation a
	// [Publish] call makes from inside [TxRunner.Atomically]; the row becomes visible
	// only when that transaction commits (and vanishes on rollback). Implementations
	// resolve the tx-or-client from ctx exactly as a tx-aware repository does.
	Append(ctx context.Context, rec *OutboxRecord) error

	// ClaimUndelivered atomically leases up to limit rows that are still eligible for
	// dispatch — those with Attempts < maxAttempts whose lease has lapsed — to the
	// calling dispatcher: it increments Attempts and stamps a fresh lease, then returns
	// them so the dispatcher can deliver them to handlers.
	//
	// F033: eligibility is attempts-based, NOT delivered_time-based. The append-only
	// table never marks a row delivered; instead a delivered event simply stops being
	// re-delivered because its handlers' idempotency markers make redelivery a no-op,
	// and a poison event stops being re-claimed once Attempts reaches maxAttempts (the
	// poison cutoff). A non-positive maxAttempts uses [DefaultMaxOutboxAttempts].
	//
	// The lease is what makes the default poller safe without SELECT ... FOR UPDATE
	// SKIP LOCKED (which ent's sql/lock is not enabled for here, F032 D-3): a leased
	// row is hidden from a concurrent claim until its lease lapses.
	ClaimUndelivered(ctx context.Context, maxAttempts, limit int) ([]*OutboxRecord, error)

	// MarkDelivered is retained for source compatibility but is a NO-OP under the F033
	// append-only model: delivery truth is the idempotency marker recorded in the
	// handler's own transaction, not a row write. The dispatcher no longer calls it on
	// the critical path; an implementation MUST NOT DELETE or mutate delivery state
	// here. It returns nil.
	MarkDelivered(ctx context.Context, id string) error

	// Release drops the lease on a claimed row so it can be re-claimed immediately
	// rather than waiting out the lease. The dispatcher calls it when a handler errors,
	// so an at-least-once retry is prompt instead of lease-delayed. A crashed
	// dispatcher that never calls Release simply re-delivers after the lease lapses
	// (the lease is the safety net; Release is the fast path). It does NOT delete the
	// row (append-only).
	Release(ctx context.Context, id string) error
}

// OutboxRetention is the F033 retention seam: it drops aged outbox data WITHOUT
// per-row deletes. On a partitioned SQL backend it detaches/drops whole RANGE
// partitions whose created_time window is entirely older than t — an O(1) DDL
// operation, not a write+delete-heavy mass DELETE that bloats the table and stresses
// vacuum. SQLite and the in-memory dev store, which have no declarative
// partitioning, model the same contract as "forget rows older than t".
//
// It is a SEPARATE interface from [OutboxStore] so the append-only store stays free
// of any delete path on the hot dispatch loop: only a retention task (which the
// service schedules — the SDK does not own a cron loop, see the store's RunRetention
// helper) ever drops data, and it does so a partition at a time.
type OutboxRetention interface {
	// DropPartitionsBefore drops every outbox partition (or, on a non-partitioned dev
	// backend, every row) whose data is strictly older than t, and returns how many
	// partitions (or rows) were dropped. Partitions that overlap t are kept so an
	// in-window row is never lost. It is O(number of partitions), not O(rows).
	DropPartitionsBefore(ctx context.Context, t time.Time) (dropped int, err error)
}

// OutboxCDCConsumer is the F033 CDC/WAL seam — an INTERFACE ONLY, with no engine
// shipped in the core. The built-in dispatcher polls the append-only outbox; an
// integrator who would rather tail the database's change stream (PostgreSQL logical
// replication / WAL, MySQL binlog, or a Debezium connector) implements this
// interface against their replication client and feeds each new outbox row to
// handler.
//
// It is deliberately NOT implemented here: a full in-process logical-replication /
// binlog engine is heavy and would pull a CDC dependency into the clean core, which
// devedge-sdk forbids in persistence/authz/grpcauthz/events. Shipping the seam (and
// documenting it) honors the "consume via WAL if possible" intent without burdening
// the core — the partitioned append-only outbox is the built-in default, the WAL/CDC
// path is a pluggable alternative an integrator wires outside this module.
//
// Contract: Consume tails the change stream of the outbox table and invokes handler
// once per committed row, in commit order, blocking until ctx is cancelled. handler
// returning an error signals the consumer to retry that row (at-least-once); the
// integrator's implementation is responsible for cursor/offset durability so a
// restart resumes rather than replays from the beginning. Because the outbox is
// append-only, a CDC consumer never has to react to UPDATEs or DELETEs of existing
// rows — only INSERTs and partition drops.
type OutboxCDCConsumer interface {
	Consume(ctx context.Context, handler func(*OutboxRecord) error) error
}
