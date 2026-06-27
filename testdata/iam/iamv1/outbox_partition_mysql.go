// outbox_partition_mysql.go — the F033 Phase-2 MySQL RANGE-partitioning DDL and
// drop-partition retention for the ent-backed IAM outbox store ("outboxes"). It is
// the MySQL twin of the PostgreSQL DDL in outbox_partition.go and mirrors the gormtx
// MySQL DDL (persistence/gormtx/outbox_partition_mysql.go), driving a raw *sql.DB
// because ent has no notion of declarative partitioning.
//
// MySQL declares the whole partition scheme inline on CREATE TABLE, appends later
// months with ALTER TABLE ... ADD PARTITION (strictly increasing upper bounds), and
// removes an aged month with ALTER TABLE ... DROP PARTITION — the O(1) DDL drop F033
// requires (AC-2), never a per-row DELETE. The RANGE is TO_DAYS(created_time) with
// each partition's upper bound the TO_DAYS of the first day of the following month
// (so partition pYYYYMM holds [YYYY-MM-01, next-month-01)); created_time is in the
// primary key (id, created_time) as MySQL requires of a partition column.
package iamv1

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// mysqlEntOutboxParentDDLFmt is the MySQL declarative RANGE-partitioned parent for the
// ent "outboxes" table. The %s is the inline partition list (MySQL requires >= 1).
const mysqlEntOutboxParentDDLFmt = `
CREATE TABLE IF NOT EXISTS outboxes (
	id             varchar(255) NOT NULL,
	account_id     varchar(255),
	aggregate_type varchar(255),
	aggregate_id   varchar(255),
	event_type     varchar(255),
	payload        longblob,
	created_time   datetime(6) NOT NULL,
	delivered_time datetime(6),
	attempts       bigint NOT NULL DEFAULT 0,
	leased_until   datetime(6),
	PRIMARY KEY (id, created_time),
	KEY idx_outboxes_claim (attempts, leased_until)
) PARTITION BY RANGE (TO_DAYS(created_time)) (
%s
)`

// entMySQLPartitionName is the deterministic child-partition name for t's month, e.g.
// p202606 (the same scheme the gormtx MySQL DDL uses).
func entMySQLPartitionName(t time.Time) string {
	t = entMonthStart(t)
	return fmt.Sprintf("p%04d%02d", t.Year(), int(t.Month()))
}

// entToDaysSinceUnixEpoch is MySQL's TO_DAYS('1970-01-01'): see the gormtx twin
// (persistence/gormtx/outbox_partition_mysql.go) for why the offset is used.
const entToDaysSinceUnixEpoch = 719528

// entMySQLToDays mirrors MySQL's TO_DAYS(date): the proleptic-Gregorian day count
// MySQL uses. Computing the integer bound in Go keeps the partition bound a plain
// literal (required so DROP/ADD PARTITION match by name) and avoids server-side
// timezone interpretation. Boundaries are month starts at 00:00:00 UTC, so dividing
// the Unix second count by 86400 is exact.
func entMySQLToDays(t time.Time) int {
	return int(t.UTC().Unix()/86400) + entToDaysSinceUnixEpoch
}

// entMySQLPartitionUpperDays is the RANGE upper bound for t's month: TO_DAYS of the
// first instant of the following month.
func entMySQLPartitionUpperDays(t time.Time) int { return entMySQLToDays(entAddMonth(t)) }

// EnsureEntMySQLOutboxPartitions creates (idempotently) the MySQL declarative
// RANGE-on-created_time partitioned "outboxes" parent table and a monthly partition
// per month in [from, until]. It is the MySQL twin of EnsureEntOutboxPartitions: call
// it instead of ent's Schema.Create for the outbox table on the partitioned MySQL
// path (drop the plain "outboxes" table ent's Schema.Create made first).
func EnsureEntMySQLOutboxPartitions(ctx context.Context, db *sql.DB, from, until time.Time) error {
	if until.Before(from) {
		from, until = until, from
	}
	return ensureEntMySQLOutboxPartitions(ctx, db, from, until)
}

// ensureEntMySQLOutboxPartitions creates the partitioned "outboxes" parent (if
// absent) with a partition for every month in [from, until], or ADDs only the months
// not yet present (ascending) if the table already exists.
func ensureEntMySQLOutboxPartitions(ctx context.Context, db *sql.DB, from, until time.Time) error {
	exists, err := entMySQLTableExists(ctx, db, "outboxes")
	if err != nil {
		return err
	}
	if !exists {
		var defs string
		first := true
		for m := entMonthStart(from); !m.After(entMonthStart(until)); m = entAddMonth(m) {
			if !first {
				defs += ",\n"
			}
			defs += fmt.Sprintf("\tPARTITION %s VALUES LESS THAN (%d)",
				entMySQLPartitionName(m), entMySQLPartitionUpperDays(m))
			first = false
		}
		ddl := fmt.Sprintf(mysqlEntOutboxParentDDLFmt, defs)
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("create partitioned outbox parent (mysql): %w", err)
		}
		return nil
	}
	have, err := entMySQLExistingPartitions(ctx, db, "outboxes")
	if err != nil {
		return err
	}
	for m := entMonthStart(from); !m.After(entMonthStart(until)); m = entAddMonth(m) {
		name := entMySQLPartitionName(m)
		if have[name] {
			continue
		}
		ddl := fmt.Sprintf("ALTER TABLE outboxes ADD PARTITION (PARTITION %s VALUES LESS THAN (%d))",
			name, entMySQLPartitionUpperDays(m))
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("add outbox partition %s (mysql): %w", name, err)
		}
	}
	return nil
}

// dropEntMySQLPartitionsBefore drops every monthly partition of "outboxes" whose
// ENTIRE window is older than t — ALTER TABLE ... DROP PARTITION (O(1) DDL), never a
// per-row DELETE (F033 AC-2). It discovers partitions from information_schema and
// derives each window from its deterministic pYYYYMM name. MySQL forbids dropping the
// last partition, so the newest aged partition is kept if every partition is aged.
func dropEntMySQLPartitionsBefore(ctx context.Context, db *sql.DB, t time.Time) (int, error) {
	cutoff := entMonthStart(t)
	have, err := entMySQLExistingPartitions(ctx, db, "outboxes")
	if err != nil {
		return 0, err
	}
	var aged []string
	for name := range have {
		upper, ok := entMySQLPartitionUpperBound(name)
		if !ok {
			continue
		}
		if !upper.After(cutoff) {
			aged = append(aged, name)
		}
	}
	if len(aged) == 0 {
		return 0, nil
	}
	if len(aged) == len(have) {
		aged = entDropNewest(aged)
	}
	dropped := 0
	for _, name := range aged {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE outboxes DROP PARTITION %s", name)); err != nil {
			return dropped, fmt.Errorf("drop outbox partition %s (mysql): %w", name, err)
		}
		dropped++
	}
	return dropped, nil
}

// entMySQLPartitionUpperBound parses a pYYYYMM name and returns its exclusive upper
// bound (first instant of the following month). ok is false for foreign names.
func entMySQLPartitionUpperBound(name string) (time.Time, bool) {
	var year, month int
	if _, err := fmt.Sscanf(name, "p%04d%02d", &year, &month); err != nil {
		return time.Time{}, false
	}
	if month < 1 || month > 12 {
		return time.Time{}, false
	}
	start := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	return entAddMonth(start), true
}

// entDropNewest removes the lexicographically-greatest pYYYYMM name (the newest month
// for the zero-padded form) from names.
func entDropNewest(names []string) []string {
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

// entMySQLTableExists reports whether table exists in the current schema.
func entMySQLTableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check table %s exists (mysql): %w", table, err)
	}
	return n > 0, nil
}

// entMySQLExistingPartitions returns the set of named partitions of table.
func entMySQLExistingPartitions(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT partition_name FROM information_schema.partitions
		WHERE table_schema = DATABASE() AND table_name = ? AND partition_name IS NOT NULL`, table)
	if err != nil {
		return nil, fmt.Errorf("list outbox partitions (mysql): %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if serr := rows.Scan(&name); serr != nil {
			return nil, fmt.Errorf("scan partition name (mysql): %w", serr)
		}
		out[name] = true
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, rerr
	}
	return out, nil
}
