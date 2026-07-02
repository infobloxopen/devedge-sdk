package federationgql

import (
	"fmt"
	"strings"

	"github.com/graphql-go/graphql"

	"github.com/infobloxopen/devedge-sdk/reference"
)

// buildQuery assembles the root Query object: for each resource a list field
// (lowercased type name + "s") and a get-by-id field (lowercased type name). The
// list resolver fetches the page then eagerly preloads every declared reference
// via reference.Load (D-3) so the edge resolvers cost no extra fetch; the get
// resolver preloads over the single fetched object.
func (b *builder) buildQuery(resources []Resource) (*graphql.Object, error) {
	fields := graphql.Fields{}
	seen := map[string]struct{}{}
	for i := range resources {
		r := &resources[i]
		obj := b.gqlByType[r.Type]
		if obj == nil {
			return nil, fmt.Errorf("federationgql: object type for %q was not built", r.Type)
		}
		listName := listFieldName(r.Name)
		getName := getFieldName(r.Name)
		if _, dup := seen[listName]; dup {
			return nil, fmt.Errorf("federationgql: duplicate root field %q", listName)
		}
		if _, dup := seen[getName]; dup {
			return nil, fmt.Errorf("federationgql: duplicate root field %q", getName)
		}
		seen[listName] = struct{}{}
		seen[getName] = struct{}{}

		res := r // capture
		fields[listName] = &graphql.Field{
			Type: graphql.NewList(obj),
			Args: graphql.FieldConfigArgument{
				"pageSize":  &graphql.ArgumentConfig{Type: graphql.Int},
				"pageToken": &graphql.ArgumentConfig{Type: graphql.String},
			},
			Resolve: b.listResolver(res),
		}
		fields[getName] = &graphql.Field{
			Type: obj,
			Args: graphql.FieldConfigArgument{
				"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			},
			Resolve: b.getResolver(res),
		}
	}
	return graphql.NewObject(graphql.ObjectConfig{Name: "Query", Fields: fields}), nil
}

// listResolver fetches a page of the resource then eagerly preloads its
// references (one BatchGet per referenced collection) so edge fields resolve
// from the request cache.
func (b *builder) listResolver(r *Resource) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (any, error) {
		if r.List == nil {
			return nil, fmt.Errorf("federationgql: resource %q has no List resolver", r.Type)
		}
		args := ListArgs{ReadMask: selfReadMask(p, r)}
		if v, ok := p.Args["pageSize"].(int); ok {
			args.PageSize = v
		}
		if v, ok := p.Args["pageToken"].(string); ok {
			args.PageToken = v
		}
		items, err := r.List(p.Context, args)
		if err != nil {
			return nil, err
		}
		b.preloadReferences(p, r, items)
		return items, nil
	}
}

// getResolver fetches one resource by id then preloads its references over that
// single object.
func (b *builder) getResolver(r *Resource) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (any, error) {
		if r.Get == nil {
			return nil, fmt.Errorf("federationgql: resource %q has no Get resolver", r.Type)
		}
		id, _ := p.Args["id"].(string)
		obj, err := r.Get(p.Context, GetArgs{ID: id, ReadMask: selfReadMask(p, r)})
		if err != nil {
			return nil, err
		}
		if obj == nil {
			return nil, nil
		}
		b.preloadReferences(p, r, []any{obj})
		return obj, nil
	}
}

// listFieldName is the root list field for a type name: lowercase first letter +
// "s" (e.g. "Asset" -> "assets", "Region" -> "regions").
func listFieldName(name string) string {
	return lowerFirst(name) + "s"
}

// getFieldName is the root get-by-id field for a type name: lowercase first
// letter (e.g. "Asset" -> "asset").
func getFieldName(name string) string {
	return lowerFirst(name)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// MaskAwareBatchGetter is a downstream reference client that honors an
// AIP-157 read_mask the gateway derived from the GraphQL selection set (D-5). A
// resolver's client that implements it reads the mask for the target type via
// [ReadMaskFromContext] and pushes it down to the service; a client that does
// NOT implement it (a plain [reference.BatchGetter]) fetches full and the
// GraphQL runtime projects the response. This interface documents the contract
// — reference.Load calls BatchGet through the client's own reference.BatchGetter
// method, so implementing masking is a matter of reading the context inside that
// method.
type MaskAwareBatchGetter[T any] interface {
	reference.BatchGetter[T]
}
