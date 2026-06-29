// outbox_partition_mysql.go — the F033 Phase-2 MySQL RANGE-partitioning DDL and the
// drop-partition retention for the GORM outbox store. It is the MySQL twin of the
// PostgreSQL DDL in outbox_partition.go.
//
// Why MySQL needs its own DDL: MySQL does not have PostgreSQL's declarative
// PARTITION OF children-as-tables model. Instead a single CREATE TABLE declares the
// whole partition scheme inline (PARTITION BY RANGE (<int expr>) (PARTITION p... )),
// later partitions are appended with ALTER TABLE ... ADD PARTITION (strictly
// increasing upper bounds), and a partition is removed with ALTER TABLE ... DROP
// PARTITION — the O(1) DDL drop F033 requires (AC-2), never a per-row DELETE.
//
// The RANGE expression must be an INTEGER function of columns that appear in every
// unique/primary key. created_time is in the primary key (id, created_time) and
// TO_DAYS(created_time) is one of MySQL's permitted partitioning functions, so the
// monthly RANGE is TO_DAYS(created_time) with each partition's upper bound the
// TO_DAYS of the FIRST day of the following month — i.e. partition pYYYYMM holds the
// month [YYYY-MM-01, next-month-01).
package gormtx

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// mysqlPartitionName is the deterministic child-partition name for the month
// containing t, e.g. p202606. Deterministic naming lets EnsureOutboxPartitions add
// only the months not already present and DropPartitionsBefore locate aged ones.
func mysqlPartitionName(t time.Time) string {
	t = monthStart(t)
	return fmt.Sprintf("p%04d%02d", t.Year(), int(t.Month()))
}

// mysqlPartitionUpperDays is the RANGE upper bound for the month containing t: the
// TO_DAYS of the first instant of the FOLLOWING month, so the partition spans
// [month, next month). MySQL's TO_DAYS is the day number since year 0; it matches
// what the server computes for TO_DAYS('YYYY-MM-DD'), so the bound is exact.
func mysqlPartitionUpperDays(t time.Time) int {
	return toDays(addMonth(t))
}

// toDaysSinceUnixEpoch is MySQL's TO_DAYS('1970-01-01'): the proleptic-Gregorian day
// number MySQL assigns to the Unix epoch. TO_DAYS counts days since 0000-01-01 (day 1),
// so the offset lets us compute TO_DAYS(t) from t's Unix day without spanning ~2000
// years (which would overflow time.Duration in t.Sub(year0)).
const toDaysSinceUnixEpoch = 719528

// toDays mirrors MySQL's TO_DAYS(date): the proleptic-Gregorian day count MySQL uses.
// Computing it in Go (rather than emitting TO_DAYS('...') for the server to evaluate)
// keeps the partition bound a plain integer literal, which is what ALTER TABLE ... DROP
// PARTITION matching by name needs, and avoids any server-side timezone interpretation
// of a date literal. Partition boundaries are always month starts at 00:00:00 UTC, so
// dividing the Unix second count by 86400 is exact.
func toDays(t time.Time) int {
	return int(t.UTC().Unix()/86400) + toDaysSinceUnixEpoch
}

// mysqlOutboxParentDDLFmt is the MySQL declarative RANGE-partitioned parent for the
// WRITE-ONLY gormtx "outbox" table. created_time is in the primary key because a MySQL
// partition column must appear in every unique key. F033: no dispatcher-bookkeeping
// columns — the table is written once and read forward. The %s is the inline partition
// list (at least one partition; MySQL requires it).
const mysqlOutboxParentDDLFmt = `
CREATE TABLE IF NOT EXISTS outbox (
	id             varchar(36) NOT NULL,
	account_id     varchar(255),
	aggregate_type varchar(255),
	aggregate_id   varchar(255),
	event_type     varchar(255),
	payload        longblob,
	created_time   datetime(6) NOT NULL,
	event_seq      bigint NOT NULL DEFAULT 0,
	event_epoch    bigint NOT NULL DEFAULT 0,
	PRIMARY KEY (id, created_time)
) PARTITION BY RANGE (TO_DAYS(created_time)) (
%s
)`

// ensureMySQLOutboxPartitions creates the partitioned "outbox" parent (if absent)
// with a partition for every month in [from, until], or, if the table already
// exists, ADDs only the months not yet present. ADD PARTITION requires strictly
// increasing upper bounds, so missing months are added in ascending order and only
// those whose bound exceeds the current max partition are added.
func ensureMySQLOutboxPartitions(ctx context.Context, db *gorm.DB, from, until time.Time) error {
	exists, err := mysqlTableExists(ctx, db, "outbox")
	if err != nil {
		return err
	}
	if !exists {
		var defs []string
		for m := monthStart(from); !m.After(monthStart(until)); m = addMonth(m) {
			defs = append(defs, fmt.Sprintf("\tPARTITION %s VALUES LESS THAN (%d)",
				mysqlPartitionName(m), mysqlPartitionUpperDays(m)))
		}
		ddl := fmt.Sprintf(mysqlOutboxParentDDLFmt, strings.Join(defs, ",\n"))
		if err := db.WithContext(ctx).Exec(ddl).Error; err != nil {
			return fmt.Errorf("create partitioned outbox parent (mysql): %w", err)
		}
		return nil
	}
	// Table exists: ADD only months not already present, in ascending order.
	have, err := mysqlExistingPartitions(ctx, db, "outbox")
	if err != nil {
		return err
	}
	for m := monthStart(from); !m.After(monthStart(until)); m = addMonth(m) {
		name := mysqlPartitionName(m)
		if have[name] {
			continue
		}
		ddl := fmt.Sprintf("ALTER TABLE outbox ADD PARTITION (PARTITION %s VALUES LESS THAN (%d))",
			name, mysqlPartitionUpperDays(m))
		if err := db.WithContext(ctx).Exec(ddl).Error; err != nil {
			return fmt.Errorf("add outbox partition %s (mysql): %w", name, err)
		}
	}
	return nil
}

// dropMySQLPartitionsBefore drops every monthly partition of "outbox" whose ENTIRE
// window is older than t — ALTER TABLE ... DROP PARTITION (O(1) DDL), never a per-row
// DELETE (F033 AC-2). It discovers partitions from information_schema.partitions and
// derives each partition's window from its deterministic pYYYYMM name. It returns the
// number of partitions dropped. MySQL forbids dropping the last partition of a
// partitioned table, so if the cutoff would drop them all, the oldest is kept.
func (s *GormOutboxStore) dropMySQLPartitionsBefore(ctx context.Context, t time.Time) (int, error) {
	cutoff := monthStart(t)
	have, err := mysqlExistingPartitions(ctx, s.db.WithContext(ctx), "outbox")
	if err != nil {
		return 0, err
	}
	// Collect aged partitions (upper bound at or before the cutoff month).
	var aged []string
	for name := range have {
		upper, ok := mysqlPartitionUpperBound(name)
		if !ok {
			continue // not a name we manage; leave it alone
		}
		if !upper.After(cutoff) {
			aged = append(aged, name)
		}
	}
	if len(aged) == 0 {
		return 0, nil
	}
	// Never drop the final partition (MySQL errors). If every partition is aged, keep
	// the newest of the aged set so the table keeps at least one partition.
	if len(aged) == len(have) {
		aged = dropNewest(aged)
	}
	dropped := 0
	for _, name := range aged {
		if err := s.db.WithContext(ctx).Exec(fmt.Sprintf("ALTER TABLE outbox DROP PARTITION %s", name)).Error; err != nil {
			return dropped, fmt.Errorf("drop outbox partition %s (mysql): %w", name, err)
		}
		dropped++
	}
	return dropped, nil
}

// mysqlPartitionUpperBound parses a pYYYYMM partition name and returns its exclusive
// upper bound (the first instant of the following month). ok is false for names we
// did not create.
func mysqlPartitionUpperBound(name string) (time.Time, bool) {
	var year, month int
	if _, err := fmt.Sscanf(name, "p%04d%02d", &year, &month); err != nil {
		return time.Time{}, false
	}
	if month < 1 || month > 12 {
		return time.Time{}, false
	}
	start := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	return addMonth(start), true
}

// dropNewest removes the lexicographically-greatest pYYYYMM name (which, for the
// zero-padded YYYYMM form, is the newest month) from names, returning the rest.
func dropNewest(names []string) []string {
	if len(names) == 0 {
		return names
	}
	maxIdx := 0
	for i := 1; i < len(names); i++ {
		if names[i] > names[maxIdx] {
			maxIdx = i
		}
	}
	out := make([]string, 0, len(names)-1)
	for i, n := range names {
		if i == maxIdx {
			continue
		}
		out = append(out, n)
	}
	return out
}

// mysqlTableExists reports whether table exists in the current schema.
func mysqlTableExists(ctx context.Context, db *gorm.DB, table string) (bool, error) {
	var n int
	err := db.WithContext(ctx).Raw(`
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&n).Error
	if err != nil {
		return false, fmt.Errorf("check table %s exists (mysql): %w", table, err)
	}
	return n > 0, nil
}

// mysqlExistingPartitions returns the set of named partitions of table in the current
// schema (empty when the table is unpartitioned).
func mysqlExistingPartitions(ctx context.Context, db *gorm.DB, table string) (map[string]bool, error) {
	var names []string
	err := db.WithContext(ctx).Raw(`
		SELECT partition_name FROM information_schema.partitions
		WHERE table_schema = DATABASE() AND table_name = ? AND partition_name IS NOT NULL`, table).
		Scan(&names).Error
	if err != nil {
		return nil, fmt.Errorf("list outbox partitions (mysql): %w", err)
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}
