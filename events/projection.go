package events

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/middleware/redact"
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// Projection is a named, derived shape of an entity that auto-feeds the change
// feed — the P5 emittable-surface seam. A search index wants a flattened,
// denormalized document (carrying account_name, compartment_name so it can sort
// and display without a runtime join); a reporting pipeline wants a different row;
// an audit record wants the entity itself. Rather than hand-write a publish in
// every handler for each, a service declares its projections once and every CUD
// fans out: the entity's own [ChangeEvent] (Wave 1 audit) PLUS one event per
// projection, each its own ResourceType, each landing on the same transactional
// outbox in the same transaction as the write. "P5 is literally surface ⨯ P1."
//
// The SDK supplies the mechanism (atomic, tenant-correct emission); the service
// supplies the one thing only it knows — the shape of its search/reporting
// document, in Project. The projected document is whatever the consumer needs; it
// is not constrained to the entity's fields. A generated projection (from a
// surface message marked (storage.v1.emit_on_change)) is the planned ergonomic
// follow-up and would populate exactly this struct.
type Projection[T any] struct {
	// ResourceType is the type string stamped on this projection's ChangeEvent
	// (e.g. "search.user"). It MUST differ from the entity feed's ResourceType and
	// from sibling projections so each consumer can route on it. Required.
	ResourceType string

	// Project derives the projected document from the entity. Returning emit=false
	// skips this projection for this entity (e.g. a draft that is not yet
	// indexable) — on a delete that also suppresses the index-removal event, which
	// is correct because the entity was never projected.
	Project func(T) (doc any, emit bool)

	// NameOf derives the projected document's resource name (the stable id the
	// consumer keys its index/row on). Optional; falls back to the feed's NameOf.
	NameOf func(T) string

	// RevisionOf derives an etag/version for the projection's ChangeEvent. Optional.
	RevisionOf func(T) string

	// Marshal renders the projected document as JSON. When nil: a proto.Message doc
	// is secret-redacted (redact.Message) and emitted as protojson — so a
	// projection of a secret-bearing entity never persists the secret; any other
	// doc type is encoding/json marshalled, and the projection author owns
	// redaction (exactly as ChangeFeedOptions.Marshal does for the entity image).
	Marshal func(doc any) (json.RawMessage, error)
}

// emitProjections fans a single mutation out to every configured projection,
// publishing each on the same outbox/transaction the entity event used. Called
// after the entity's own event so the entity feed and its projections share
// ordering within the change stream.
func (r *changeEmitter[T, K]) emitProjections(ctx context.Context, ct ChangeType, tenant string, entity T) error {
	for i := range r.opts.Projections {
		ce, ok, err := buildProjectionEvent(ctx, r.opts.Projections[i], ct, tenant, r.opts.NameOf, entity)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := r.publish(ctx, ce); err != nil {
			return err
		}
	}
	return nil
}

// buildProjectionEvent renders one projection of entity into a ChangeEvent, or
// reports ok=false when the projection elects not to emit. fallbackName supplies
// the resource name when the projection has no NameOf of its own. A delete carries
// only the resource identity (no after-image) so the consumer drops the document;
// every other change type carries the projected document as the after-image.
func buildProjectionEvent[T any](
	ctx context.Context,
	p Projection[T],
	ct ChangeType,
	tenant string,
	fallbackName func(T) string,
	entity T,
) (ChangeEvent, bool, error) {
	doc, emit := p.Project(entity)
	if !emit {
		return ChangeEvent{}, false, nil
	}
	ce := ChangeEvent{
		Tenant:       tenant,
		ResourceType: p.ResourceType,
		Change:       ct,
		Actor:        actorFromContext(ctx),
		RequestID:    middleware.RequestIDFromContext(ctx),
	}
	switch {
	case p.NameOf != nil:
		ce.ResourceName = p.NameOf(entity)
	case fallbackName != nil:
		ce.ResourceName = fallbackName(entity)
	}
	if p.RevisionOf != nil {
		ce.Revision = p.RevisionOf(entity)
	}
	if ct != ChangeDelete {
		img, err := marshalDoc(doc, p.Marshal)
		if err != nil {
			return ChangeEvent{}, false, fmt.Errorf("events: marshal projection %q: %w", p.ResourceType, err)
		}
		ce.After = img
	}
	return ce, true, nil
}

// marshalDoc renders a projected document as JSON: the projection's own
// marshaller if set, else secret-redacted protojson for a proto.Message, else
// encoding/json for any other shape.
func marshalDoc(doc any, custom func(any) (json.RawMessage, error)) (json.RawMessage, error) {
	if custom != nil {
		return custom(doc)
	}
	if m, ok := doc.(proto.Message); ok {
		b, err := protojson.Marshal(redact.Message(m))
		if err != nil {
			return nil, err
		}
		return json.RawMessage(b), nil
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// ProjectExisting is the P11 bootstrap dev-default: it re-emits every LIVE entity
// as a projection CREATE, so onboarding a type, adding a denormalized field, or
// rebuilding an index can backfill the consumer from the current state of the
// repository — the reactive change feed keeps the index current going forward,
// this seeds it. It pages through repo.List within the caller's tenant (taken from
// ctx, and required — a backfill emitting under an empty tenant is the same leak
// the live feed guards against) and publishes each page's projection events inside
// one tx.Atomically, because the transactional outbox only accepts an Append that
// is enrolled in a transaction (a publish outside one is the dual-write the outbox
// exists to prevent).
//
// An enterprise transport can replace this with a versioned snapshot + epoch
// recovery behind the same seam, activated by presence; the List-based replay is
// deliberately light for local dev. Returns the number of projection events
// emitted.
func ProjectExisting[T any, K comparable](
	ctx context.Context,
	repo persistence.Repository[T, K],
	tx persistence.TxRunner,
	pub Publisher,
	proj Projection[T],
	fallbackName func(T) string,
	pageSize int,
) (int, error) {
	tenant := middleware.TenantIDFromContext(ctx)
	if tenant == "" {
		return 0, fmt.Errorf("events: ProjectExisting for %q requires a tenant on context", proj.ResourceType)
	}
	if pageSize <= 0 {
		pageSize = 200
	}
	emitted := 0
	token := ""
	for {
		items, next, err := repo.List(ctx, persistence.ListOptions{PageSize: pageSize, PageToken: token})
		if err != nil {
			return emitted, fmt.Errorf("events: ProjectExisting list %q: %w", proj.ResourceType, err)
		}
		err = tx.Atomically(ctx, func(ctx context.Context) error {
			for i := range items {
				ce, ok, err := buildProjectionEvent(ctx, proj, ChangeCreate, tenant, fallbackName, items[i])
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				evt, err := ce.ToEvent()
				if err != nil {
					return err
				}
				if err := pub.Publish(ctx, evt); err != nil {
					return fmt.Errorf("events: ProjectExisting publish %q: %w", proj.ResourceType, err)
				}
				emitted++
			}
			return nil
		})
		if err != nil {
			return emitted, err
		}
		if next == "" {
			break
		}
		token = next
	}
	return emitted, nil
}
