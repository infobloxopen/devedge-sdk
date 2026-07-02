package federationgql

import (
	"context"
	"fmt"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/infobloxopen/devedge-sdk/reference"
)

// buildObject constructs (once) the GraphQL object type for a resource, with its
// scalar fields plus one object-typed edge field per declared reference. Fields
// are supplied through a thunk so an edge's target type can be built in any
// order (GraphQL forward reference).
func (b *builder) buildObject(r *Resource) *graphql.Object {
	if obj, ok := b.gqlByType[r.Type]; ok {
		return obj
	}
	res := r // capture for the thunk
	b.fieldThunks[r.Type] = func() graphql.Fields {
		fields := graphql.Fields{}
		for i := range res.Scalars {
			sf := res.Scalars[i]
			t := sf.Type
			if t == nil {
				t = graphql.String
			}
			resolve := sf.Resolve
			fields[sf.Name] = &graphql.Field{
				Type: t,
				Resolve: func(p graphql.ResolveParams) (any, error) {
					if resolve == nil {
						return nil, nil
					}
					return resolve(p.Source), nil
				},
			}
		}
		for _, ref := range res.References {
			if f := b.edgeField(res, ref); f != nil {
				name := b.edgeFieldName(res, ref)
				fields[name] = f
			}
		}
		return fields
	}
	obj := graphql.NewObject(graphql.ObjectConfig{
		Name:   r.Name,
		Fields: b.fieldThunks[r.Type],
	})
	b.gqlByType[r.Type] = obj
	return obj
}

// edgeFieldName is the GraphQL field name for a reference edge: the descriptor's
// override if it yields a non-empty name, else the target Resource's Name
// lowercased.
func (b *builder) edgeFieldName(r *Resource, ref reference.Reference) string {
	if r.EdgeFieldName != nil {
		if n := r.EdgeFieldName(ref); n != "" {
			return n
		}
	}
	if target, ok := b.byType[ref.TargetType]; ok {
		return strings.ToLower(target.Name[:1]) + target.Name[1:]
	}
	// Fall back to the FK field's Go name lowercased (should be unreachable — a
	// missing target is rejected in edgeField).
	return strings.ToLower(ref.FieldName)
}

// edgeField builds the object-typed edge field for one reference. The resolver
// reads the target from the request-scoped cache the root resolver preloaded via
// reference.Load (D-3, eager per-collection preload) — it performs no fetch of
// its own, which is what makes the whole list→edge query cost exactly one
// BatchGet per referenced collection.
func (b *builder) edgeField(source *Resource, ref reference.Reference) *graphql.Field {
	target, ok := b.byType[ref.TargetType]
	if !ok {
		// NewSchema validates this before assembling the query; keep the edge out
		// of the schema rather than panic.
		return nil
	}
	targetObj := b.gqlByType[ref.TargetType]
	if targetObj == nil {
		targetObj = b.buildObject(target)
	}
	refIDs := source.RefIDs
	fieldType := graphql.Type(targetObj)
	if ref.Cardinality == reference.Many {
		fieldType = graphql.NewList(targetObj)
	}
	return &graphql.Field{
		Type: fieldType,
		Resolve: func(p graphql.ResolveParams) (any, error) {
			cache := cacheFrom(p.Context)
			if cache == nil {
				return nil, fmt.Errorf("federationgql: no request cache in context (edge %s.%s)", source.Name, ref.FieldName)
			}
			if refIDs == nil {
				return nil, fmt.Errorf("federationgql: resource %q has no RefIDs (edge %s)", source.Type, ref.FieldName)
			}
			// Surface any preload error for this collection as a per-field GraphQL
			// error (fail-loud: a missing resolver / downstream denial does not
			// become a silent null).
			if err := cache.errFor(ref.TargetType); err != nil {
				return nil, err
			}
			ids := refIDs(ref, p.Source)
			if ref.Cardinality == reference.Many {
				out := make([]any, 0, len(ids))
				for _, id := range ids {
					if t, ok := cache.get(ref.TargetType, id); ok {
						out = append(out, t)
					}
				}
				return out, nil
			}
			if len(ids) == 0 || ids[0] == "" {
				return nil, nil
			}
			if t, ok := cache.get(ref.TargetType, ids[0]); ok {
				return t, nil
			}
			return nil, nil
		},
	}
}

// preloadReferences batch-fetches, for each declared reference of source over
// the just-fetched parents, the referenced targets NOT already in the
// request-scoped cache, stashing them (and any error) for the edge resolvers to
// read. This is the eager per-collection preload (D-3): a collection of N
// parents naming M distinct targets costs exactly ONE BatchGet.
//
// The dedup is keyed by (target type, id) via the cache's fetched set — not a
// bare "target type already loaded" flag — so it is correct AND still one
// BatchGet per collection when the SAME target type is reached from more than
// one reference (two edges on one source) or from more than one source
// collection in the same query: each preload BatchGets only the ids still
// missing, never silently dropping a second reference's targets. read_mask
// (D-5) is derived per reference from its edge sub-selection.
func (b *builder) preloadReferences(p graphql.ResolveParams, source *Resource, parents []any) {
	cache := cacheFrom(p.Context)
	if cache == nil {
		return
	}
	refIDs := source.RefIDs
	for _, ref := range source.References {
		target, ok := b.byType[ref.TargetType]
		if !ok {
			cache.setErr(ref.TargetType, fmt.Errorf("federationgql: no resource registered for reference target %q", ref.TargetType))
			continue
		}
		if refIDs == nil {
			cache.setErr(ref.TargetType, fmt.Errorf("federationgql: resource %q has no RefIDs", source.Type))
			continue
		}
		// Collect the distinct ids this reference names across all parents, then
		// narrow to the ones not yet fetched in this request.
		var want []string
		for _, pp := range parents {
			want = append(want, refIDs(ref, pp)...)
		}
		missing := cache.missingIDs(ref.TargetType, want)
		if len(missing) == 0 {
			continue // every referenced target already resolved this request
		}
		mask := targetReadMask(p, b.edgeFieldName(source, ref), target)
		byID, err := loadMissing(p.Context, b.resolver, ref, target, missing, mask)
		cache.markFetched(ref.TargetType, missing)
		if err != nil {
			cache.setErr(ref.TargetType, err)
			continue
		}
		for id, t := range byID {
			cache.put(ref.TargetType, id, t)
		}
	}
}

// loadMissing batch-fetches exactly the given target ids in ONE BatchGet through
// reference.Load, keyed by the target descriptor's IDOf. The target's client is
// resolved by reference.Load from the resolver; the mask (when supported by the
// downstream BatchGetter) is applied by the client the resolver returns — the
// gateway passes the mask via a MaskAwareBatchGetter when the client implements
// it, else fetches full (D-5). Because reference.Load is generic over the target
// Go type (any here), it batch-fetches through a BatchGetter[any] the resolver
// must supply for federation. The ids are passed as a single synthetic parent so
// reference.Load issues one BatchGet of exactly this id set.
func loadMissing(
	ctx context.Context,
	resolver reference.ReferenceResolver,
	ref reference.Reference,
	target *Resource,
	ids []string,
	mask []string,
) (map[string]any, error) {
	// Stash the mask on the context so a MaskAwareBatchGetter can read it (the
	// gateway pushes the read_mask down through the client without changing
	// reference.Load's signature).
	if len(mask) > 0 {
		ctx = withReadMask(ctx, ref.TargetType, mask)
	}
	idOf := target.IDOf
	return reference.Load[any, []string](
		ctx, resolver, ref, [][]string{ids},
		func(idset []string) []string { return idset },
		func(t any) string {
			if idOf == nil {
				return ""
			}
			return idOf(t)
		},
	)
}
