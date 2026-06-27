// outbox_partition.go — the F033 PostgreSQL declarative-partitioning DDL and
// drop-partition retention for the ent-backed IAM outbox store. It mirrors
// gormtx/outbox_partition.go but drives a raw *sql.DB, because ent has no notion of
// declarative partitioning: ent's Schema.Create makes a plain table, so on PG the
// partitioned outbox table is created by this explicit DDL instead.
//
// The partitioning DDL lives in the fixture (a driver-aware place), keeping the
// persistence.OutboxStore / OutboxRetention interfaces ORM/driver-neutral.
package iamv1

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/infobloxopen/devedge-sdk/persistence"
)

// entOutboxTable is the physical table name ent generates for the Outbox schema
// (ent pluralizes: the entity "outbox" → table "outboxes"). The partitioning DDL
// must target this exact name so the generated ent client reads/writes the
// partitioned table rather than a separate one.
const entOutboxTable = "outboxes"

// outboxPGParentDDL is the PostgreSQL declarative-partitioned parent for the ent
// outbox table. created_time is in the primary key because a PG partition key must
// appear in every unique constraint. The column set matches the ent Outbox schema.
const outboxPGParentDDL = `
CREATE TABLE IF NOT EXISTS outboxes (
	id             varchar NOT NULL,
	account_id     varchar,
	aggregate_type varchar,
	aggregate_id   varchar,
	event_type     varchar,
	payload        bytea,
	created_time   timestamptz NOT NULL,
	delivered_time timestamptz,
	attempts       bigint NOT NULL DEFAULT 0,
	leased_until   timestamptz,
	PRIMARY KEY (id, created_time)
) PARTITION BY RANGE (created_time)`

func entMonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func entAddMonth(t time.Time) time.Time { return entMonthStart(t).AddDate(0, 1, 0) }

func entPartitionName(t time.Time) string {
	t = entMonthStart(t)
	return fmt.Sprintf("outbox_p%04d_%02d", t.Year(), int(t.Month()))
}

// EnsureEntOutboxPartitions creates (idempotently) the PostgreSQL declarative
// RANGE-on-created_time parent outbox table ("outboxes", ent's pluralized name) and
// one monthly partition per month in [from, until]. Call it instead of ent's
// Schema.Create for the outbox table when running the partitioned PG path (drop the
// plain "outboxes" table ent's Schema.Create made first, otherwise it is a plain,
// non-partitioned table).
func EnsureEntOutboxPartitions(ctx context.Context, db *sql.DB, from, until time.Time) error {
	if until.Before(from) {
		from, until = until, from
	}
	if _, err := db.ExecContext(ctx, outboxPGParentDDL); err != nil {
		return fmt.Errorf("create partitioned outbox parent: %w", err)
	}
	for m := entMonthStart(from); !m.After(entMonthStart(until)); m = entAddMonth(m) {
		next := entAddMonth(m)
		ddl := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
			entPartitionName(m),
			entOutboxTable,
			m.Format("2006-01-02 15:04:05-07"),
			next.Format("2006-01-02 15:04:05-07"),
		)
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("create outbox partition %s: %w", entPartitionName(m), err)
		}
	}
	return nil
}

// dropEntPGPartitionsBefore drops each monthly partition of outbox whose entire
// window is older than t — a whole-partition DROP TABLE (O(1) DDL), never a per-row
// DELETE (F033 AC-2). It discovers partitions from pg_inherits and derives each
// partition's window from its deterministic name.
func dropEntPGPartitionsBefore(ctx context.Context, db *sql.DB, t time.Time) (int, error) {
	cutoff := entMonthStart(t)
	rows, err := db.QueryContext(ctx, `
		SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'outboxes'
	`)
	if err != nil {
		return 0, fmt.Errorf("list outbox partitions: %w", err)
	}
	var children []string
	for rows.Next() {
		var name string
		if serr := rows.Scan(&name); serr != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan partition name: %w", serr)
		}
		children = append(children, name)
	}
	if cerr := rows.Err(); cerr != nil {
		_ = rows.Close()
		return 0, cerr
	}
	_ = rows.Close()

	dropped := 0
	for _, child := range children {
		upper, ok := entPartitionUpperBound(child)
		if !ok {
			continue
		}
		if !upper.After(cutoff) {
			if _, derr := db.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", child)); derr != nil {
				return dropped, fmt.Errorf("drop outbox partition %s: %w", child, derr)
			}
			dropped++
		}
	}
	return dropped, nil
}

// entPartitionUpperBound parses an outbox_pYYYY_MM partition name and returns its
// exclusive upper bound (the first instant of the following month).
func entPartitionUpperBound(name string) (time.Time, bool) {
	var year, month int
	if _, err := fmt.Sscanf(name, "outbox_p%04d_%02d", &year, &month); err != nil {
		return time.Time{}, false
	}
	if month < 1 || month > 12 {
		return time.Time{}, false
	}
	start := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	return entAddMonth(start), true
}

// compile-time check: the fixture store satisfies the retention seam.
var _ persistence.OutboxRetention = (*EntOutboxStore)(nil)
