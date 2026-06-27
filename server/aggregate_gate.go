package server

import (
	"fmt"
	"sort"
	"strings"
)

// MemberBinding records that a service's resource is a DDD aggregate MEMBER owned
// by Root, together with the write-capable standard methods (Create/Update/
// Delete/Undelete/Batch*) the service registers on the transport surface. The
// generated Register<Svc> contributes one MemberBinding per member service (via
// [Server.RecordMemberBinding]); the boot-time boundary gate
// [AssertAggregateBoundaries] reads the accumulated set at Serve.
//
// A member resource is addressable for READS (Get/List) but written THROUGH its
// root, so a registered member write is a boundary violation that fails closed —
// mirroring the authz completeness gate (an undeclared method is denied).
type MemberBinding struct {
	// Resource is the member resource's Go type name (e.g. "Item").
	Resource string
	// Root is the owning aggregate root's message name (e.g. "Order").
	Root string
	// WriteMethods are the gRPC FullMethod names of the write-capable standard
	// methods this member service registers (e.g.
	// "/order.v1.ItemService/CreateItem"). A non-empty intersection with the
	// registered method set is the boundary violation the gate fails on.
	WriteMethods []string
}

// AssertAggregateBoundaries returns an error if any aggregate MEMBER resource
// registers a write-capable standard method on the transport surface. methods is
// the full set of registered gRPC FullMethods (as recorded by RecordMethods);
// members is the accumulated set of member→root bindings. It is a pure function
// over (methods, members) so it can be unit-tested directly, and it is run at
// Serve beside the authz completeness gate (fail-closed, default-deny: a member
// resource cannot independently mutate — writes route through the root).
//
// Reads (Get/List) are intentionally NOT considered: addressability is not write
// authority, and a read-only projection of a member is not a member write. Only
// the WriteMethods recorded on each binding are checked.
func AssertAggregateBoundaries(methods []string, members []MemberBinding) error {
	registered := make(map[string]struct{}, len(methods))
	for _, m := range methods {
		registered[m] = struct{}{}
	}
	var violations []string
	for _, mb := range members {
		for _, w := range mb.WriteMethods {
			if _, ok := registered[w]; ok {
				violations = append(violations, fmt.Sprintf(
					"%s (member of aggregate %s) registers write method %s", mb.Resource, mb.Root, w))
			}
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		return fmt.Errorf(
			"server: %d aggregate boundary violation(s) — a member resource must not register write methods (route writes through the aggregate root; keep Get/List): %s",
			len(violations), strings.Join(violations, "; "))
	}
	return nil
}
