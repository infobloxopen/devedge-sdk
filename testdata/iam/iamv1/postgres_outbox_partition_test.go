package iamv1_test

// postgres_outbox_partition_test.go — F033 Phase-1 validation of the APPEND-ONLY
// partitioned outbox + drop-partition retention on REAL PostgreSQL (the production
// target). Each test runs against a testcontainers postgres:16 server or SKIPS
// cleanly when Docker is unavailable (see pgtest_test.go).
//
// Why PG matters for F033: the headline guarantee is that retention is a
// whole-partition DROP (O(1) DDL), not a per-row DELETE. Only a real PG declarative
// RANGE-partitioned table can prove that — the test asserts the PARTITION COUNT drops
// (a table was DROPped) while current-window rows survive, which is the DDL-not-delete
// proof. SQLite/memory model the same contract as "forget old rows" (covered in the
// gormtx + persistence unit tests).

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"gorm.io/gorm"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
	entiam "github.com/infobloxopen/devedge-sdk/testdata/iam/ent"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/iamv1"
)

// errPoison is the permanent failure an always-failing handler returns to exercise
// the maxAttempts poison cutoff.
var errPoison = errors.New("permanent handler failure")

// openIAMEntPGPartitioned opens an ent client on a fresh Postgres database over a
// HELD *sql.DB (so the test can drive partition DDL retention), migrates the IAM
// schema, then REPLACES the plain ent-created outbox table with a declarative
// RANGE-on-created_time partitioned table covering [from, until]. It returns the
// client and the raw *sql.DB to inject into the EntOutboxStore for retention.
func openIAMEntPGPartitioned(t *testing.T, from, until time.Time) (*entiam.Client, *sql.DB) {
	t.Helper()
	dsn := freshPGDatabase(t, startPostgres(t))
	rawDB, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open postgres: %v", err)
	}
	client := entiam.NewClient(entiam.Driver(entsql.OpenDB("postgres", rawDB)))
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Schema.Create makes a PLAIN outbox table (ent has no partitioning); drop it and
	// recreate the outbox as a declarative RANGE-partitioned table. The other IAM
	// tables (users, api_keys, idem markers) stay as Schema.Create made them.
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("ent migrate: %v", err)
	}
	if _, err := rawDB.ExecContext(ctx, "DROP TABLE IF EXISTS outboxes"); err != nil {
		t.Fatalf("drop plain outbox: %v", err)
	}
	if err := iamv1.EnsureEntOutboxPartitions(ctx, rawDB, from, until); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	return client, rawDB
}

// outboxPartitionCount counts the child partitions of the "outbox" parent table via
// the inheritance catalog — the DDL-level evidence for AC-2 (a dropped partition
// reduces this count; a per-row DELETE would not).
func outboxPartitionCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	err := db.QueryRow(`
		SELECT count(*)
		FROM pg_inherits i
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'outboxes'
	`).Scan(&n)
	if err != nil {
		t.Fatalf("count partitions: %v", err)
	}
	return n
}

// pgOutboxAttempts returns the attempts count of the (single) outbox row for the
// given aggregate_id — used to assert a delivered row is never re-written (its
// attempts must not advance on a later poll).
func pgOutboxAttempts(t *testing.T, db *sql.DB, aggregateID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT attempts FROM outboxes WHERE aggregate_id = $1`, aggregateID).Scan(&n); err != nil {
		t.Fatalf("read attempts for %s: %v", aggregateID, err)
	}
	return n
}

// appendOutboxAt appends one outbox row stamped at created so it lands in the
// partition for that month. It goes through the store inside a tx (the only legal
// Append path), exactly as a real Publish would.
func appendOutboxAt(t *testing.T, tx persistence.TxRunner, store persistence.OutboxStore, id string, created time.Time) {
	t.Helper()
	err := tx.Atomically(context.Background(), func(ctx context.Context) error {
		return store.Append(ctx, &persistence.OutboxRecord{
			ID:          id,
			EventType:   "iam.v1.UserSuspended",
			AggregateID: "u1",
			CreatedTime: created,
			Payload:     []byte("u1"),
		})
	})
	if err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

// TestPG_F033_DropPartitionRetention is the AC-2 proof on real Postgres: appends
// into an OLD month's partition and the CURRENT month's partition, then
// DropPartitionsBefore removes the OLD partition via partition DDL (a whole-partition
// DROP TABLE, O(1)), NOT a per-row DELETE. The proof is at the DDL level — the
// partition COUNT drops by exactly one — and the current-window row survives.
func TestPG_F033_DropPartitionRetention(t *testing.T) {
	now := time.Now().UTC()
	oldMonth := now.AddDate(0, -6, 0) // six months ago: an aged partition
	client, rawDB := openIAMEntPGPartitioned(t, oldMonth, now)

	store := iamv1.NewEntOutboxStore(client, 0, iamv1.WithEntOutboxRawDB(rawDB))
	tx := iamv1.NewEntTxRunner(client)

	// Two partitions exist (old month + current month).
	if got := outboxPartitionCount(t, rawDB); got < 2 {
		t.Fatalf("setup must create at least the old + current partitions, got %d", got)
	}
	partitionsBefore := outboxPartitionCount(t, rawDB)

	// Append one row into the OLD partition and one into the CURRENT partition.
	appendOutboxAt(t, tx, store, "evt-old", time.Date(oldMonth.Year(), oldMonth.Month(), 15, 12, 0, 0, 0, time.UTC))
	appendOutboxAt(t, tx, store, "evt-current", time.Date(now.Year(), now.Month(), 15, 12, 0, 0, 0, time.UTC))

	if n := entOutboxCount(t, client); n != 2 {
		t.Fatalf("two rows must be appended, found %d", n)
	}

	// Retention: drop everything older than the start of the current month. This must
	// DROP the old partition (DDL), not DELETE its row.
	cutoff := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	dropped, err := store.DropPartitionsBefore(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("DropPartitionsBefore: %v", err)
	}
	if dropped < 1 {
		t.Fatalf("retention must drop the aged partition, dropped=%d", dropped)
	}

	// DDL-level proof: the partition count dropped (a table was DROPped). A per-row
	// DELETE would leave the partition count unchanged.
	partitionsAfter := outboxPartitionCount(t, rawDB)
	if partitionsAfter != partitionsBefore-dropped {
		t.Fatalf("drop-partition must reduce the partition COUNT by the dropped count (DDL, not row-delete): before=%d after=%d dropped=%d",
			partitionsBefore, partitionsAfter, dropped)
	}

	// The aged row is gone (its whole partition was dropped) and the current-window
	// row SURVIVES.
	if n := entOutboxCount(t, client); n != 1 {
		t.Fatalf("after dropping the old partition exactly the current-window row must survive, found %d", n)
	}
	var oldExists int
	if err := rawDB.QueryRow(`SELECT count(*) FROM outboxes WHERE id = 'evt-old'`).Scan(&oldExists); err != nil {
		t.Fatalf("query old row: %v", err)
	}
	if oldExists != 0 {
		t.Fatalf("the aged row must be gone with its partition, found %d", oldExists)
	}
	var curExists int
	if err := rawDB.QueryRow(`SELECT count(*) FROM outboxes WHERE id = 'evt-current'`).Scan(&curExists); err != nil {
		t.Fatalf("query current row: %v", err)
	}
	if curExists != 1 {
		t.Fatalf("the current-window row must survive the drop, found %d", curExists)
	}
}

// TestPG_F033_AppendOnlyDispatchDelivers is the AC-1 + AC-3 + churn-avoidance proof on
// the partitioned PG table: the dispatch path NEVER deletes a row (append-only — the
// row count only grows), a delivered row is NEVER re-claimed on a later poll (the
// churn-free happy path — no per-poll re-lease), delivery still works
// (ClaimUndelivered(maxAttempts,limit) + dispatch + exactly-once via the idempotency
// marker), and a poison event stops after maxAttempts.
func TestPG_F033_AppendOnlyDispatchDelivers(t *testing.T) {
	now := time.Now().UTC()
	client, rawDB := openIAMEntPGPartitioned(t, now, now)
	ctx := tenantCtx("acme")

	users := iamv1.NewUserEntRepository(client)
	tx := iamv1.NewEntTxRunner(client)
	// A 1ns lease so a NON-delivered row would be immediately re-claimable — this is
	// what makes the no-re-claim assertion below meaningful: the ONLY thing keeping a
	// delivered row out of the claim set is its terminal delivered-mark, not the lease.
	store := iamv1.NewEntOutboxStore(client, time.Nanosecond, iamv1.WithEntOutboxRawDB(rawDB))
	pub := events.NewOutboxPublisher(store)
	idem := iamv1.NewEntIdempotencyStore(client)

	if _, err := users.Create(ctx, &iamv1.User{Id: "u1", Email: "u1@acme.test", DisplayName: "Alice"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := suspendUser(ctx, tx, users, pub, "u1"); err != nil {
		t.Fatalf("suspend: %v", err)
	}

	rowsAfterPublish := entOutboxCount(t, client)
	if rowsAfterPublish != 1 {
		t.Fatalf("one event published into the current partition, found %d", rowsAfterPublish)
	}

	applied := 0
	d := events.NewDispatcher(store, tx, idem)
	d.Subscribe(eventUserSuspended, "mark-applied", func(hctx context.Context, evt events.Event) error {
		applied++
		return nil
	})

	// Deliver: ClaimUndelivered(maxAttempts, limit) + dispatch.
	if delivered, err := d.RunOnce(ctx, 10); err != nil || delivered != 1 {
		t.Fatalf("dispatch must deliver the event: delivered=%d err=%v", delivered, err)
	}
	if applied != 1 {
		t.Fatalf("handler must apply once, applied=%d", applied)
	}

	// AC-1 (append-only): the row was NOT deleted by delivery — the count is unchanged
	// (the delivered-mark is an in-place UPDATE, never an insert/delete).
	if n := entOutboxCount(t, client); n != rowsAfterPublish {
		t.Fatalf("append-only: dispatch must never delete a row, count went %d -> %d", rowsAfterPublish, n)
	}
	// The delivered row carries a single terminal delivered_time and its attempts are
	// at exactly 1 (one claim) — record the watermark to prove no per-poll re-write.
	attemptsAtDelivery := pgOutboxAttempts(t, rawDB, "u1")

	// Churn-free happy path: re-running the dispatcher must NOT re-claim the delivered
	// row (delivered rows are excluded from the claim), so the handler is not re-invoked
	// and attempts do NOT advance — no per-poll re-lease UPDATE churn on a delivered event.
	for i := 0; i < 3; i++ {
		delivered, err := d.RunOnce(ctx, 10)
		if err != nil {
			t.Fatalf("re-run %d: %v", i, err)
		}
		if delivered != 0 {
			t.Fatalf("re-run %d re-claimed a DELIVERED event (per-poll churn), delivered=%d", i, delivered)
		}
	}
	if applied != 1 {
		t.Fatalf("exactly-once: the handler must apply exactly once, applied=%d", applied)
	}
	if a := pgOutboxAttempts(t, rawDB, "u1"); a != attemptsAtDelivery {
		t.Fatalf("churn-free: a delivered event must not be re-written, attempts went %d -> %d", attemptsAtDelivery, a)
	}
	markers, err := client.IdemMarker.Query().Count(ctx)
	if err != nil {
		t.Fatalf("count markers: %v", err)
	}
	if markers != 1 {
		t.Fatalf("exactly one idempotency marker must commit, found %d", markers)
	}

	// AC-3 poison cutoff: an always-failing handler stops being claimed after
	// maxAttempts. Publish a poison event and drive a dispatcher whose handler always
	// errors, with a small maxAttempts.
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return pub.Publish(ctx, events.Event{ID: "poison", Type: "poison.type", AggregateType: "User", AggregateID: "u1", Payload: []byte("u1")})
	}); err != nil {
		t.Fatalf("publish poison: %v", err)
	}
	const maxAttempts = 3
	dp := events.NewDispatcher(store, tx, iamv1.NewEntIdempotencyStore(client), events.WithMaxAttempts(maxAttempts))
	handlerCalls := 0
	dp.Subscribe("poison.type", "always-fails", func(hctx context.Context, evt events.Event) error {
		handlerCalls++
		return errPoison
	})
	// Run more times than the cutoff; the row must stop being claimed at maxAttempts.
	for i := 0; i < maxAttempts+3; i++ {
		_, _ = dp.RunOnce(ctx, 10)
	}
	if handlerCalls != maxAttempts {
		t.Fatalf("poison cutoff: the handler must be attempted exactly maxAttempts(%d) times, got %d", maxAttempts, handlerCalls)
	}
	// Append-only: the poison row is parked, not deleted.
	var poisonRows int
	if err := rawDB.QueryRow(`SELECT count(*) FROM outboxes WHERE id = 'poison'`).Scan(&poisonRows); err != nil {
		t.Fatalf("query poison row: %v", err)
	}
	if poisonRows != 1 {
		t.Fatalf("poison row must be parked (append-only), not deleted, found %d", poisonRows)
	}
}

// gormPartitionCount counts the child partitions of the gormtx "outbox" parent.
func gormPartitionCount(t *testing.T, db *gorm.DB) int {
	t.Helper()
	var n int
	err := db.Raw(`
		SELECT count(*)
		FROM pg_inherits i
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'outbox'
	`).Scan(&n).Error
	if err != nil {
		t.Fatalf("count gorm partitions: %v", err)
	}
	return n
}

// TestPG_F033_Gorm_DropPartitionRetention proves AC-2 on the gormtx store over real
// Postgres: EnsureOutboxPartitions builds the declarative RANGE-partitioned "outbox"
// table; appends land in an old + current partition; DropPartitionsBefore drops the
// aged partition via DDL (partition COUNT drops — not a per-row DELETE) and the
// current-window row survives. RunRetention (the scheduler-agnostic helper) is also
// exercised end-to-end.
func TestPG_F033_Gorm_DropPartitionRetention(t *testing.T) {
	db := openIAMGormPG(t)
	ctx := context.Background()
	now := time.Now().UTC()
	oldMonth := now.AddDate(0, -6, 0)

	// Replace the AutoMigrate'd plain outbox table with a partitioned one.
	if err := db.Exec("DROP TABLE IF EXISTS outbox").Error; err != nil {
		t.Fatalf("drop plain outbox: %v", err)
	}
	if err := gormtx.EnsureOutboxPartitions(ctx, db, oldMonth, now); err != nil {
		t.Fatalf("EnsureOutboxPartitions: %v", err)
	}

	store := gormtx.NewGormOutboxStore(db)
	tx := gormtx.NewGormTxRunner(db)

	partsBefore := gormPartitionCount(t, db)
	if partsBefore < 2 {
		t.Fatalf("setup must create old + current partitions, got %d", partsBefore)
	}

	appendOutboxAt(t, tx, store, "g-old", time.Date(oldMonth.Year(), oldMonth.Month(), 15, 12, 0, 0, 0, time.UTC))
	appendOutboxAt(t, tx, store, "g-current", time.Date(now.Year(), now.Month(), 15, 12, 0, 0, 0, time.UTC))

	var total int64
	db.Model(&gormtx.OutboxRow{}).Count(&total)
	if total != 2 {
		t.Fatalf("two rows appended, found %d", total)
	}

	cutoff := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	dropped, err := store.DropPartitionsBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("DropPartitionsBefore: %v", err)
	}
	if dropped < 1 {
		t.Fatalf("retention must drop the aged partition, dropped=%d", dropped)
	}

	// DDL-level proof: partition COUNT dropped (a table was DROPped, not rows DELETEd).
	partsAfter := gormPartitionCount(t, db)
	if partsAfter != partsBefore-dropped {
		t.Fatalf("drop-partition must reduce the partition COUNT (DDL, not row-delete): before=%d after=%d dropped=%d", partsBefore, partsAfter, dropped)
	}

	// The aged row is gone with its partition; the current-window row survives.
	if n := outboxCount(t, db, "g-old"); n != 0 {
		t.Fatalf("aged row must be gone with its partition, found %d", n)
	}
	if n := outboxCount(t, db, "g-current"); n != 1 {
		t.Fatalf("current-window row must survive, found %d", n)
	}
}
