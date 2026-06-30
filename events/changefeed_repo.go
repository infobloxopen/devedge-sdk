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

// ChangeFeedOptions configures a [ChangeEmitting] decorator for resource T.
type ChangeFeedOptions[T any] struct {
	// ResourceType is the resource-type string recorded on every emitted
	// [ChangeEvent] (e.g. "iam.account"). Required — a feed with no resource type
	// is not consumable.
	ResourceType string

	// NameOf derives the AIP-122 resource name (or a stable id) of an entity for
	// ChangeEvent.ResourceName. Optional; when nil the resource name is left empty
	// (the feed is still consumable by type, just not by individual resource).
	NameOf func(T) string

	// RevisionOf derives an etag/version string for ChangeEvent.Revision.
	// Optional.
	RevisionOf func(T) string

	// Marshal renders an entity as the after/before image, as valid JSON. When
	// nil, the default marshaller is used: it requires T to be a proto.Message,
	// redacts (infoblox.field.v1.opts).secret fields via redact.Message, and
	// emits protojson — so the durable change record never carries a secret. A
	// non-proto T MUST supply this option.
	Marshal func(T) (json.RawMessage, error)

	// AllowMissingTenant opts OUT of the fail-closed tenant guard. By default
	// (false) a mutation whose context carries no tenant is REJECTED before the
	// write commits, so a change can never be recorded against an empty/wrong
	// tenant — a leaky feed is a security incident. Set true only for genuinely
	// tenantless paths (system bootstrap, global resources).
	AllowMissingTenant bool

	// EmitBefore includes the before-image on UPDATE/DELETE (an extra read +
	// storage + PII cost). Off by default; opt in per resource when a consumer
	// needs the prior state (e.g. a diff-style audit record).
	EmitBefore bool

	// Projections are named, derived shapes (search documents, reporting rows)
	// emitted ALONGSIDE the entity's own change event on every mutation — the P5
	// emittable-surfaces seam: one CUD feed fans out to N consumer-shaped
	// projections, each its own ChangeEvent type, each routed to its consumer.
	// Empty (the default) is pure P1 (Wave 1 audit) — unchanged behaviour. See
	// [Projection].
	Projections []Projection[T]
}

// ChangeEmitting wraps a [persistence.Repository] so that every Create/Update/
// Delete/Undelete also appends a typed [ChangeEvent] to the outbox IN THE SAME
// TRANSACTION as the write — the P1 automatic domain-change feed. It is the
// keystone seam the horizontal ilities (audit/search/export) consume: a resource
// opts in (its generated registrar installs this decorator) and from then on
// every mutation is observable uniformly, with no per-service emit code.
//
// Atomicity. The decorator opens tx.Atomically itself and runs the inner write +
// the emit inside it, because the generated default CRUD handler does NOT wrap
// its repository calls in a transaction. Nested Atomically calls join the outer
// transaction (see persistence.TxRunner), so this composes correctly when a
// caller (e.g. an aggregate Save or a custom handler) already opened one: the
// commit still happens once, at the outermost boundary, and a rollback discards
// the change event with the write (no orphan outbox row — the transactional
// outbox guarantee).
//
// Tenant correctness. The emitter stamps ChangeEvent.Tenant from the request
// tenant on context (middleware.TenantIDFromContext) — the same source the
// generated repository scopes the write by — and fails closed when that tenant
// is absent unless AllowMissingTenant is set. See [ChangeEvent] for why this is a
// security property.
//
// tx is the transaction runner that makes the inner write and the outbox Append
// atomic (for the in-memory backend it must enrol both the repository's store
// and the outbox store; for a SQL backend a single TxRunner over the connection
// covers both). pub is the [Publisher] the emit goes through (typically an
// [OutboxPublisher] over the same outbox store).
func ChangeEmitting[T any, K comparable](
	inner persistence.Repository[T, K],
	tx persistence.TxRunner,
	pub Publisher,
	opts ChangeFeedOptions[T],
) persistence.Repository[T, K] {
	return &changeEmitter[T, K]{inner: inner, tx: tx, pub: pub, opts: opts}
}

type changeEmitter[T any, K comparable] struct {
	inner persistence.Repository[T, K]
	tx    persistence.TxRunner
	pub   Publisher
	opts  ChangeFeedOptions[T]
}

// Get and List are pure reads — passed through unchanged, no event.
func (r *changeEmitter[T, K]) Get(ctx context.Context, key K) (T, error) {
	return r.inner.Get(ctx, key)
}

func (r *changeEmitter[T, K]) List(ctx context.Context, opts persistence.ListOptions) ([]T, string, error) {
	return r.inner.List(ctx, opts)
}

func (r *changeEmitter[T, K]) Create(ctx context.Context, entity T) (T, error) {
	var out T
	err := r.tx.Atomically(ctx, func(ctx context.Context) error {
		created, err := r.inner.Create(ctx, entity)
		if err != nil {
			return err
		}
		out = created
		return r.emit(ctx, ChangeCreate, created, nil, nil)
	})
	return out, err
}

func (r *changeEmitter[T, K]) Update(ctx context.Context, key K, entity T, fieldMask ...string) (T, error) {
	var out T
	err := r.tx.Atomically(ctx, func(ctx context.Context) error {
		var before *T
		if r.opts.EmitBefore {
			if b, err := r.inner.Get(ctx, key); err == nil {
				before = &b
			}
		}
		updated, err := r.inner.Update(ctx, key, entity, fieldMask...)
		if err != nil {
			return err
		}
		out = updated
		return r.emit(ctx, ChangeUpdate, updated, before, fieldMask)
	})
	return out, err
}

func (r *changeEmitter[T, K]) Delete(ctx context.Context, key K) error {
	return r.tx.Atomically(ctx, func(ctx context.Context) error {
		// Capture the entity before deletion when we need its identity (for the
		// resource name) or its before-image. Audit of a delete is meaningless
		// without knowing WHICH resource went away, so this read is justified.
		var snapshot *T
		if r.opts.NameOf != nil || r.opts.EmitBefore {
			if s, err := r.inner.Get(ctx, key); err == nil {
				snapshot = &s
			}
		}
		if err := r.inner.Delete(ctx, key); err != nil {
			return err
		}
		return r.emitDelete(ctx, key, snapshot)
	})
}

func (r *changeEmitter[T, K]) Undelete(ctx context.Context, key K) (T, error) {
	var out T
	err := r.tx.Atomically(ctx, func(ctx context.Context) error {
		restored, err := r.inner.Undelete(ctx, key)
		if err != nil {
			return err
		}
		out = restored
		return r.emit(ctx, ChangeUndelete, restored, nil, nil)
	})
	return out, err
}

// emit builds and publishes the change event for a mutation that yields an
// after-image (create/update/undelete). before is optional (update only).
func (r *changeEmitter[T, K]) emit(ctx context.Context, ct ChangeType, after T, before *T, fieldMask []string) error {
	tenant, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	ce := ChangeEvent{
		Tenant:       tenant,
		ResourceType: r.opts.ResourceType,
		Change:       ct,
		FieldMask:    fieldMask,
		Actor:        actorFromContext(ctx),
		RequestID:    middleware.RequestIDFromContext(ctx),
	}
	if r.opts.NameOf != nil {
		ce.ResourceName = r.opts.NameOf(after)
	}
	if r.opts.RevisionOf != nil {
		ce.Revision = r.opts.RevisionOf(after)
	}
	img, err := r.marshal(after)
	if err != nil {
		return err
	}
	ce.After = img
	if before != nil && r.opts.EmitBefore {
		if bimg, err := r.marshal(*before); err == nil {
			ce.Before = bimg
		}
	}
	if err := r.publish(ctx, ce); err != nil {
		return err
	}
	return r.emitProjections(ctx, ct, tenant, after)
}

// emitDelete builds and publishes the change event for a deletion. The resource
// has no after-image; snapshot (the pre-delete entity, when captured) supplies
// the resource name and the optional before-image.
func (r *changeEmitter[T, K]) emitDelete(ctx context.Context, key K, snapshot *T) error {
	tenant, err := r.tenant(ctx)
	if err != nil {
		return err
	}
	ce := ChangeEvent{
		Tenant:       tenant,
		ResourceType: r.opts.ResourceType,
		Change:       ChangeDelete,
		Actor:        actorFromContext(ctx),
		RequestID:    middleware.RequestIDFromContext(ctx),
	}
	switch {
	case snapshot != nil && r.opts.NameOf != nil:
		ce.ResourceName = r.opts.NameOf(*snapshot)
	default:
		ce.ResourceName = fmt.Sprintf("%v", key)
	}
	if snapshot != nil && r.opts.EmitBefore {
		if bimg, err := r.marshal(*snapshot); err == nil {
			ce.Before = bimg
		}
	}
	if err := r.publish(ctx, ce); err != nil {
		return err
	}
	// A projection can only be removed from its consumer (search index, read
	// model) if we know its identity, which comes from the pre-delete snapshot.
	if snapshot != nil {
		return r.emitProjections(ctx, ChangeDelete, tenant, *snapshot)
	}
	return nil
}

func (r *changeEmitter[T, K]) publish(ctx context.Context, ce ChangeEvent) error {
	evt, err := ce.ToEvent()
	if err != nil {
		return err
	}
	return r.pub.Publish(ctx, evt)
}

// tenant resolves the change's tenant from context and enforces the fail-closed
// guard. A tenantless mutation is rejected (rolling back the write) unless the
// resource opted into AllowMissingTenant.
func (r *changeEmitter[T, K]) tenant(ctx context.Context) (string, error) {
	t := middleware.TenantIDFromContext(ctx)
	if t == "" && !r.opts.AllowMissingTenant {
		return "", fmt.Errorf("events: change feed for %q requires a tenant on context "+
			"(no account-id resolved); set ChangeFeedOptions.AllowMissingTenant to permit tenantless changes",
			r.opts.ResourceType)
	}
	return t, nil
}

// marshal renders an entity as the JSON after/before image, using the configured
// marshaller or the secret-redacting protojson default.
func (r *changeEmitter[T, K]) marshal(e T) (json.RawMessage, error) {
	if r.opts.Marshal != nil {
		return r.opts.Marshal(e)
	}
	m, ok := any(e).(proto.Message)
	if !ok {
		return nil, fmt.Errorf("events: change feed for %q needs ChangeFeedOptions.Marshal for non-proto entity type %T",
			r.opts.ResourceType, e)
	}
	// Redact secret-annotated fields BEFORE serialising: the image lands in the
	// durable outbox, so a secret left in it is a persisted leak even if a
	// downstream consumer would later redact it.
	b, err := protojson.Marshal(redact.Message(m))
	if err != nil {
		return nil, fmt.Errorf("events: marshal after-image for %q: %w", r.opts.ResourceType, err)
	}
	return json.RawMessage(b), nil
}

// actorFromContext projects the authz principal stashed on ctx (by the authz
// interceptor) into the minimal [Actor] recorded on a change. Empty on an
// unauthenticated/public path.
func actorFromContext(ctx context.Context) Actor {
	p, ok := middleware.PrincipalFromContext(ctx)
	if !ok {
		return Actor{}
	}
	return Actor{Subject: p.Subject, Tenant: p.Tenant, Groups: p.Groups}
}

var _ persistence.Repository[any, string] = (*changeEmitter[any, string])(nil)
