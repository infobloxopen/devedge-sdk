package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Outbox is the F032/F033 WRITE-ONLY transactional-outbox table for the IAM fixture.
// It is NOT a proto-derived resource (it is internal infrastructure, never exposed via
// the API), so it is a HAND-WRITTEN ent schema rather than a protoc-gen-ent output.
//
// The point of modelling it as an ent entity is that the IAM OutboxStore can write a
// row through the ctx *ent.Tx (resolved via persistence.TxFromContext) so the row
// commits in the SAME transaction as the aggregate change that emitted it (F032 AC-1).
//
// F033 WRITE-ONLY: the table is written once (Append) and read forward (ReadAfter). The
// fields are exactly the durable event: account-scoped for tenant isolation, the
// emitting aggregate by type+id, the event type + opaque payload, and the immutable
// created_time (the forward-cursor sort key and partition key). There are NO
// dispatcher-bookkeeping columns (no delivered_time, attempts, or leased_until) — the
// in-process dispatcher keeps its position in a SIDECAR (OutboxCursor /
// OutboxDeadLetter), never in this table, so an outbox row is never mutated after it is
// appended.
//
// Deliberately NO TenantMixin: that mixin auto-scopes every query to the ctx tenant via
// an interceptor, but the background dispatcher reads rows ACROSS all tenants.
// account_id is therefore a plain field used for isolation in the event payload, not a
// query-scoping discriminator.
type Outbox struct {
	ent.Schema
}

// Fields defines the fields of Outbox (the durable event only — no bookkeeping).
func (Outbox) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("id").Immutable(),
		field.String("account_id").Optional(),
		field.String("aggregate_type").Optional(),
		field.String("aggregate_id").Optional(),
		field.String("event_type").Optional(),
		field.Bytes("payload").Optional(),
		field.Time("created_time").Optional(),
	}
}

// Indexes defines the indexes of Outbox: the dispatcher reads forward in
// (created_time, id) order, so created_time is indexed for the keyset scan.
func (Outbox) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("created_time"),
	}
}
