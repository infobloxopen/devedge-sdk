// Package health provides the readiness-check seam for devedge-sdk services.
//
// A Check reports whether a dependency is ready (nil error) or not (non-nil).
// Checks are registered on [server.Config.ReadinessChecks]; the server's /readyz
// HTTP endpoint and the gRPC Health service run all checks with a per-check 2s
// timeout and aggregate the result.
//
// The only built-in check is [DBCheck], which pings a *sql.DB (or any
// PingContext-capable connection). No ORM dependency enters core.
package health

import (
	"context"
	"fmt"
	"time"
)

// CheckTimeout is the per-check deadline applied by [Aggregate]. A hung dep
// can't stall the probe beyond this.
const CheckTimeout = 2 * time.Second

// Check is the readiness-check interface. Implement it to register a dep with
// the server's readiness aggregator.
type Check interface {
	// Name returns a short, stable identifier for the check (used as the JSON
	// key in the /readyz failure body, e.g. "db", "cache").
	Name() string
	// Check runs the probe. Returning nil means the dep is ready; any non-nil
	// error marks the dep as not ready and propagates to /readyz + gRPC health.
	Check(ctx context.Context) error
}

// Result holds the outcome of one check run.
type Result struct {
	Name string
	Err  error
}

// Aggregate runs all checks concurrently with [CheckTimeout] per check and
// returns the slice of results that failed (non-nil Err). An empty slice means
// all checks passed.
func Aggregate(ctx context.Context, checks []Check) []Result {
	if len(checks) == 0 {
		return nil
	}
	type item struct {
		name string
		err  error
	}
	ch := make(chan item, len(checks))
	for _, c := range checks {
		c := c
		go func() {
			cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
			defer cancel()
			ch <- item{name: c.Name(), err: c.Check(cctx)}
		}()
	}
	var failures []Result
	for range checks {
		it := <-ch
		if it.err != nil {
			failures = append(failures, Result{Name: it.name, Err: it.err})
		}
	}
	return failures
}

// Pinger is the minimal interface satisfied by *database/sql.DB (and by the
// *sql.DB returned by gorm's db.DB() or ent's sql.Open). No ORM import required.
type Pinger interface {
	PingContext(ctx context.Context) error
}

// DBCheck is a readiness Check that pings a database connection pool.
// Use [NewDBCheck] to construct one.
type DBCheck struct {
	name string
	db   Pinger
}

// NewDBCheck returns a Check that pings db on every probe. name is used as the
// JSON key in failure bodies (conventionally "db", "primary", etc.).
//
//	sqlDB, _ := gormDB.DB()
//	check := health.NewDBCheck("db", sqlDB)
//	s, _ := server.New(server.Config{ReadinessChecks: []health.Check{check}})
func NewDBCheck(name string, db Pinger) *DBCheck {
	return &DBCheck{name: name, db: db}
}

// Name implements Check.
func (c *DBCheck) Name() string { return c.name }

// Check implements Check. It pings the DB with the provided context (already
// deadline-bounded by the aggregator).
func (c *DBCheck) Check(ctx context.Context) error {
	if err := c.db.PingContext(ctx); err != nil {
		return fmt.Errorf("db ping: %w", err)
	}
	return nil
}
