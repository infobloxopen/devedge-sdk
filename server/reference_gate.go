package server

import (
	"fmt"
	"sort"
	"strings"

	"github.com/infobloxopen/devedge-sdk/reference"
)

// AssertReferenceTargets returns an error if any declared cross-service reference
// names a target resource type that is NOT batch-fetchable — i.e. no service on
// this server registered a generated AIP-137 BatchGet for that type (F041 G-3).
//
// batchTargets is the set of resource types that serve BatchGet (recorded by the
// generated Register<Svc> via [Server.RecordBatchTarget]); references is the
// accumulated set of cross-service references (via [Server.RecordReferences]). It
// is a pure function over (batchTargets, references) so it is unit-testable, and
// it runs at Serve beside the authz completeness and aggregate boundary gates.
//
// This is the registration-time BACKSTOP of the fail-loud rule: the primary gate
// is at codegen (protoc-gen-svc makes a reference target's repository a
// persistence.BatchRepository, so a non-batch repo fails to compile). This gate
// additionally catches cross-repo / version skew that local codegen cannot see —
// a reference to a target served by a DIFFERENT service/binary that does not
// expose BatchGet. Fail-closed: an unresolvable reference must not serve, never a
// silent runtime N+1.
func AssertReferenceTargets(batchTargets map[string]struct{}, references []reference.Reference) error {
	var violations []string
	for _, r := range references {
		if _, ok := batchTargets[r.TargetType]; ok {
			continue
		}
		violations = append(violations, fmt.Sprintf(
			"reference field %q (%s) → target type %q has no registered BatchGet on this server",
			r.FieldName, r.FKField, r.TargetType))
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		return fmt.Errorf(
			"server: %d cross-service reference(s) target a resource type with no BatchGet — a referenced target must be batch-fetchable (register a service exposing BatchGet<Target>, or the composition would N+1): %s",
			len(violations), strings.Join(violations, "; "))
	}
	return nil
}
