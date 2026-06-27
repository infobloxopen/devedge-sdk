package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// OutboxDeadLetter is the F033 SIDECAR dead-letter table for the in-process
// dispatcher: a poison event (one that failed delivery maxAttempts times at the cursor
// head) is parked HERE before the dispatcher advances PAST it, so it stays auditable
// and replayable without ever mutating the WRITE-ONLY outbox. Internal infrastructure,
// never exposed via the API — a hand-written ent schema.
//
// The auto-increment int id is ent's default; cursor_name groups parked events by the
// dispatcher cursor that dead-lettered them, event_id/event_type/created_time identify
// the outbox event, reason captures the last delivery error, and recorded_at stamps when
// it was parked.
type OutboxDeadLetter struct {
	ent.Schema
}

// Fields defines the fields of OutboxDeadLetter.
func (OutboxDeadLetter) Fields() []ent.Field {
	return []ent.Field{
		field.String("cursor_name").Optional(),
		field.String("event_id").Optional(),
		field.String("event_type").Optional(),
		field.String("reason").Optional(),
		field.Time("created_time").Optional(),
		field.Time("recorded_at").Optional(),
	}
}

// Indexes defines the indexes of OutboxDeadLetter.
func (OutboxDeadLetter) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("cursor_name"),
	}
}
