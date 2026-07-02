package migrate

import (
	"embed"
	"io/fs"
)

// baselineFS embeds the SDK-owned framework migration baseline — the generated
// 0001_framework_init.{up,down}.sql that materializes the framework tables (outbox incl.
// the WS-008 event_seq/event_epoch + created_time partition-key columns, idempotency,
// dispatch cursor, dead-letter, and the cell-development tenant_fence/tenant_event_seq/
// tenant_event_policy tables). It is generated from the canonical gormtx framework model
// set by Atlas (see atlas.hcl + schemagen/, build-time only) and drift-checked in CI.
//
//go:embed baseline/*.sql
var baselineFS embed.FS

// FrameworkBaseline returns the SDK-owned framework migration baseline as an fs.FS
// rooted at the migration files (0001_framework_init.{up,down}.sql). A host composes it
// AHEAD of a module's own migrations FS (see [Config.FrameworkBaseline]) so every
// devedge service builds its versioned schema forward from the framework's 0001. The
// baseline replaces AutoMigrate for the framework tables on the Postgres/MySQL path.
func FrameworkBaseline() fs.FS {
	sub, err := fs.Sub(baselineFS, "baseline")
	if err != nil {
		// baseline/ is embedded at build time, so this cannot fail; return the whole
		// FS rather than panic in a library.
		return baselineFS
	}
	return sub
}
