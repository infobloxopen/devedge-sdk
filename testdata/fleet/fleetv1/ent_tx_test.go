package fleetv1_test

import (
	"context"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite" // register SQLite driver for enttest

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/testdata/fleet/ent/enttest"
	"github.com/infobloxopen/devedge-sdk/testdata/fleet/fleetv1"
)

// TestEntAtomically_RollbackOnError is F030 AC-1 on the ent (sqlite) backend:
// using ONLY the TxRunner + the generated repositories (no raw ent client), a
// handler loads a parent Fleet, checks it, writes a child Vehicle — atomically —
// and a forced error mid-fn rolls back with no partial write.
func TestEntAtomically_RollbackOnError(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:fleet_tx_rollback?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetEntRepository(client)
	vehicles := fleetv1.NewVehicleEntRepository(client)
	tx := fleetv1.NewEntTxRunner(client)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "fleet-1", DisplayName: "East Coast"}); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}

	wantErr := errors.New("forced mid-fn failure")
	err := tx.Atomically(ctx, func(txCtx context.Context) error {
		// load → check → write, all through the neutral seam, all tx-bound.
		if _, gerr := fleets.Get(txCtx, "fleet-1"); gerr != nil {
			return gerr
		}
		if _, cerr := vehicles.Create(txCtx, &fleetv1.Vehicle{Id: "veh-1", Vin: "VIN-AAA", FleetId: "fleet-1"}); cerr != nil {
			return cerr
		}
		return wantErr // fail after the child write
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Atomically: want forced error, got %v", err)
	}

	// No partial write: the child must not have been committed.
	if _, gerr := vehicles.Get(ctx, "veh-1"); !errors.Is(gerr, persistence.ErrNotFound) {
		t.Fatalf("child veh-1 must be rolled back, got %v", gerr)
	}
}

// TestEntAtomically_CommitOnSuccess proves the parent+child write is committed
// when fn returns nil — and that the child is visible afterwards through the
// non-tx repository.
func TestEntAtomically_CommitOnSuccess(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:fleet_tx_commit?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetEntRepository(client)
	vehicles := fleetv1.NewVehicleEntRepository(client)
	tx := fleetv1.NewEntTxRunner(client)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "fleet-1", DisplayName: "East Coast"}); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}

	if err := tx.Atomically(ctx, func(txCtx context.Context) error {
		if _, gerr := fleets.Get(txCtx, "fleet-1"); gerr != nil {
			return gerr
		}
		_, cerr := vehicles.Create(txCtx, &fleetv1.Vehicle{Id: "veh-1", Vin: "VIN-AAA", FleetId: "fleet-1"})
		return cerr
	}); err != nil {
		t.Fatalf("Atomically: %v", err)
	}

	got, err := vehicles.Get(ctx, "veh-1")
	if err != nil {
		t.Fatalf("child veh-1 must be committed: %v", err)
	}
	if got.FleetId != "fleet-1" {
		t.Fatalf("committed child wrong: %+v", got)
	}
}

// TestEntAtomically_TxBoundReadsSeeUncommitted is the participation half of AC-2:
// a write issued through a generated repo INSIDE Atomically is visible to a
// tx-bound read in the same transaction (it participates in the tx), and is
// discarded on rollback so a subsequent non-tx read sees nothing — i.e. the write
// never escaped the transaction.
func TestEntAtomically_TxBoundReadsSeeUncommitted(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:fleet_tx_visibility?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetEntRepository(client)
	vehicles := fleetv1.NewVehicleEntRepository(client)
	tx := fleetv1.NewEntTxRunner(client)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "fleet-1", DisplayName: "East Coast"}); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}

	boom := errors.New("rollback")
	err := tx.Atomically(ctx, func(txCtx context.Context) error {
		if _, cerr := vehicles.Create(txCtx, &fleetv1.Vehicle{Id: "veh-1", Vin: "VIN-AAA", FleetId: "fleet-1"}); cerr != nil {
			return cerr
		}
		// Inside the tx, the tx-bound repo sees its own uncommitted write.
		if _, gerr := vehicles.Get(txCtx, "veh-1"); gerr != nil {
			return errors.New("tx-bound read should see the uncommitted write: " + gerr.Error())
		}
		return boom // discard everything
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Atomically: want rollback error, got %v", err)
	}

	// After rollback the write is gone — it never escaped the transaction.
	if _, gerr := vehicles.Get(ctx, "veh-1"); !errors.Is(gerr, persistence.ErrNotFound) {
		t.Fatalf("rolled-back write must be invisible, got %v", gerr)
	}
}

// TestEntAtomically_BatchWriteRollsBack is the AIP-137 batch half of AC-2: a
// BatchUpdate / BatchDelete issued through a generated repo INSIDE Atomically must
// participate in the surrounding transaction — when fn fails, the batch write is
// rolled back with the rest of the unit. This guards the spec's worst failure mode
// ("Tx not propagated — looks atomic, isn't"): a batch op that opened its own
// ent transaction and committed it independently would survive the outer rollback.
func TestEntAtomically_BatchWriteRollsBack(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:fleet_tx_batch?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetEntBatchRepository(client)
	tx := fleetv1.NewEntTxRunner(client)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "fleet-1", DisplayName: "East Coast"}); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}

	boom := errors.New("rollback")
	err := tx.Atomically(ctx, func(txCtx context.Context) error {
		// A batch update issued inside the tx must be part of the tx.
		if _, uerr := fleets.BatchUpdate(txCtx, []persistence.BatchUpdateItem[*fleetv1.Fleet, string]{
			{Key: "fleet-1", Entity: &fleetv1.Fleet{Id: "fleet-1", DisplayName: "RENAMED"}, FieldMask: []string{"display_name"}},
		}); uerr != nil {
			return uerr
		}
		return boom // discard the batch write with the unit
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Atomically: want rollback error, got %v", err)
	}

	// The batch update must have been rolled back: the name is still the original.
	got, gerr := fleets.Get(ctx, "fleet-1")
	if gerr != nil {
		t.Fatalf("fleet-1 should still exist: %v", gerr)
	}
	if got.DisplayName != "East Coast" {
		t.Fatalf("batch update must be rolled back with the outer tx, got display_name=%q", got.DisplayName)
	}
}

// TestEntAtomically_InvisibleToConcurrentReader is the concurrent-reader half of
// AC-2 on ent: while the transaction is open, a read of the in-flight row issued
// on a fresh (non-tx) repository must not observe it; once the tx commits, the row
// becomes visible. SQLite serializes the write transaction, so the concurrent read
// either blocks until commit or returns not-found until then — in both cases it
// must never return the uncommitted row.
func TestEntAtomically_InvisibleToConcurrentReader(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:fleet_tx_concurrent?mode=memory&cache=shared&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetEntRepository(client)
	vehicles := fleetv1.NewVehicleEntRepository(client)
	tx := fleetv1.NewEntTxRunner(client)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "fleet-1", DisplayName: "East Coast"}); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}

	inTx := make(chan struct{})
	release := make(chan struct{})
	txDone := make(chan error, 1)
	go func() {
		txDone <- tx.Atomically(ctx, func(txCtx context.Context) error {
			if _, cerr := vehicles.Create(txCtx, &fleetv1.Vehicle{Id: "veh-1", Vin: "VIN-AAA", FleetId: "fleet-1"}); cerr != nil {
				return cerr
			}
			close(inTx)
			<-release // hold the tx open while the reader probes
			return nil
		})
	}()

	<-inTx
	// Probe with the non-tx repo. Run it in a goroutine: SQLite may block the read
	// until the writer commits; a returned value before release would be the bug.
	readResult := make(chan error, 1)
	go func() {
		_, err := vehicles.Get(ctx, "veh-1")
		readResult <- err
	}()

	select {
	case err := <-readResult:
		if err == nil {
			t.Fatal("concurrent reader saw the uncommitted write before commit")
		}
		// not-found / locked before commit is acceptable (the row is not visible).
	case <-time.After(100 * time.Millisecond):
		// Blocked on the write lock — also acceptable; it is not visible.
	}

	close(release)
	if err := <-txDone; err != nil {
		t.Fatalf("Atomically: %v", err)
	}

	// After commit, the row is visible.
	if _, err := vehicles.Get(ctx, "veh-1"); err != nil {
		t.Fatalf("after commit the reader must see the committed row: %v", err)
	}
}
