package iamv1_test

// mysql_outbox_partition_test.go — F033 Phase-2 validation of the APPEND-ONLY
// partitioned outbox + drop-partition retention on REAL MySQL (the second production
// target named in the F033 directive). Each test runs against a testcontainers
// mysql:8 server or SKIPS cleanly when Docker is unavailable (see mysqltest_test.go).
// It is the MySQL twin of postgres_outbox_partition_test.go.
//
// Why MySQL matters for F033: the headline guarantee is that retention is a
// whole-partition DROP (ALTER TABLE ... DROP PARTITION, O(1) DDL), not a per-row
// DELETE. Only a real MySQL RANGE-partitioned table can prove that — the test asserts
// the PARTITION COUNT drops (via information_schema.partitions) while current-window
// rows survive, which is the DDL-not-delete proof.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"gorm.io/gorm"

	"github.com/infobloxopen/devedge-sdk/events"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
	entiam "github.com/infobloxopen/devedge-sdk/testdata/iam/ent"
	"github.com/infobloxopen/devedge-sdk/testdata/iam/iamv1"
)

// mysqlGormPartitionCount counts the named partitions of the gormtx "outbox" table
// via information_schema — the DDL-level evidence for AC-2 (a dropped partition
// reduces this count; a per-row DELETE would not).
func mysqlGormPartitionCount(t *testing.T, db *gorm.DB) int {
	t.Helper()
	var n int
	err := db.Raw(`
		SELECT count(*) FROM information_schema.partitions
		WHERE table_schema = DATABASE() AND table_name = 'outbox' AND partition_name IS NOT NULL`).
		Scan(&n).Error
	if err != nil {
		t.Fatalf("count gorm mysql partitions: %v", err)
	}
	return n
}

// mysqlOutboxRowCount returns how many ent "outboxes" rows exist for aggregate_id —
// used to assert the write-only invariant (delivery never deletes/duplicates a row).
func mysqlOutboxRowCount(t *testing.T, db *sql.DB, aggregateID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM outboxes WHERE aggregate_id = ?`, aggregateID).Scan(&n); err != nil {
		t.Fatalf("count rows for %s: %v", aggregateID, err)
	}
	return n
}

// mysqlEntPartitionCount counts the named partitions of the ent "outboxes" table.
func mysqlEntPartitionCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	err := db.QueryRow(`
		SELECT count(*) FROM information_schema.partitions
		WHERE table_schema = DATABASE() AND table_name = 'outboxes' AND partition_name IS NOT NULL`).
		Scan(&n)
	if err != nil {
		t.Fatalf("count ent mysql partitions: %v", err)
	}
	return n
}

// openIAMEntMySQLPartitioned opens an ent client on a fresh MySQL database over a HELD
// *sql.DB (so the test can drive partition DDL retention), migrates the IAM schema,
// then REPLACES the plain ent-created "outboxes" table with a RANGE-partitioned table
// covering [from, until]. It is the MySQL twin of openIAMEntPGPartitioned.
func openIAMEntMySQLPartitioned(t *testing.T, from, until time.Time) (*entiam.Client, *sql.DB) {
	t.Helper()
	dsn := freshMySQLDatabase(t, startMySQL(t))
	rawDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open mysql: %v", err)
	}
	client := entiam.NewClient(entiam.Driver(entsql.OpenDB("mysql", rawDB)))
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("ent migrate (mysql): %v", err)
	}
	if _, err := rawDB.ExecContext(ctx, "DROP TABLE IF EXISTS outboxes"); err != nil {
		t.Fatalf("drop plain outbox (mysql): %v", err)
	}
	if err := iamv1.EnsureEntMySQLOutboxPartitions(ctx, rawDB, from, until); err != nil {
		t.Fatalf("ensure mysql partitions: %v", err)
	}
	return client, rawDB
}

// TestMySQL_F033_Gorm_DropPartitionRetention is the AC-2 proof on real MySQL via the
// gormtx store: EnsureOutboxPartitions builds the RANGE-partitioned "outbox" table;
// appends land in an old + current partition; DropPartitionsBefore drops the aged
// partition via ALTER TABLE ... DROP PARTITION (partition COUNT drops — NOT a per-row
// DELETE) and the current-window row survives. RunRetention is exercised too.
func TestMySQL_F033_Gorm_DropPartitionRetention(t *testing.T) {
	db := openIAMGormMySQL(t)
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

	partsBefore := mysqlGormPartitionCount(t, db)
	if partsBefore < 2 {
		t.Fatalf("setup must create old + current partitions, got %d", partsBefore)
	}

	appendOutboxAt(t, tx, store, "gm-old", time.Date(oldMonth.Year(), oldMonth.Month(), 15, 12, 0, 0, 0, time.UTC))
	appendOutboxAt(t, tx, store, "gm-current", time.Date(now.Year(), now.Month(), 15, 12, 0, 0, 0, time.UTC))

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

	// DDL-level proof: partition COUNT dropped (a partition was DROPped, not rows DELETEd).
	partsAfter := mysqlGormPartitionCount(t, db)
	if partsAfter != partsBefore-dropped {
		t.Fatalf("drop-partition must reduce the partition COUNT (DDL, not row-delete): before=%d after=%d dropped=%d", partsBefore, partsAfter, dropped)
	}

	// The aged row is gone with its partition; the current-window row survives.
	if n := outboxCount(t, db, "gm-old"); n != 0 {
		t.Fatalf("aged row must be gone with its partition, found %d", n)
	}
	if n := outboxCount(t, db, "gm-current"); n != 1 {
		t.Fatalf("current-window row must survive, found %d", n)
	}

	// RunRetention (the scheduler-agnostic helper) rolls the window forward (this +
	// next month) and drops nothing new (the only aged partition is already gone).
	if _, err := gormtx.RunRetention(ctx, db, store, 30*24*time.Hour); err != nil {
		t.Fatalf("RunRetention: %v", err)
	}
	// After roll-forward the current + next month partitions exist and the current row
	// still survives.
	if got := mysqlGormPartitionCount(t, db); got < 2 {
		t.Fatalf("RunRetention must keep at least current + next-month partitions, got %d", got)
	}
	if n := outboxCount(t, db, "gm-current"); n != 1 {
		t.Fatalf("current-window row must survive RunRetention, found %d", n)
	}
}

// TestMySQL_F033_Ent_DropPartitionRetention is the AC-2 proof on real MySQL via the
// ent store: appends into an OLD month's partition and the CURRENT month's partition,
// then DropPartitionsBefore removes the OLD partition via ALTER TABLE ... DROP
// PARTITION (DDL), NOT a per-row DELETE. The partition COUNT drops and the
// current-window row survives.
func TestMySQL_F033_Ent_DropPartitionRetention(t *testing.T) {
	now := time.Now().UTC()
	oldMonth := now.AddDate(0, -6, 0)
	client, rawDB := openIAMEntMySQLPartitioned(t, oldMonth, now)

	store := iamv1.NewEntOutboxStore(client, 0, iamv1.WithEntOutboxRawDB(rawDB), iamv1.WithEntOutboxMySQL())
	tx := iamv1.NewEntTxRunner(client)

	if got := mysqlEntPartitionCount(t, rawDB); got < 2 {
		t.Fatalf("setup must create at least the old + current partitions, got %d", got)
	}
	partitionsBefore := mysqlEntPartitionCount(t, rawDB)

	appendOutboxAt(t, tx, store, "em-old", time.Date(oldMonth.Year(), oldMonth.Month(), 15, 12, 0, 0, 0, time.UTC))
	appendOutboxAt(t, tx, store, "em-current", time.Date(now.Year(), now.Month(), 15, 12, 0, 0, 0, time.UTC))

	if n := entOutboxCount(t, client); n != 2 {
		t.Fatalf("two rows must be appended, found %d", n)
	}

	cutoff := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	dropped, err := store.DropPartitionsBefore(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("DropPartitionsBefore: %v", err)
	}
	if dropped < 1 {
		t.Fatalf("retention must drop the aged partition, dropped=%d", dropped)
	}

	partitionsAfter := mysqlEntPartitionCount(t, rawDB)
	if partitionsAfter != partitionsBefore-dropped {
		t.Fatalf("drop-partition must reduce the partition COUNT (DDL, not row-delete): before=%d after=%d dropped=%d",
			partitionsBefore, partitionsAfter, dropped)
	}

	if n := entOutboxCount(t, client); n != 1 {
		t.Fatalf("after dropping the old partition exactly the current-window row must survive, found %d", n)
	}
	var oldExists int
	if err := rawDB.QueryRow(`SELECT count(*) FROM outboxes WHERE id = 'em-old'`).Scan(&oldExists); err != nil {
		t.Fatalf("query old row: %v", err)
	}
	if oldExists != 0 {
		t.Fatalf("the aged row must be gone with its partition, found %d", oldExists)
	}
	var curExists int
	if err := rawDB.QueryRow(`SELECT count(*) FROM outboxes WHERE id = 'em-current'`).Scan(&curExists); err != nil {
		t.Fatalf("query current row: %v", err)
	}
	if curExists != 1 {
		t.Fatalf("the current-window row must survive the drop, found %d", curExists)
	}
}

// TestMySQL_F033_Ent_AppendOnlyDispatchDelivers is the AC-1 + AC-3 proof on the
// partitioned MySQL table via the ent store: the dispatch path NEVER deletes or
// row-marks a row (append-only — the count only grows), delivery still works
// (ClaimUndelivered(maxAttempts,limit) + dispatch + exactly-once via the idempotency
// marker), and a poison event stops after maxAttempts. Mirrors the PG test.
func TestMySQL_F033_Ent_AppendOnlyDispatchDelivers(t *testing.T) {
	now := time.Now().UTC()
	client, rawDB := openIAMEntMySQLPartitioned(t, now, now)
	ctx := tenantCtx("acme")

	users := iamv1.NewUserEntRepository(client)
	tx := iamv1.NewEntTxRunner(client)
	store := iamv1.NewEntOutboxStore(client, 0, iamv1.WithEntOutboxRawDB(rawDB), iamv1.WithEntOutboxMySQL())
	cursors := iamv1.NewEntOutboxCursorStore(client)
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
	d := events.NewDispatcher(store, cursors, tx, idem)
	d.Subscribe(eventUserSuspended, "mark-applied", func(hctx context.Context, evt events.Event) error {
		applied++
		return nil
	})

	if delivered, err := d.RunOnce(ctx, 10); err != nil || delivered != 1 {
		t.Fatalf("dispatch must deliver the event: delivered=%d err=%v", delivered, err)
	}
	if applied != 1 {
		t.Fatalf("handler must apply once, applied=%d", applied)
	}

	// AC-1 (write-only): delivery did not delete or mutate a row — count unchanged; the
	// dispatcher only advanced its sidecar cursor.
	if n := entOutboxCount(t, client); n != rowsAfterPublish {
		t.Fatalf("write-only: dispatch must never delete a row, count went %d -> %d", rowsAfterPublish, n)
	}
	if c := mysqlOutboxRowCount(t, rawDB, "u1"); c != 1 {
		t.Fatalf("write-only: the event row must be intact (exactly one), found %d", c)
	}

	// Churn-free happy path: re-running the dispatcher must NOT re-deliver the consumed
	// event (the cursor has advanced past it), so the handler is not re-invoked and the
	// outbox is never touched again.
	for i := 0; i < 3; i++ {
		delivered, err := d.RunOnce(ctx, 10)
		if err != nil {
			t.Fatalf("re-run %d: %v", i, err)
		}
		if delivered != 0 {
			t.Fatalf("re-run %d re-delivered a CONSUMED event (cursor must have advanced), delivered=%d", i, delivered)
		}
	}
	if applied != 1 {
		t.Fatalf("exactly-once: the handler must apply exactly once, applied=%d", applied)
	}
	if c := mysqlOutboxRowCount(t, rawDB, "u1"); c != 1 {
		t.Fatalf("write-only: a consumed event row must not be re-written/duplicated, found %d", c)
	}
	markers, err := client.IdemMarker.Query().Count(ctx)
	if err != nil {
		t.Fatalf("count markers: %v", err)
	}
	if markers != 1 {
		t.Fatalf("exactly one idempotency marker must commit, found %d", markers)
	}

	// AC-3 poison → dead-letter + cursor advances: the always-failing head event is
	// attempted maxAttempts times, then dead-lettered in the sidecar and skipped.
	if err := tx.Atomically(ctx, func(ctx context.Context) error {
		return pub.Publish(ctx, events.Event{ID: "poison", Type: "poison.type", AggregateType: "User", AggregateID: "u1", Payload: []byte("u1")})
	}); err != nil {
		t.Fatalf("publish poison: %v", err)
	}
	const maxAttempts = 3
	poisonCursors := iamv1.NewEntOutboxCursorStore(client)
	dp := events.NewDispatcher(store, poisonCursors, tx, iamv1.NewEntIdempotencyStore(client), events.WithMaxAttempts(maxAttempts))
	handlerCalls := 0
	dp.Subscribe("poison.type", "always-fails", func(hctx context.Context, evt events.Event) error {
		handlerCalls++
		return errPoison
	})
	for i := 0; i < maxAttempts+3; i++ {
		_, _ = dp.RunOnce(ctx, 10)
	}
	if handlerCalls != maxAttempts {
		t.Fatalf("poison: the handler must be attempted exactly maxAttempts(%d) times, got %d", maxAttempts, handlerCalls)
	}
	// The poison event was recorded in the SIDECAR dead-letter; the outbox row is intact.
	var deadLettered int
	if err := rawDB.QueryRow(`SELECT count(*) FROM outbox_dead_letters WHERE event_id = 'poison'`).Scan(&deadLettered); err != nil {
		t.Fatalf("query dead-letter: %v", err)
	}
	if deadLettered != 1 {
		t.Fatalf("the poison event must be dead-lettered in the sidecar, found %d", deadLettered)
	}
	// Write-only: the poison row is intact in the outbox (never deleted/mutated).
	var poisonRows int
	if err := rawDB.QueryRow(`SELECT count(*) FROM outboxes WHERE id = 'poison'`).Scan(&poisonRows); err != nil {
		t.Fatalf("query poison row: %v", err)
	}
	if poisonRows != 1 {
		t.Fatalf("poison row must be intact in the write-only outbox, found %d", poisonRows)
	}
}
