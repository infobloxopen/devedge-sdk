// Package reference is the cross-service resource-reference seam (F041, WS-021
// P1). It carries the machine-readable metadata protoc-gen-svc emits for a
// field annotated with the standard google.api.resource_reference (AIP-124) and
// the resolver seam a composition layer uses to batch-fetch referenced targets
// without an N+1 waterfall.
//
// This package is deliberately stdlib-only so a server-only consumer's module
// graph stays dependency-light (the repo's check-graph-isolation gate). It
// defines mechanism, not policy: it does NOT dial services, own a catalog, or
// know transport — a static/in-process [StaticResolver] serves tests and the
// fixture; the catalog-backed resolver lands with WP-A/P2.
//
// The invariant this package upholds is the anti-N+1 guarantee: given N parent
// resources each naming a target by a foreign key, [Load] resolves them in
// exactly ONE BatchGet call per distinct target-id set (DataLoader-style). The
// referenced field stays a scalar foreign key — this package never introduces a
// traversable Go edge or a cascade (metadata only; parity with the deliberately
// edge-less infoblox.ddd.v1.references).
package reference

import (
	"context"
	"fmt"
)

// Cardinality is whether a reference field names a single target (a scalar FK)
// or many (a repeated FK).
type Cardinality string

const (
	// One is a single-target reference (a scalar foreign-key field).
	One Cardinality = "one"
	// Many is a multi-target reference (a repeated foreign-key field).
	Many Cardinality = "many"
)

// Reference is the generated metadata for one cross-service reference field: the
// source field on a resource message points at a target resource type served by
// (possibly) another microservice. protoc-gen-svc emits a <Svc>References table
// of these from google.api.resource_reference annotations.
//
// It is metadata ONLY — a composition layer reads it to resolve references; it
// is not a Go edge and carries no mutation authority. The target module/endpoint
// is resolved elsewhere (the apx catalog index, WP-A) from TargetType, which is
// a globally unique AIP-122 resource type.
type Reference struct {
	// FieldName is the Go field name on the source resource holding the target's
	// foreign key (e.g. "RegionId").
	FieldName string
	// FKField is the proto field name of that foreign key (e.g. "region_id").
	FKField string
	// TargetType is the referenced resource's AIP-122 type (e.g.
	// "region.example.com/Region"). Globally unique; the target module/endpoint is
	// catalog-resolved from it.
	TargetType string
	// Cardinality is One for a scalar FK, Many for a repeated FK.
	Cardinality Cardinality
}

// BatchGetter is the batch-fetch capability a reference target must expose so a
// reference to it can be resolved without an N+1 waterfall (AIP-137 BatchGet).
// A generated reference-target service exposes exactly this over its BatchGet
// client. T is the target resource's Go type (e.g. *regionv1.Region).
type BatchGetter[T any] interface {
	// BatchGet retrieves the targets named by ids, in id order. Implementations
	// front a single backend BatchGet call (they are NOT expected to loop).
	BatchGet(ctx context.Context, ids []string) ([]T, error)
}

// ReferenceResolver maps a target resource type to a BatchGet-capable client for
// it. It is the seam between the generated reference metadata and the concrete
// clients: a composition layer looks up the resolver for a Reference.TargetType
// and batch-fetches through it.
//
// The returned client is untyped (any) because a resolver serves many target
// types with different Go types; a caller type-asserts it to a BatchGetter[T]
// for the target it is resolving (see [Load]). ok is false when no client is
// registered for targetType — which a fail-loud composition treats as an error
// (never a silent per-row fetch).
type ReferenceResolver interface {
	ResolverFor(targetType string) (client any, ok bool)
}

// StaticResolver is an in-process ReferenceResolver backed by a map from target
// type to client. It serves tests and the P1 two-service fixture; the
// catalog-backed resolver (dialing services from the apx type→endpoint index)
// lands with WP-A/P2. The zero value is not usable — construct with
// [NewStaticResolver].
type StaticResolver struct {
	clients map[string]any
}

// NewStaticResolver returns an empty in-process resolver. Register clients with
// [StaticResolver.Register].
func NewStaticResolver() *StaticResolver {
	return &StaticResolver{clients: map[string]any{}}
}

// Register wires a BatchGet-capable client for targetType. A later Register for
// the same type replaces the earlier one.
func (r *StaticResolver) Register(targetType string, client any) {
	r.clients[targetType] = client
}

// ResolverFor implements ReferenceResolver.
func (r *StaticResolver) ResolverFor(targetType string) (any, bool) {
	c, ok := r.clients[targetType]
	return c, ok
}

// Load resolves a reference for N parents in exactly ONE BatchGet call
// (DataLoader-style): it collects the distinct, non-empty target ids named by
// the parents (via ids), batch-fetches them once through the resolver's client
// for ref.TargetType, and returns the targets keyed by id.
//
// It is the anti-N+1 primitive F041 AC-5 asserts: for N parents naming M
// distinct targets it issues one BatchGet of M ids, never M or N single Gets. A
// missing resolver for the target type is a fail-loud error (never a silent
// per-row fetch). BatchGet returns targets in id order, so the returned map is
// built by zipping the deduped id set with the result slice.
//
// T is the target resource's Go type; the resolver's registered client for
// ref.TargetType must be a BatchGetter[T]. keyOf extracts a target's id so the
// result can be keyed (BatchGet returns id order, but keyOf keeps the mapping
// robust to an implementation that returns a different length/order).
func Load[T any, P any](
	ctx context.Context,
	resolver ReferenceResolver,
	ref Reference,
	parents []P,
	ids func(P) []string,
	keyOf func(T) string,
) (map[string]T, error) {
	// Collect distinct, non-empty target ids across all parents — the dedup is
	// what turns N parents into ONE BatchGet of the distinct set.
	seen := map[string]struct{}{}
	var distinct []string
	for _, p := range parents {
		for _, id := range ids(p) {
			if id == "" {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			distinct = append(distinct, id)
		}
	}
	out := make(map[string]T, len(distinct))
	if len(distinct) == 0 {
		return out, nil
	}

	client, ok := resolver.ResolverFor(ref.TargetType)
	if !ok {
		return nil, fmt.Errorf("reference: no resolver registered for target type %q (field %q → %s); cannot batch-resolve without a BatchGet-capable client",
			ref.TargetType, ref.FieldName, ref.FKField)
	}
	getter, ok := client.(BatchGetter[T])
	if !ok {
		return nil, fmt.Errorf("reference: resolver for target type %q is not a BatchGetter[%T]", ref.TargetType, *new(T))
	}

	// The single BatchGet — one round trip for the whole collection.
	targets, err := getter.BatchGet(ctx, distinct)
	if err != nil {
		return nil, fmt.Errorf("reference: batch get %q: %w", ref.TargetType, err)
	}
	for _, t := range targets {
		out[keyOf(t)] = t
	}
	return out, nil
}
