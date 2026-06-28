// Package events is the F032 transactional-outbox + domain-events seam: a
// backend-neutral way to react to a change in one aggregate by changing another,
// WITHOUT making the two changes one transaction (which F031 forbids across
// aggregate boundaries) and WITHOUT a dual write that loses the second step on a
// crash.
//
// The mechanism is the transactional outbox. A handler that changes aggregate A
// calls [Publisher.Publish] from inside the same [persistence.TxRunner.Atomically]
// it used for the A write; Publish appends a durable event row to the outbox
// THROUGH that transaction (via [persistence.OutboxStore]), so the event commits
// atomically with the A change and is discarded if the A change rolls back. A
// [Dispatcher] then delivers the committed event at-least-once to registered
// handlers, each of which changes aggregate B in its OWN Atomically — so the
// cross-aggregate reaction is itself a safe single-aggregate write, reached by
// eventual consistency.
//
// Clean core: this package depends only on [persistence] (the OutboxStore seam and
// the F030 tx helpers). No message broker, ORM, or driver is imported — a broker
// adapter can implement the same OutboxStore/Dispatcher seam outside the core.
package events

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// Event is a domain-event notification (F032 D-5): a small message that references
// the aggregate it concerns BY ID ONLY (consistent with F031 `references`), not by
// carrying the aggregate itself. Handlers re-load whatever they need from the IDs.
//
// Payload is opaque bytes — a marshalled proto or JSON body — so the seam stays
// codec-neutral. ID is the idempotency key (F032 D-4): a duplicate delivery of the
// same ID must be a no-op in a handler. Leave ID empty and Publish assigns a fresh
// UUID; leave AccountID empty and Publish fills it from the tenant on ctx.
type Event struct {
	ID            string
	Type          string
	AggregateType string
	AggregateID   string
	AccountID     string
	Payload       []byte

	// EventSeq and EventEpoch are the cell-based-development ordering/fencing
	// metadata carried from the outbox row onto the published event so a consumer
	// can order and dedup a tenant's events by (AccountID, EventSeq) and discard a
	// superseded-epoch event after a tenant move. They are allocated/stamped by the
	// outbox store inside the producing transaction (left 0 here on Publish); 0
	// means "unsequenced/unfenced", which is the case for services that have not
	// adopted cell-based development.
	EventSeq   int64
	EventEpoch int64
}

// Publisher records a domain event. The contract that makes this a transactional
// outbox rather than a dual write: Publish MUST be called inside
// [persistence.TxRunner.Atomically], and it appends the event through the ctx
// transaction so the event and the aggregate change share one commit (F032 G-1).
type Publisher interface {
	// Publish records evt durably in the current transaction. It returns
	// [persistence.ErrNoTransaction] when ctx is not enrolled in an Atomically
	// transaction (F032 D-1: the safe choice — refuse rather than write outside the
	// aggregate's commit and risk a dual-write loss).
	Publish(ctx context.Context, evt Event) error
}

// OutboxPublisher is the [Publisher] backed by a [persistence.OutboxStore]. It is
// the only Publish implementation the core ships; it is backend-neutral because the
// OutboxStore it writes to is (in-memory for tests, ent/SQL for the IAM example).
type OutboxPublisher struct {
	store persistence.OutboxStore
	now   func() time.Time
}

// NewOutboxPublisher returns a Publisher that appends events to store.
func NewOutboxPublisher(store persistence.OutboxStore) *OutboxPublisher {
	return &OutboxPublisher{store: store, now: time.Now}
}

// Publish implements [Publisher]. It enforces the F030 RequireTx guard FIRST (D-1):
// a Publish outside Atomically returns [persistence.ErrNoTransaction] before any
// store call, so a caller who forgot to wrap the work in a transaction fails loudly
// instead of writing an outbox row that is not atomic with the aggregate change.
// Inside a transaction it fills defaults (a fresh id, the tenant account, the
// created time) and appends the row through the ctx tx via the store.
func (p *OutboxPublisher) Publish(ctx context.Context, evt Event) error {
	// D-1: refuse outside a transaction. This is the dual-write guard at the publish
	// seam; the store's Append enforces the same as a backstop.
	if err := persistence.RequireTx(ctx); err != nil {
		return err
	}
	if evt.ID == "" {
		evt.ID = uuid.NewString()
	}
	if evt.AccountID == "" {
		evt.AccountID = middleware.TenantIDFromContext(ctx)
	}
	rec := &persistence.OutboxRecord{
		ID:            evt.ID,
		AccountID:     evt.AccountID,
		AggregateType: evt.AggregateType,
		AggregateID:   evt.AggregateID,
		EventType:     evt.Type,
		Payload:       evt.Payload,
		CreatedTime:   p.now(),
	}
	return p.store.Append(ctx, rec)
}

// compile-time check.
var _ Publisher = (*OutboxPublisher)(nil)
