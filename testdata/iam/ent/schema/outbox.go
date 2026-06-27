package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Outbox is the F032 transactional-outbox table for the IAM fixture. It is NOT a
// proto-derived resource (it is internal infrastructure, never exposed via the
// API), so it is a HAND-WRITTEN ent schema rather than a protoc-gen-ent output.
//
// The point of modelling it as an ent entity is that the IAM OutboxStore can write
// a row through the ctx *ent.Tx (resolved via persistence.TxFromContext) so the row
// commits in the SAME transaction as the aggregate change that emitted it (F032 D-2
// / AC-1). The fields are the D-2 schema: account-scoped for tenant isolation, the
// emitting aggregate by type+id, the event type + opaque payload, and dispatcher
// bookkeeping (created/delivered times, attempts, plus a lease for the claimed-flag
// claim strategy of D-3 — ent sql/lock is not enabled here).
//
// Deliberately NO TenantMixin: that mixin auto-scopes every query to the ctx tenant
// via an interceptor, but the background dispatcher must claim undelivered rows
// ACROSS all tenants. account_id is therefore a plain field used for isolation in
// the event payload, not a query-scoping discriminator.
type Outbox struct {
	ent.Schema
}

// Fields defines the fields of Outbox.
func (Outbox) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("id").Immutable(),
		field.String("account_id").Optional(),
		field.String("aggregate_type").Optional(),
		field.String("aggregate_id").Optional(),
		field.String("event_type").Optional(),
		field.Bytes("payload").Optional(),
		field.Time("created_time").Optional(),
		field.Time("delivered_time").Optional().Nillable(),
		field.Int("attempts").Default(0),
		// leased_until backs the claimed-flag/lease claim strategy (D-3): a claimed
		// row is hidden from a competing claim until this time passes.
		field.Time("leased_until").Optional().Nillable(),
	}
}

// Indexes defines the indexes of Outbox: the dispatcher scans for undelivered rows.
func (Outbox) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("delivered_time"),
	}
}
