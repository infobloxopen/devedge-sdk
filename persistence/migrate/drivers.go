// Package migrate is devedge-sdk's versioned-SQL migration engine: it applies a
// module's embedded, sequentially-numbered SQL migrations to a Postgres schema-of-record
// through the Infoblox golang-migrate fork (github.com/infobloxopen/migrate, branch ib,
// pulled in via the go.mod replace of github.com/golang-migrate/migrate/v4). It is the
// SDK-side parity for devedge's internal ForkApplier: it targets the highest version,
// enables the fork's persisted down-store + dirty-state recovery (WithDirtyStateConfig),
// normalizes the DSN to the pgx/v5 scheme, and runs on a SAFE connection
// (lock_timeout/statement_timeout + per-module search_path).
//
// It lives in its OWN nested module so neither golang-migrate nor the pgx driver enters
// the root module's dependency graph or a server-only consumer's build closure
// (check-graph-isolation stays green). The engine is BACKEND-AGNOSTIC — it applies SQL
// files to a DSN — so it depends on neither gorm nor ent; the ent and GORM host paths
// both drive this SAME applier over the module's embedded migrations FS. AutoMigrate is
// kept ONLY as the SQLite dev/test fast-path (F043 D-2); Postgres/MySQL use this engine.
package migrate

import (
	// Database driver: Postgres over pgx/v5, registered under the "pgx5" scheme for
	// database.Open and (via jackc's stdlib) the "pgx" database/sql driver used for the
	// advisory-lock connection.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"

	// Source driver: local migration files. The applier materializes the composed
	// (framework baseline + module) FS onto disk and reads it as file:// so the fork's
	// persisted down-store (a directory) works.
	_ "github.com/golang-migrate/migrate/v4/source/file"
)
