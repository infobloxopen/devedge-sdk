// outbox_partition.go — the F033 PostgreSQL declarative-partitioning DDL and the
// drop-partition retention for the GORM outbox store, plus a scheduler-agnostic
// retention helper.
//
// Why this lives in gormtx and not persistence: the partitioning DDL is dialect-
// and driver-specific (PostgreSQL `PARTITION BY RANGE`, `ATTACH/DETACH PARTITION`),
// so it belongs in the driver-aware store, keeping the persistence.OutboxStore /
// OutboxRetention interfaces ORM/driver-neutral (clean core). The built-in default
// is an APPEND-ONLY outbox whose retention is whole-partition drops (O(1) DDL), not
// per-row DELETE — so a high-throughput outbox never becomes write+delete-heavy.
package gormtx

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// outboxPartitionParentDDL is the column list / constraints of the WRITE-ONLY
// partitioned parent table. created_time is in the primary key because a PostgreSQL
// partition key must appear in every unique constraint of the partitioned table. F033:
// there are NO dispatcher-bookkeeping columns (no delivered_time / attempts /
// leased_until) — the table is written once (Append) and read forward (ReadAfter).
const outboxPartitionParentDDL = `
CREATE TABLE IF NOT EXISTS outbox (
	id             varchar(36) NOT NULL,
	account_id     text,
	aggregate_type text,
	aggregate_id   text,
	event_type     text,
	payload        bytea,
	created_time   timestamptz NOT NULL,
	PRIMARY KEY (id, created_time)
) PARTITION BY RANGE (created_time)`

// monthStart returns the first instant of t's UTC month — a partition boundary.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// addMonth returns the first instant of the month after t's UTC month.
func addMonth(t time.Time) time.Time {
	return monthStart(t).AddDate(0, 1, 0)
}

// partitionName is the deterministic child-table name for the month containing t,
// e.g. outbox_p2026_06. Deterministic naming lets EnsureOutboxPartitions be
// idempotent and DropPartitionsBefore locate partitions by their bound.
func partitionName(t time.Time) string {
	t = monthStart(t)
	return fmt.Sprintf("outbox_p%04d_%02d", t.Year(), int(t.Month()))
}

// EnsureOutboxPartitions creates (idempotently) the declarative
// RANGE-on-created_time parent table and one monthly partition for every month in
// [from, until], so appends in that window land in a real partition. It is the DDL
// the SQLite-friendly AutoMigrate(&OutboxRow{}) cannot produce (AutoMigrate makes a
// plain table); call it once at startup, then again from the retention task to roll
// the window forward.
//
// It is no-op-safe to call repeatedly: on PostgreSQL it uses CREATE TABLE IF NOT
// EXISTS for both the parent and each monthly child; on MySQL it CREATEs the
// partitioned parent if absent and ADD PARTITIONs only the months not already
// present. Monthly partitions span [month, next month).
//
// PostgreSQL and MySQL (P2) are supported. On any other dialect this returns an
// error so a caller does not silently get an unpartitioned table believing it is
// partitioned.
func EnsureOutboxPartitions(ctx context.Context, db *gorm.DB, from, until time.Time) error {
	if until.Before(from) {
		from, until = until, from
	}
	switch db.Dialector.Name() {
	case "postgres":
		if err := db.WithContext(ctx).Exec(outboxPartitionParentDDL).Error; err != nil {
			return fmt.Errorf("create partitioned outbox parent: %w", err)
		}
		for m := monthStart(from); !m.After(monthStart(until)); m = addMonth(m) {
			if err := ensureMonthlyPartition(ctx, db, m); err != nil {
				return err
			}
		}
		return nil
	case "mysql":
		return ensureMySQLOutboxPartitions(ctx, db, from, until)
	default:
		return fmt.Errorf("gormtx: EnsureOutboxPartitions supports postgres and mysql, got %q", db.Dialector.Name())
	}
}

// ensureMonthlyPartition attaches one monthly child partition covering
// [monthStart, nextMonth) if it does not already exist.
func ensureMonthlyPartition(ctx context.Context, db *gorm.DB, month time.Time) error {
	month = monthStart(month)
	next := addMonth(month)
	ddl := fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s PARTITION OF outbox FOR VALUES FROM ('%s') TO ('%s')`,
		partitionName(month),
		month.Format("2006-01-02 15:04:05-07"),
		next.Format("2006-01-02 15:04:05-07"),
	)
	if err := db.WithContext(ctx).Exec(ddl).Error; err != nil {
		return fmt.Errorf("create outbox partition %s: %w", partitionName(month), err)
	}
	return nil
}

// DropPartitionsBefore implements persistence.OutboxRetention for the GORM store.
//
// On PostgreSQL it drops every monthly partition whose ENTIRE window is older than
// t — a whole-partition DROP TABLE (O(1) DDL), never a per-row DELETE, so retention
// on a high-throughput append-only outbox does not churn the heap or stress vacuum
// (the F033 AC-2 guarantee). A partition whose window overlaps t is kept so an
// in-window row is never lost. It returns the number of partitions dropped.
//
// On any other dialect (e.g. the SQLite dev backend, which has no declarative
// partitioning) it models the same contract as a windowed delete of rows older than
// t — acceptable for the dev/test backend only, as the spec notes.
func (s *GormOutboxStore) DropPartitionsBefore(ctx context.Context, t time.Time) (int, error) {
	switch s.db.Dialector.Name() {
	case "postgres":
		return s.dropPGPartitionsBefore(ctx, t)
	case "mysql":
		return s.dropMySQLPartitionsBefore(ctx, t)
	}
	// Dev/test backend: no partitions, so "drop a partition" degrades to forgetting
	// rows older than t. This is the only delete path and it is NOT on the dispatch
	// loop (retention task only).
	res := s.db.WithContext(ctx).Where("created_time < ?", t).Delete(&OutboxRow{})
	if res.Error != nil {
		return 0, fmt.Errorf("drop outbox rows before %s: %w", t.Format(time.RFC3339), res.Error)
	}
	return int(res.RowsAffected), nil
}

// dropPGPartitionsBefore drops each monthly partition of outbox whose upper bound is
// at or before monthStart(t) — i.e. every partition entirely older than t's month.
// It discovers partitions from the catalog (pg_inherits) and inspects each child's
// RANGE bound, so it drops exactly the aged partitions regardless of how they were
// named.
func (s *GormOutboxStore) dropPGPartitionsBefore(ctx context.Context, t time.Time) (int, error) {
	cutoff := monthStart(t)
	// For a monthly RANGE partition created by EnsureOutboxPartitions the name encodes
	// the month, so derive each partition's [from, upper) window from its name rather
	// than parsing the catalog bound expression. List the partitions of "outbox" via
	// the inheritance catalog.
	var children []string
	err := s.db.WithContext(ctx).Raw(`
		SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c   ON c.oid = i.inhrelid
		JOIN pg_class p   ON p.oid = i.inhparent
		WHERE p.relname = 'outbox'
	`).Scan(&children).Error
	if err != nil {
		return 0, fmt.Errorf("list outbox partitions: %w", err)
	}
	dropped := 0
	for _, child := range children {
		upper, ok := partitionUpperBound(child)
		if !ok {
			continue // not a name we manage; leave it alone
		}
		// Drop only if the partition's whole window is strictly older than the cutoff
		// month (upper bound <= cutoff month start means [from, upper) is entirely before t).
		if !upper.After(cutoff) {
			if err := s.db.WithContext(ctx).Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", child)).Error; err != nil {
				return dropped, fmt.Errorf("drop outbox partition %s: %w", child, err)
			}
			dropped++
		}
	}
	return dropped, nil
}

// partitionUpperBound parses a partition name of the form outbox_pYYYY_MM produced by
// EnsureOutboxPartitions and returns the partition's exclusive upper bound (the first
// instant of the FOLLOWING month). ok is false for names we did not create.
func partitionUpperBound(name string) (time.Time, bool) {
	var year, month int
	if _, err := fmt.Sscanf(name, "outbox_p%04d_%02d", &year, &month); err != nil {
		return time.Time{}, false
	}
	if month < 1 || month > 12 {
		return time.Time{}, false
	}
	start := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	return addMonth(start), true
}

// RunRetention is the scheduler-agnostic retention HELPER the F033 spec calls for:
// the SDK does NOT own a cron loop, so a service wires this into its own scheduler
// (a ticker, a k8s CronJob, etc.). One call (a) rolls the partition window forward
// so this month and next month always have a real partition to append into, then
// (b) drops every partition older than the retention window. window is the retention
// horizon (e.g. 30 days); a non-positive window uses the F033 default of ~30 days.
//
// It operates only through the OutboxRetention seam plus EnsureOutboxPartitions, so
// it never touches the dispatch path and never issues a per-row DELETE on the
// PostgreSQL partitioned table — retention is whole-partition drops.
func RunRetention(ctx context.Context, db *gorm.DB, store persistence.OutboxRetention, window time.Duration) (dropped int, err error) {
	if window <= 0 {
		window = 30 * 24 * time.Hour
	}
	now := time.Now().UTC()
	// Roll the window forward: make sure this month and next month have partitions so
	// appends never hit a missing-partition error at a month boundary. Only meaningful
	// on the partitioning dialects (PostgreSQL, MySQL); EnsureOutboxPartitions errors
	// on other dialects, which we tolerate for the dev backend (it has no partitions to
	// ensure).
	if db != nil {
		if name := db.Dialector.Name(); name == "postgres" || name == "mysql" {
			if eerr := EnsureOutboxPartitions(ctx, db, now, addMonth(now)); eerr != nil {
				return 0, fmt.Errorf("ensure forward partitions: %w", eerr)
			}
		}
	}
	cutoff := now.Add(-window)
	return store.DropPartitionsBefore(ctx, cutoff)
}
