package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// IdemMarker is the F032 idempotency-marker table for the IAM fixture — the ent
// twin of persistence/gormtx.IdemMarker. Like Outbox it is NOT a proto-derived
// resource (it is internal exactly-once infrastructure, never exposed via the
// API), so it is a HAND-WRITTEN ent schema rather than a protoc-gen-ent output.
//
// The point of modelling it as an ent entity is that the IAM EntIdempotencyStore
// can write a marker row through the ctx *ent.Tx (resolved via
// persistence.TxFromContext) so the marker commits in the SAME transaction as the
// handler's aggregate write. The idempotency key is the PRIMARY KEY (a string id
// with storage column "key", exactly as Outbox uses a string id): a duplicate
// Record is therefore a primary-key conflict — the in-tx uniqueness that
// serializes a concurrent double-apply and lets the loser's whole tx (effect +
// marker) roll back, closing the orphan-marker window the in-memory store leaves
// on the ent path (F032 AC-2).
//
// Deliberately NO TenantMixin and NO account_id: the dispatcher claims and applies
// across all tenants, and the (event id, handler name) key is already globally
// unique, so a global dedup marker keyed by that string is sufficient. A tenant
// discriminator would only risk a mixin interceptor re-scoping the marker query
// away from the background dispatcher's (empty) tenant.
type IdemMarker struct {
	ent.Schema
}

// Fields defines the fields of IdemMarker: the idempotency key as the string
// primary key, stored in the "key" column.
func (IdemMarker) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("key").Immutable(),
	}
}
