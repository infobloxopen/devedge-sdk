package federationv1_test

// noedge_test.go is the F041 / WS-021 P1 AC-4 assertion: a cross-service
// reference is METADATA, not a Go edge. The region_id field annotated with
// google.api.resource_reference must stay a plain scalar foreign key, so
// protoc-gen-ent must emit NO ent edge from Asset to Region and NO cascade —
// reads compose above the services, they are never traversed in-process.
//
// We assert this on the generated ent schema source directly (the source of
// truth the ent client is derived from): the Asset schema must declare region_id
// as a scalar field.String and must NOT declare any edge (edge.To/edge.From) at
// all — an edge to Region is the exact regression this fixture guards against.

import (
	"os"
	"strings"
	"testing"
)

func TestAsset_NoEntEdgeToRegion(t *testing.T) {
	// The test runs with cwd = the federationv1 package dir; the generated ent
	// schema is a sibling under ../ent/schema.
	const schemaPath = "../ent/schema/asset.go"
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read generated Asset ent schema %q: %v", schemaPath, err)
	}
	src := string(raw)

	// region_id must be present as a scalar string field (the metadata-only FK).
	if !strings.Contains(src, `field.String("region_id")`) {
		t.Errorf("Asset schema missing scalar region_id field; got:\n%s", src)
	}

	// No ent edge may reference Region — neither an owning edge.To nor an inverse
	// edge.From. A cross-service reference must never become a traversable edge.
	for _, forbidden := range []string{
		"edge.To",
		"edge.From",
		`"region"`, // the singular edge name protoc-gen-ent would use for a Region edge
		"Region.Type",
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("Asset schema contains %q — a cross-service reference must stay metadata-only (no ent edge/cascade); got:\n%s", forbidden, src)
		}
	}
}
