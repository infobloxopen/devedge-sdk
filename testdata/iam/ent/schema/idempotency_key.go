package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// IdempotencyKey is the WS-043 / F048 durable, exactly-once request-idempotency table for
// the IAM (ent) fixture — the ent twin of gormtx.IdempotencyKeyRow. Like Outbox/IdemMarker
// it is internal exactly-once infrastructure (never a proto-derived API resource), so it is
// a HAND-WRITTEN ent schema, and the entrepo.EntDurableDedupStore closures (iamv1/
// dedup_store.go) read/write it through the ctx *ent.Tx.
//
// ent 0.14 has no composite natural primary key, so the primary key is a single `id` that
// ENCODES the full key (account_id\x00method\x00request_id — see
// entrepo.EncodeIdempotencyID). id uniqueness ≡ (account_id, method, request_id) uniqueness,
// so exactly-once is preserved; account_id / method / request_id are kept as real columns
// for querying and for WS-029 row-level security (account_id).
type IdempotencyKey struct {
	ent.Schema
}

// Annotations pins the physical table name (rather than ent's pluralization) so it matches
// the durable-dedup table name the rest of the SDK uses.
func (IdempotencyKey) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "idempotency_keys"}}
}

// Fields defines the columns. The response bytes + proto type name are populated only on
// Complete; fingerprint is optional (param-fingerprint guard).
func (IdempotencyKey) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("id").Immutable(),
		field.String("account_id").Default(""),
		field.String("method").Default(""),
		field.String("request_id").Default(""),
		field.String("status").Default(""),
		field.String("response_type").Optional().Default(""),
		field.Bytes("response").Optional(),
		field.String("fingerprint").Optional().Default(""),
		field.Time("created_at"),
		field.Time("expires_at"),
	}
}

// Indexes adds the expires_at index the GC sweep scans.
func (IdempotencyKey) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("expires_at"),
	}
}
