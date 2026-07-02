// Package federationgql is the cross-service GraphQL federation gateway (F042,
// WS-021 P3). It turns the F041 reference primitives — the generated
// <Svc>References metadata and the guaranteed AIP-137 BatchGet on referenced
// targets — into a queryable graph: each resource becomes a GraphQL object type,
// each declared cross-service [reference.Reference] becomes an object-typed edge
// field, and a client issues ONE GraphQL query that spans microservice moats.
//
// # What it is (and is not)
//
// The gateway is stateless composition. It holds no datastore and makes ZERO
// authz decisions: it propagates the caller's execution context (principal /
// metadata) unchanged into every downstream call, so a per-service
// PermissionDenied surfaces as a per-field GraphQL error (null + errors[]),
// never a bypass (F042 G-3 / D-4). It composes reads only — mutations and
// subscriptions are out of scope (the DDD write boundary holds; writes route to
// the owning service).
//
// # The anti-N+1 guarantee, end to end
//
// Edge resolution goes through [reference.Load], which resolves N parents'
// references in exactly ONE BatchGet per distinct target-id set. The gateway
// preserves that guarantee across a real service boundary with EAGER
// per-collection preload (D-3): the list/get root resolver runs [reference.Load]
// for each declared reference up front and stashes the resolved targets in a
// request-scoped cache; the edge field resolver reads from that cache. So for a
// list-of-N → edge query the target service receives deterministically one
// BatchGet, proven by the sample's e2e spy (F042 AC-3).
//
// # read_mask pushdown
//
// Where a descriptor supports a field mask, the gateway derives it from the
// GraphQL selection set (the requested scalar fields) and passes it down so the
// downstream service honors AIP-157 (D-5). A descriptor that does not support a
// mask fetches full and the GraphQL runtime projects the response.
//
// # Isolation
//
// This package lives in its own Go module so the GraphQL runtime library is a
// transitive dependency, never part of a server-only consumer's module graph
// (F042 AC-6 / the repo's check-graph-isolation gate).
package federationgql

import (
	"context"
	"fmt"

	"github.com/graphql-go/graphql"

	"github.com/infobloxopen/devedge-sdk/reference"
)

// ScalarField is one non-edge field on a resource's GraphQL type: an id or a
// plain scalar. Resolve extracts the value from a source object (the concrete
// value the descriptor's Get/List returned, e.g. *regionv1.Region). MaskPath is
// the downstream field name (AIP-157 read_mask path, typically the proto field
// name) this GraphQL field maps to; it drives read_mask pushdown (D-5). When
// MaskPath is empty the field contributes nothing to the derived mask.
type ScalarField struct {
	// Name is the GraphQL field name (e.g. "id", "name").
	Name string
	// Type is the GraphQL scalar type. Defaults to graphql.String when nil.
	Type *graphql.Scalar
	// Resolve extracts this field's value from a source object.
	Resolve func(source any) any
	// MaskPath is the downstream read_mask path this field maps to (e.g.
	// "display_name"). Empty means the field does not participate in mask
	// pushdown.
	MaskPath string
}

// ListArgs are the arguments a root list resolver receives, derived from the
// GraphQL query. ReadMask is the set of downstream field paths the selection set
// requested (empty = all fields); a descriptor whose List accepts a field mask
// pushes it down (D-5).
type ListArgs struct {
	// PageSize is the requested page size (0 = server default).
	PageSize int
	// PageToken is the pagination cursor (empty = first page).
	PageToken string
	// ReadMask is the downstream field paths the selection set requested.
	ReadMask []string
}

// GetArgs are the arguments a root get resolver receives. ReadMask carries the
// selection-set-derived field paths for pushdown (D-5).
type GetArgs struct {
	// ID is the resource id to fetch.
	ID string
	// ReadMask is the downstream field paths the selection set requested.
	ReadMask []string
}

// Resource is the explicit descriptor for one federated resource: how it maps to
// a GraphQL object type, which cross-service references become edges, and the
// closures that fetch it from its owning service. The wiring is deliberately
// explicit (not derived from a catalog) — that registration step is what the
// setup skill (F042 G-6) walks an agent through.
type Resource struct {
	// Type is the resource's AIP-122 type (e.g. "asset.example.com/Asset"). It
	// keys the request-scoped edge cache and matches a [reference.Reference]'s
	// TargetType so an edge can find its target Resource.
	Type string
	// Name is the GraphQL object type name (e.g. "Asset"). Must be a valid
	// GraphQL name and unique across the schema.
	Name string
	// Scalars are the id + scalar fields of the GraphQL type.
	Scalars []ScalarField
	// References are the cross-service references declared on this resource,
	// taken verbatim from the generated <Svc>References table. Each becomes an
	// object-typed edge field named after the target Resource, resolved through
	// [reference.Load].
	References []reference.Reference
	// Get fetches one resource by id from its owning service. Required for the
	// root get(id) field.
	Get func(ctx context.Context, args GetArgs) (any, error)
	// List fetches a page of resources from its owning service. Required for the
	// root list field.
	List func(ctx context.Context, args ListArgs) ([]any, error)
	// IDOf extracts a source object's id — used to key the resource in the edge
	// cache and as the BatchGet key of a referenced target.
	IDOf func(source any) string
	// RefIDs extracts the foreign-key id(s) a source object names for a given
	// reference (e.g. the RegionId on an Asset). It returns a slice so a
	// [reference.Many] reference works unchanged; a [reference.One] reference
	// returns a single-element slice.
	RefIDs func(ref reference.Reference, source any) []string
	// EdgeFieldName overrides the GraphQL field name for the edge of a given
	// reference. When nil (or it returns ""), the edge is named after the target
	// Resource's Name lowercased (e.g. Region -> "region"). Used when two
	// references point at the same target type and would otherwise collide.
	EdgeFieldName func(ref reference.Reference) string
}

// NewSchema builds a GraphQL schema from the resource descriptors and the
// reference resolver. Each Resource becomes a GraphQL object type with its
// scalar fields; each declared reference becomes an object-typed edge field
// resolved through [reference.Load] against the resolver (one BatchGet per
// referenced collection, D-3). The root Query type exposes, per resource, a
// list field (named after the pluralized-ish lowercase type name) and a
// get-by-id field.
//
// It fails if a resource's Name/Type is empty or duplicated, if a reference
// names a TargetType with no registered Resource, or if the assembled schema is
// invalid.
func NewSchema(resources []Resource, resolver reference.ReferenceResolver) (graphql.Schema, error) {
	if resolver == nil {
		return graphql.Schema{}, fmt.Errorf("federationgql: resolver is required")
	}
	b := &builder{
		resolver:    resolver,
		byType:      make(map[string]*Resource, len(resources)),
		gqlByType:   make(map[string]*graphql.Object, len(resources)),
		fieldThunks: make(map[string]graphql.FieldsThunk, len(resources)),
	}
	// Index resources by AIP-122 type and validate uniqueness.
	seenName := make(map[string]struct{}, len(resources))
	for i := range resources {
		r := &resources[i]
		if r.Type == "" {
			return graphql.Schema{}, fmt.Errorf("federationgql: resource %q has empty Type", r.Name)
		}
		if r.Name == "" {
			return graphql.Schema{}, fmt.Errorf("federationgql: resource %q has empty Name", r.Type)
		}
		if _, dup := b.byType[r.Type]; dup {
			return graphql.Schema{}, fmt.Errorf("federationgql: duplicate resource Type %q", r.Type)
		}
		if _, dup := seenName[r.Name]; dup {
			return graphql.Schema{}, fmt.Errorf("federationgql: duplicate resource Name %q", r.Name)
		}
		b.byType[r.Type] = r
		seenName[r.Name] = struct{}{}
	}

	// Build the GraphQL object types. Fields are provided via a thunk so an edge
	// can reference a target type declared later (GraphQL's forward-reference
	// pattern for cyclic/ordering-independent type graphs).
	for i := range resources {
		r := &resources[i]
		b.buildObject(r)
	}

	// Assemble the root Query type: per-resource list + get(id).
	query, err := b.buildQuery(resources)
	if err != nil {
		return graphql.Schema{}, err
	}

	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: query})
	if err != nil {
		return graphql.Schema{}, fmt.Errorf("federationgql: build schema: %w", err)
	}
	return schema, nil
}

// builder holds the mutable state while assembling the schema.
type builder struct {
	resolver  reference.ReferenceResolver
	byType    map[string]*Resource
	gqlByType map[string]*graphql.Object
	// fieldThunks defers field construction so an edge can point at a target
	// object type built in any order.
	fieldThunks map[string]graphql.FieldsThunk
}
