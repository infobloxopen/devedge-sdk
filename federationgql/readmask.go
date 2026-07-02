package federationgql

import (
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
)

// selfReadMask derives the downstream read_mask for the CURRENT resource field
// from its GraphQL selection set (D-5): each directly selected scalar field of
// the resource maps to its ScalarField.MaskPath. Edge sub-selections are
// ignored here (the edge preload derives the target's own mask). An empty result
// means "all fields" — the caller should push down no mask.
func selfReadMask(p graphql.ResolveParams, r *Resource) []string {
	maskByField := scalarMaskPaths(r)
	var paths []string
	seen := map[string]struct{}{}
	fragments := p.Info.Fragments
	for _, fld := range p.Info.FieldASTs {
		collectMaskPaths(fld.GetSelectionSet(), maskByField, fragments, seen, &paths)
	}
	return paths
}

// targetReadMask derives the read_mask for a reference edge's TARGET from the
// selection set(s) of that edge field within the current resource field's
// selection (D-5). edgeName is the GraphQL field name of the edge; target
// supplies the scalar→MaskPath mapping for the referenced type. It unions the
// masks of EVERY occurrence of the edge (the edge may appear under multiple
// aliases or merged from fragments, all of which the resolver must satisfy), so
// the pushed-down mask covers the full merged selection.
func targetReadMask(p graphql.ResolveParams, edgeName string, target *Resource) []string {
	maskByField := scalarMaskPaths(target)
	fragments := p.Info.Fragments
	var paths []string
	seen := map[string]struct{}{}
	for _, fld := range p.Info.FieldASTs {
		for _, edgeSel := range findFieldSelections(fld.GetSelectionSet(), edgeName, fragments) {
			collectMaskPaths(edgeSel, maskByField, fragments, seen, &paths)
		}
	}
	return paths
}

// scalarMaskPaths maps a resource's GraphQL scalar field names to their
// downstream read_mask paths (only fields that declare a non-empty MaskPath).
func scalarMaskPaths(r *Resource) map[string]string {
	m := make(map[string]string, len(r.Scalars))
	for _, sf := range r.Scalars {
		if sf.MaskPath != "" {
			m[sf.Name] = sf.MaskPath
		}
	}
	return m
}

// findFieldSelections returns the selection sets of EVERY occurrence of the edge
// field named name within ss (following fragment spreads / inline fragments).
// The edge is identified by its FIELD name (s.Name), not its response key — an
// aliased edge (`r: region { ... }`) still has s.Name == "region", so this
// matches it; the alias only changes the response key, which does not affect the
// pushed-down mask. Returning ALL occurrences (not just the first) unions the
// masks of an edge that appears more than once via aliases or fragment merges,
// which the resolver must jointly satisfy.
func findFieldSelections(ss *ast.SelectionSet, name string, fragments map[string]ast.Definition) []*ast.SelectionSet {
	if ss == nil {
		return nil
	}
	var out []*ast.SelectionSet
	for _, sel := range ss.Selections {
		switch s := sel.(type) {
		case *ast.Field:
			if s.Name != nil && s.Name.Value == name {
				out = append(out, s.GetSelectionSet())
			}
		case *ast.InlineFragment:
			out = append(out, findFieldSelections(s.GetSelectionSet(), name, fragments)...)
		case *ast.FragmentSpread:
			if s.Name != nil {
				if fd, ok := fragments[s.Name.Value].(*ast.FragmentDefinition); ok {
					out = append(out, findFieldSelections(fd.GetSelectionSet(), name, fragments)...)
				}
			}
		}
	}
	return out
}

// collectMaskPaths walks the immediate scalar selections of ss and appends each
// selected field's MaskPath (deduped) to out. Nested edge selections are not
// descended into — each collection derives its own mask.
func collectMaskPaths(ss *ast.SelectionSet, maskByField map[string]string, fragments map[string]ast.Definition, seen map[string]struct{}, out *[]string) {
	if ss == nil {
		return
	}
	for _, sel := range ss.Selections {
		switch s := sel.(type) {
		case *ast.Field:
			if s.Name == nil {
				continue
			}
			if path, ok := maskByField[s.Name.Value]; ok {
				if _, dup := seen[path]; !dup {
					seen[path] = struct{}{}
					*out = append(*out, path)
				}
			}
		case *ast.InlineFragment:
			collectMaskPaths(s.GetSelectionSet(), maskByField, fragments, seen, out)
		case *ast.FragmentSpread:
			if s.Name != nil {
				if fd, ok := fragments[s.Name.Value].(*ast.FragmentDefinition); ok {
					collectMaskPaths(fd.GetSelectionSet(), maskByField, fragments, seen, out)
				}
			}
		}
	}
}
