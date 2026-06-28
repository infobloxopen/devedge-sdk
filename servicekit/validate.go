package servicekit

import (
	"fmt"
	"strings"

	"github.com/infobloxopen/devedge-sdk/authz"
)

// ValidateModules runs the composition-time, descriptor-level checks the host
// applies before constructing the server (proposal §5.3 step 2): unique module
// IDs, and — across the union of descriptors — no duplicate gRPC service names,
// HTTP route prefixes, or permission names. It returns the first conflict found.
//
// It deliberately does NOT re-check authz-rule completeness (every registered
// method has a rule or a public exemption): that is the server's existing
// fail-closed union gate at server.Serve (server.go:337) — the host reuses it
// rather than inventing a parallel mechanism. Version-compatibility gating
// (Requires) is a later-phase concern (P4/P5).
func ValidateModules(mods []Module) error {
	if len(mods) == 0 {
		return fmt.Errorf("servicekit: no modules to run")
	}

	descs := make([]Descriptor, 0, len(mods))
	for i, m := range mods {
		if m == nil {
			return fmt.Errorf("servicekit: module at index %d is nil", i)
		}
		descs = append(descs, m.Descriptor())
	}
	return ValidateDescriptors(descs)
}

// ValidateDescriptors is the descriptor-only form of [ValidateModules], split out
// so it can be unit-tested without constructing real modules and reused by the
// later composition tooling (de compose tidy).
func ValidateDescriptors(descs []Descriptor) error {
	if len(descs) == 0 {
		return fmt.Errorf("servicekit: no module descriptors to validate")
	}

	seenID := map[string]struct{}{}
	seenService := map[string]string{}    // gRPC service name -> owning module ID
	seenPrefix := map[string]string{}     // route prefix -> owning module ID
	seenPermission := map[string]string{} // permission name -> owning module ID

	for _, d := range descs {
		if strings.TrimSpace(d.ID) == "" {
			return fmt.Errorf("servicekit: a module has an empty Descriptor.ID")
		}
		if _, dup := seenID[d.ID]; dup {
			return fmt.Errorf("servicekit: duplicate module ID %q", d.ID)
		}
		seenID[d.ID] = struct{}{}

		// Duplicate gRPC service names: derive the service name from each
		// FullMethod ("/pkg.Service/Method" -> "pkg.Service"). Two modules
		// registering the same service name would collide on the shared
		// grpc.Server. (Within one module the same service name repeats across its
		// methods — expected — so we dedup per module before crossing modules.)
		for svcName := range grpcServiceNames(d.Methods) {
			if owner, dup := seenService[svcName]; dup {
				return fmt.Errorf("servicekit: gRPC service %q is registered by both module %q and module %q", svcName, owner, d.ID)
			}
			seenService[svcName] = d.ID
		}

		// Duplicate HTTP route prefixes.
		for _, r := range d.Routes {
			prefix := strings.TrimSpace(r.Prefix)
			if prefix == "" {
				continue
			}
			key := r.Host + "\x00" + prefix
			if owner, dup := seenPrefix[key]; dup {
				return fmt.Errorf("servicekit: HTTP route prefix %q (host %q) is declared by both module %q and module %q", prefix, r.Host, owner, d.ID)
			}
			seenPrefix[key] = d.ID
		}

		// Duplicate permission names. A permission is the (verb, resource) pair a
		// rule declares; co-resident modules must not collide on it (unless
		// intentionally shared — out of scope for P1). Public exemptions carry no
		// permission, so they are skipped.
		for _, name := range permissionNames(d.AuthzRules) {
			if owner, dup := seenPermission[name]; dup {
				return fmt.Errorf("servicekit: permission %q is declared by both module %q and module %q", name, owner, d.ID)
			}
			seenPermission[name] = d.ID
		}
	}
	return nil
}

// grpcServiceNames returns the set of gRPC service names referenced by a set of
// FullMethods. A FullMethod looks like "/pkg.Service/Method"; the service name is
// "pkg.Service". Malformed entries are skipped.
func grpcServiceNames(methods []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, m := range methods {
		svc := grpcServiceName(m)
		if svc != "" {
			out[svc] = struct{}{}
		}
	}
	return out
}

// grpcServiceName extracts "pkg.Service" from "/pkg.Service/Method".
func grpcServiceName(fullMethod string) string {
	s := strings.TrimPrefix(fullMethod, "/")
	i := strings.LastIndex(s, "/")
	if i <= 0 {
		return ""
	}
	return s[:i]
}

// permissionNames returns the per-module-deduplicated permission names a rule set
// declares. A permission name is "<resource>:<verb>"; public exemptions (no verb,
// no resource) carry no permission and are skipped. Dedup within a module is
// expected (List and Get may share read:resource); the cross-module check in
// ValidateDescriptors catches a real collision between two different modules.
func permissionNames(rules []authz.MethodRule) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, r := range rules {
		if r.Public {
			continue
		}
		if r.Resource == "" && r.Verb == "" {
			continue
		}
		name := r.Resource + ":" + string(r.Verb)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}
