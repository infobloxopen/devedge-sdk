package gormtx_test

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
)

// openSeqDB opens a shared-cache in-memory SQLite db with the outbox + event-seq
// allocator tables migrated.
func openSeqDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:"+dsn+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&gormtx.OutboxRow{}, &gormtx.TenantEventSeqRow{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// appendWithRetry runs one Append inside its own Atomically, retrying on a transient
// SQLite busy/locked error (the in-memory shared-cache serializes writers and may
// return SQLITE_BUSY under contention).
func appendWithRetry(ctx context.Context, tx *gormtx.GormTxRunner, store *gormtx.GormOutboxStore, rec *persistence.OutboxRecord) error {
	var lastErr error
	for i := 0; i < 50; i++ {
		err := tx.Atomically(ctx, func(c context.Context) error {
			return store.Append(c, rec)
		})
		if err == nil {
			return nil
		}
		if isBusy(err) {
			lastErr = err
			continue
		}
		return err
	}
	return lastErr
}

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "locked") || strings.Contains(m, "busy")
}

// TestEventSeq_Concurrent_StrictlyIncreasingGapFree proves the per-tenant event_seq
// allocator: concurrent Appends for the SAME tenant get a strictly-increasing,
// gap-free sequence {1..N}; a different tenant gets its own independent {1..M}.
func TestEventSeq_Concurrent_StrictlyIncreasingGapFree(t *testing.T) {
	db := openSeqDB(t, "eventseq_concurrent")
	store := gormtx.NewGormOutboxStore(db)
	tx := gormtx.NewGormTxRunner(db)
	ctx := context.Background()

	const nA, nB = 25, 10
	var wg sync.WaitGroup
	errs := make(chan error, nA+nB)
	emit := func(account, idPrefix string, n int) {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				rec := &persistence.OutboxRecord{ID: idPrefix + string(rune('A'+i%26)) + string(rune('0'+i/26)), AccountID: account, EventType: "X", AggregateID: "agg"}
				if err := appendWithRetry(ctx, tx, store, rec); err != nil {
					errs <- err
				}
			}(i)
		}
	}
	emit("acct-A", "a", nA)
	emit("acct-B", "b", nB)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}

	assertGapFree := func(account string, want int) {
		var seqs []int64
		if err := db.WithContext(ctx).Model(&gormtx.OutboxRow{}).
			Where("account_id = ?", account).
			Order("event_seq ASC").
			Pluck("event_seq", &seqs).Error; err != nil {
			t.Fatalf("pluck seqs for %s: %v", account, err)
		}
		if len(seqs) != want {
			t.Fatalf("%s: expected %d rows, got %d", account, want, len(seqs))
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		for i, s := range seqs {
			if s != int64(i+1) {
				t.Fatalf("%s: event_seq must be gap-free {1..%d}; at index %d got %d (full=%v)", account, want, i, s, seqs)
			}
		}
	}
	assertGapFree("acct-A", nA)
	assertGapFree("acct-B", nB)
}

// TestEventSeq_StampsEpochFromToken proves Append stamps event_epoch from the ctx
// admission token's route epoch when the record's epoch is 0, and leaves it 0 with no
// token.
func TestEventSeq_StampsEpochFromToken(t *testing.T) {
	db := openSeqDB(t, "eventseq_epoch")
	store := gormtx.NewGormOutboxStore(db)
	tx := gormtx.NewGormTxRunner(db)

	// No token → event_epoch stays 0.
	if err := tx.Atomically(context.Background(), func(c context.Context) error {
		return store.Append(c, &persistence.OutboxRecord{ID: "no-tok", AccountID: "acme", EventType: "X"})
	}); err != nil {
		t.Fatalf("append no-token: %v", err)
	}

	// Admitted on cell-a@7 → event_epoch stamped 7.
	tokCtx := admittedContext(t, "cell-a", "acme", 7)
	if err := tx.Atomically(tokCtx, func(c context.Context) error {
		return store.Append(c, &persistence.OutboxRecord{ID: "tok", AccountID: "acme", EventType: "X"})
	}); err != nil {
		t.Fatalf("append with token: %v", err)
	}

	var noTok, withTok gormtx.OutboxRow
	db.WithContext(context.Background()).Model(&gormtx.OutboxRow{}).Where("id = ?", "no-tok").Take(&noTok)
	db.WithContext(context.Background()).Model(&gormtx.OutboxRow{}).Where("id = ?", "tok").Take(&withTok)
	if noTok.EventEpoch != 0 {
		t.Errorf("no-token event must have event_epoch 0, got %d", noTok.EventEpoch)
	}
	if withTok.EventEpoch != 7 {
		t.Errorf("admitted event must carry event_epoch 7 from the token, got %d", withTok.EventEpoch)
	}
	// Both got a per-tenant seq.
	if noTok.EventSeq == 0 || withTok.EventSeq == 0 || noTok.EventSeq == withTok.EventSeq {
		t.Errorf("each event must get a distinct non-zero per-tenant seq, got %d and %d", noTok.EventSeq, withTok.EventSeq)
	}
}

// TestEventSeq_ExplicitValuesPreserved proves Append does NOT overwrite a caller's
// explicit EventSeq/EventEpoch (only allocates/stamps when they are 0).
func TestEventSeq_ExplicitValuesPreserved(t *testing.T) {
	db := openSeqDB(t, "eventseq_explicit")
	store := gormtx.NewGormOutboxStore(db)
	tx := gormtx.NewGormTxRunner(db)
	if err := tx.Atomically(context.Background(), func(c context.Context) error {
		return store.Append(c, &persistence.OutboxRecord{ID: "x", AccountID: "acme", EventType: "X", EventSeq: 99, EventEpoch: 42})
	}); err != nil {
		t.Fatalf("append explicit: %v", err)
	}
	var row gormtx.OutboxRow
	db.WithContext(context.Background()).Model(&gormtx.OutboxRow{}).Where("id = ?", "x").Take(&row)
	if row.EventSeq != 99 || row.EventEpoch != 42 {
		t.Fatalf("explicit seq/epoch must be preserved, got seq=%d epoch=%d", row.EventSeq, row.EventEpoch)
	}
}
