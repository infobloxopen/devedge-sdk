package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
)

// OutboxCursor is the F033 SIDECAR cursor table for the in-process dispatcher. The
// outbox is WRITE-ONLY, so the dispatcher cannot record its progress there; it keeps
// its forward position (created_time, id) and head-of-line failure count HERE instead,
// one row per named cursor. It is internal infrastructure, never exposed via the API,
// so it is a HAND-WRITTEN ent schema.
//
// The string id is the cursor name (the dispatcher's logical cursor; a service runs a
// single in-process dispatcher, so one row is typical). cursor_time + cursor_id are the
// last consumed event's (created_time, id); head_failures counts consecutive failed
// deliveries of the event now at the cursor head (the poison counter that lives off the
// write-only outbox).
type OutboxCursor struct {
	ent.Schema
}

// Fields defines the fields of OutboxCursor.
func (OutboxCursor) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("name").Immutable(), // the cursor name
		// cursor_time must store at MICROSECOND precision so it round-trips exactly
		// against the outbox created_time (datetime(6) on MySQL / timestamptz on PG). A
		// lower-precision column would truncate the saved cursor and re-read the just-
		// consumed head event as "after" the cursor (an infinite re-delivery loop).
		field.Time("cursor_time").Optional().SchemaType(map[string]string{
			dialect.MySQL:    "datetime(6)",
			dialect.Postgres: "timestamptz",
		}),
		field.String("cursor_id").Optional(),
		field.Int("head_failures").Default(0),
	}
}
