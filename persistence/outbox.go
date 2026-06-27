package persistence

import (
	"context"
	"time"
)

// OutboxRecord is one row of the transactional outbox: a durable, account-scoped
// record of a domain event awaiting delivery. It is written in the SAME backend
// transaction as the aggregate change that produced it (via [OutboxStore.Append]
// through the ctx tx handle), so the event commits atomically with the aggregate
// write and is discarded on rollback — the transactional-outbox guarantee that
// prevents dual-write loss (update A, notify B, crash between).
//
// The fields are the F032 D-2 schema. Payload is opaque bytes (a marshalled proto
// or JSON event body) so the seam stays backend- and codec-neutral. AccountID
// scopes the row to a tenant for isolation; AggregateType/AggregateID record which
// aggregate emitted it (events reference aggregates by ID only, F031). Attempts and
// DeliveredTime are dispatcher bookkeeping for at-least-once delivery.
type OutboxRecord struct {
	ID            string
	AccountID     string
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       []byte
	CreatedTime   time.Time
	// DeliveredTime is nil until a dispatcher has successfully delivered the event
	// to every handler. A non-nil value marks the row delivered (a terminal state).
	DeliveredTime *time.Time
	// Attempts counts delivery tries; the dispatcher increments it on each claim so
	// a poison event can be detected against a dead-letter threshold.
	Attempts int
}

// OutboxStore is the pluggable persistence seam for the transactional outbox
// (F032 G-2). It is intentionally backend-neutral — no broker, ORM, or driver
// import leaks across it, mirroring [lro.Store] and the dedup store — so the
// in-memory dev default and an ent/SQL-backed store satisfy the same contract and
// a message-broker adapter can be added outside the core later.
//
// The whole point is Append: it MUST write the row through the transaction handle
// carried on ctx (see [TxFromContext]), so the outbox row and the aggregate change
// share one commit. A store that wrote on a separate connection would reintroduce
// the dual-write it exists to prevent.
type OutboxStore interface {
	// Append durably records rec inside the ctx transaction. It is the operation a
	// [Publish] call makes from inside [TxRunner.Atomically]; the row becomes visible
	// only when that transaction commits (and vanishes on rollback). Implementations
	// resolve the tx-or-client from ctx exactly as a tx-aware repository does.
	Append(ctx context.Context, rec *OutboxRecord) error

	// ClaimUndelivered atomically leases up to limit undelivered rows to the calling
	// dispatcher: it marks each claimed row (incrementing Attempts and recording the
	// claim) and returns them so the dispatcher can deliver them to handlers. The
	// claim is what makes the default poller safe without SELECT ... FOR UPDATE SKIP
	// LOCKED (which ent's sql/lock is not enabled for here, F032 D-3): a leased row is
	// hidden from a concurrent claim until its lease lapses.
	ClaimUndelivered(ctx context.Context, limit int) ([]*OutboxRecord, error)

	// MarkDelivered records that the event identified by id was delivered to all
	// handlers (stamping DeliveredTime), a terminal state that future claims skip.
	MarkDelivered(ctx context.Context, id string) error

	// Release drops the lease on a claimed-but-undelivered row so it can be re-claimed
	// immediately rather than waiting out the lease. The dispatcher calls it when a
	// handler errors, so an at-least-once retry is prompt instead of lease-delayed. A
	// crashed dispatcher that never calls Release simply re-delivers after the lease
	// lapses (the lease is the safety net; Release is the fast path).
	Release(ctx context.Context, id string) error
}
