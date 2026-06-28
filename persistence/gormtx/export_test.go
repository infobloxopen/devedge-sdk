package gormtx

import (
	"context"
	"time"

	"gorm.io/gorm"

	"github.com/infobloxopen/devedge-sdk/cells"
)

// WithOutboxNowForTest exposes the unexported clock-override option to the
// external _test package so the outbox lease tests can drive lease expiry
// deterministically. It lives in a _test.go file, so it never ships in the real
// package — the public API keeps no test-only knob.
func WithOutboxNowForTest(now func() time.Time) OutboxOption { return withOutboxNow(now) }

// WriteGuardAllowForTest exposes the unexported fence predicate to the external
// _test package so the token match/mismatch/sealed/no-row cases are proven directly
// against a real fence table (no gorm-callback plumbing needed). table is the
// (possibly namespaced) tenant_fence table the guard reads.
func WriteGuardAllowForTest(db *gorm.DB, ctx context.Context, table, tenantID string, tok cells.AdmissionToken) (bool, error) {
	g := &writeGuard{table: table}
	return g.allow(db, ctx, tenantID, tok)
}

// IsFrameworkTableForTest exposes the framework-table guard shield to the external
// _test package.
func IsFrameworkTableForTest(table string) bool { return isFrameworkTable(table) }
