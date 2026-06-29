package events

import (
	"encoding/json"
	"fmt"
)

// ChangeEventType is the well-known [Event.Type] every domain-change event is
// published under. A consumer (e.g. an audit service) subscribes ONCE to this
// type and receives every change across every resource; the ResourceType inside
// the [ChangeEvent] envelope distinguishes them. This keeps the change feed a
// single topic on the existing relay rather than a topic per resource, which is
// what the horizontal ility consumers (audit/search/export) want: one feed, not
// one subscription per service.
const ChangeEventType = "devedge.change.v1"

// ChangeType is the kind of domain mutation a [ChangeEvent] records.
type ChangeType string

const (
	ChangeCreate   ChangeType = "CREATE"
	ChangeUpdate   ChangeType = "UPDATE"
	ChangeDelete   ChangeType = "DELETE"
	ChangeUndelete ChangeType = "UNDELETE"
)

// Actor is the minimal authenticated-caller identity recorded on a change — the
// "who". It is a deliberately small projection of the authz principal (subject +
// tenant + groups) so a durable change record does NOT carry the full claim set
// (tokens/PII) into the outbox. It is populated from the identity the authz stage
// stashes on the request context (see middleware.PrincipalFromContext); it is
// empty on an unauthenticated/public path.
type Actor struct {
	Subject string   `json:"subject,omitempty"`
	Tenant  string   `json:"tenant,omitempty"`
	Groups  []string `json:"groups,omitempty"`
}

// ChangeEvent is the typed envelope over an outbox row (P1): a uniform,
// automatic description of ONE domain mutation, emitted in the same transaction
// as the write that caused it (see [ChangeEmitting]). It is the keystone of the
// horizontal ility seams — audit consumes it sanitized, search consumes it
// projected, export IS it. The payload of the underlying [Event] stays opaque
// []byte (this header is JSON-encoded into it); the routing fields of the Event
// (Type/AccountID/AggregateType/AggregateID) are set so the existing relay topic
// routing and the consumer's per-event tenant re-scoping work unchanged.
//
// TENANT CORRECTNESS (a security property, not a nicety): Tenant is the account
// the change belongs to. The emitter stamps it from the request tenant on
// context — the SAME source the generated repository scopes the write by — and
// the consumer re-injects it as the handler's tenant before reacting
// (events/consumer.go). The outbox/relay itself is a trusted, multi-tenant
// internal plane (ReadAfter is not tenant-filtered by design); correctness rests
// on every event carrying the right Tenant, which [ChangeEmitting] guarantees.
type ChangeEvent struct {
	Tenant       string          `json:"tenant"`
	ResourceType string          `json:"resource_type"`
	ResourceName string          `json:"resource_name,omitempty"`
	Change       ChangeType      `json:"change"`
	FieldMask    []string        `json:"field_mask,omitempty"`
	Actor        Actor           `json:"actor,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	Revision     string          `json:"revision,omitempty"`
	// Before is the resource representation before the change (UPDATE/DELETE),
	// off by default for storage/PII cost; opt in per resource. Valid JSON when
	// present (protojson of the redacted message under the default marshaller).
	Before json.RawMessage `json:"before,omitempty"`
	// After is the resource representation after the change (CREATE/UPDATE/
	// UNDELETE), redacted of secret fields. Valid JSON when present.
	After json.RawMessage `json:"after,omitempty"`
	// Seq and Epoch are the per-tenant ordering/fencing metadata of the outbox
	// row. They are NOT part of the encoded payload (the store allocates them
	// inside the producing transaction, so they are unknown at emit time); they
	// are restored from the consumed [Event] by [ChangeEventFromEvent].
	Seq   int64 `json:"-"`
	Epoch int64 `json:"-"`
}

// ToEvent encodes ce as an outbox [Event]: the typed header is JSON-marshalled
// into the opaque Payload, and the Event routing fields are set so the relay and
// consumer treat it like any other domain event. AccountID is set to Tenant —
// this is the field the consumer re-scopes each handler by, so it MUST be the
// change's tenant. Seq/Epoch are deliberately not encoded (the store allocates
// them in-tx).
func (ce ChangeEvent) ToEvent() (Event, error) {
	payload, err := json.Marshal(ce)
	if err != nil {
		return Event{}, fmt.Errorf("events: encode change event: %w", err)
	}
	return Event{
		Type:          ChangeEventType,
		AccountID:     ce.Tenant,
		AggregateType: ce.ResourceType,
		AggregateID:   ce.ResourceName,
		Payload:       payload,
	}, nil
}

// ChangeEventFromEvent decodes the [ChangeEvent] carried by evt, restoring the
// authoritative tenant and the per-tenant ordering/fencing metadata from the
// outbox row (carried on the Event) rather than trusting the encoded payload.
// Consumers (audit/search/export) call it inside their handler.
func ChangeEventFromEvent(evt Event) (ChangeEvent, error) {
	var ce ChangeEvent
	if err := json.Unmarshal(evt.Payload, &ce); err != nil {
		return ChangeEvent{}, fmt.Errorf("events: decode change event %q: %w", evt.ID, err)
	}
	// The Event's AccountID/Seq/Epoch are the authoritative values from the
	// outbox row (the relay copies them off the row); prefer them over the
	// encoded payload, which carried 0 for Seq/Epoch at emit time.
	if evt.AccountID != "" {
		ce.Tenant = evt.AccountID
	}
	ce.Seq = evt.EventSeq
	ce.Epoch = evt.EventEpoch
	return ce, nil
}

// IsChangeEvent reports whether evt is a change-feed event (so a consumer that
// also handles bespoke domain events can dispatch on it).
func IsChangeEvent(evt Event) bool { return evt.Type == ChangeEventType }
