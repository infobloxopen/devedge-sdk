package entrepo

import (
	"github.com/infobloxopen/devedge-sdk/persistence"
)

// WS-012 P2 — DB module-namespacing for the ent backend.
//
// Unlike the gorm path, the ent adapter does NOT own table names: the ent CLIENT
// owns the schema (ent.Schema.Create / migrate), and queries go through the
// generated client, not a hand-held table name. So module-namespacing on the ent
// path is applied ONE level up — at the connection — by opening the ent client on a
// DSN whose Postgres search_path is the module schema. Then:
//
//   - ent.Schema.Create(ctx) creates the module's tables INSIDE the module schema
//     (the host creates the schema first, like the gorm migrator does);
//   - every generated query resolves unqualified table names through search_path,
//     so two co-resident modules' ent clients never collide.
//
// The framework outbox/idempotency stores a service mounts on the ent path
// (the EntOutboxStore in the iam fixture, or the gorm stores) are namespaced the
// same way: under schema isolation their bare table names resolve into the module
// schema via search_path; under prefix isolation use the gorm stores' With*Namespace
// options.
//
// This keeps the entrepo adapter dependency-light (it adds no schema-qualification
// machinery) while honoring the SAME persistence.DatabaseNamespace contract the gorm
// path does — the single source of truth.

// NamespacedDSN returns the module-namespaced Postgres DSN for an ent client: the
// module schema is appended to the connection search_path so ent.Schema.Create and
// all queries land in the module's schema. It delegates to the engine-level rule in
// the root persistence package (the same one gormtx uses), so the two adapters stay
// consistent. A namespace with no Schema (prefix/dedicated isolation) returns dsn
// unchanged — prefix isolation is a gorm-naming-strategy concern the ent path does
// not implement (use a dedicated database/DSN per module instead).
func NamespacedDSN(dsn string, ns persistence.DatabaseNamespace) string {
	return persistence.NamespacedPostgresDSN(dsn, ns)
}
