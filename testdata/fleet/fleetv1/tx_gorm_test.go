package fleetv1_test

// tx_gorm_test.go — F030 acceptance tests on the GORM (sqlite) backend, the
// GORM twin of ent_tx_test.go. They use ONLY gormtx.NewGormTxRunner + the
// generated GORM repositories (no raw *gorm.DB writes), proving the generated
// conn(ctx) resolver binds CRUD and batch writes to the surrounding transaction.

import (
	"context"
	"errors"
	"testing"

	_ "modernc.org/sqlite" // registers the sqlite driver; sqlite_test.go aliases it to "sqlite3"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/gormtx"
	"github.com/infobloxopen/devedge-sdk/testdata/fleet/fleetv1"
)

// openFleetGormDB opens an in-memory SQLite GORM DB and migrates the fleet models.
func openFleetGormDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:"+dsn+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&fleetv1.FleetModel{}, &fleetv1.VehicleModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// TestGormAtomically_RollbackOnError is F030 AC-1 on the GORM backend: load a
// parent Fleet, check it, write a child Vehicle — atomically — and a forced error
// mid-fn rolls back with no partial write.
func TestGormAtomically_RollbackOnError(t *testing.T) {
	db := openFleetGormDB(t, "fleet_gorm_tx_rollback")
	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetRepository(db)
	vehicles := fleetv1.NewVehicleRepository(db)
	tx := gormtx.NewGormTxRunner(db)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "fleet-1", DisplayName: "East Coast"}); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}

	wantErr := errors.New("forced mid-fn failure")
	err := tx.Atomically(ctx, func(txCtx context.Context) error {
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

	if _, gerr := vehicles.Get(ctx, "veh-1"); !errors.Is(gerr, persistence.ErrNotFound) {
		t.Fatalf("child veh-1 must be rolled back, got %v", gerr)
	}
}

// TestGormAtomically_CommitOnSuccess proves the parent+child write is committed
// when fn returns nil and the child is visible afterwards through the non-tx repo.
func TestGormAtomically_CommitOnSuccess(t *testing.T) {
	db := openFleetGormDB(t, "fleet_gorm_tx_commit")
	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetRepository(db)
	vehicles := fleetv1.NewVehicleRepository(db)
	tx := gormtx.NewGormTxRunner(db)

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

// TestGormAtomically_TxBoundReadsSeeUncommitted is the participation half of
// AC-2: a write issued through a generated repo INSIDE Atomically is visible to a
// tx-bound read in the same transaction, and discarded on rollback so a
// subsequent non-tx read sees nothing.
func TestGormAtomically_TxBoundReadsSeeUncommitted(t *testing.T) {
	db := openFleetGormDB(t, "fleet_gorm_tx_visibility")
	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetRepository(db)
	vehicles := fleetv1.NewVehicleRepository(db)
	tx := gormtx.NewGormTxRunner(db)

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

	if _, gerr := vehicles.Get(ctx, "veh-1"); !errors.Is(gerr, persistence.ErrNotFound) {
		t.Fatalf("rolled-back write must be invisible, got %v", gerr)
	}
}

// TestGormAtomically_BatchWriteRollsBack is the AIP-137 batch half of AC-2 and
// the reconciliation proof: a BatchUpdate issued through a generated repo INSIDE
// Atomically must JOIN the surrounding transaction — when fn fails, the batch
// write is rolled back with the rest of the unit. A batch op that opened its own
// gorm transaction and committed it independently would survive the outer
// rollback (the spec's worst failure mode: "looks atomic, isn't").
func TestGormAtomically_BatchWriteRollsBack(t *testing.T) {
	db := openFleetGormDB(t, "fleet_gorm_tx_batch")
	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetRepository(db)
	tx := gormtx.NewGormTxRunner(db)

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

// TestGormBatch_StandaloneAtomicity is the other half of the batch reconciliation
// contract: when a batch op is called WITHOUT a surrounding Atomically, the
// generated repo must still wrap the loop in its own db.Transaction so a mid-batch
// failure rolls the WHOLE batch back. Here the first item renames fleet-1 and the
// second targets a missing key (Update -> ErrNotFound); the first write must NOT
// survive. This guards the "no outer tx -> r.db.Transaction(...)" branch, distinct
// from the "join the ctx tx" branch proven by the *_BatchWrite* tests above.
func TestGormBatch_StandaloneAtomicity(t *testing.T) {
	db := openFleetGormDB(t, "fleet_gorm_batch_standalone")
	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetRepository(db)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "fleet-1", DisplayName: "East Coast"}); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}

	// No outer Atomically: BatchUpdate must open its own transaction.
	_, err := fleets.BatchUpdate(ctx, []persistence.BatchUpdateItem[*fleetv1.Fleet, string]{
		{Key: "fleet-1", Entity: &fleetv1.Fleet{Id: "fleet-1", DisplayName: "RENAMED"}, FieldMask: []string{"display_name"}},
		{Key: "fleet-missing", Entity: &fleetv1.Fleet{Id: "fleet-missing", DisplayName: "NOPE"}, FieldMask: []string{"display_name"}},
	})
	if err == nil {
		t.Fatalf("BatchUpdate must fail on the missing second item")
	}

	// The first item's rename must have rolled back with the standalone tx.
	got, gerr := fleets.Get(ctx, "fleet-1")
	if gerr != nil {
		t.Fatalf("fleet-1 should still exist: %v", gerr)
	}
	if got.DisplayName != "East Coast" {
		t.Fatalf("standalone batch must be all-or-nothing; first item leaked, got display_name=%q", got.DisplayName)
	}
}

// TestGormAtomically_BatchWriteCommits is the positive counterpart: a batch
// update inside a committed Atomically persists (it joined and committed with the
// outer tx).
func TestGormAtomically_BatchWriteCommits(t *testing.T) {
	db := openFleetGormDB(t, "fleet_gorm_tx_batch_commit")
	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetRepository(db)
	tx := gormtx.NewGormTxRunner(db)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "fleet-1", DisplayName: "East Coast"}); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}

	if err := tx.Atomically(ctx, func(txCtx context.Context) error {
		_, uerr := fleets.BatchUpdate(txCtx, []persistence.BatchUpdateItem[*fleetv1.Fleet, string]{
			{Key: "fleet-1", Entity: &fleetv1.Fleet{Id: "fleet-1", DisplayName: "RENAMED"}, FieldMask: []string{"display_name"}},
		})
		return uerr
	}); err != nil {
		t.Fatalf("Atomically: %v", err)
	}

	got, err := fleets.Get(ctx, "fleet-1")
	if err != nil {
		t.Fatalf("fleet-1 should exist: %v", err)
	}
	if got.DisplayName != "RENAMED" {
		t.Fatalf("committed batch update should persist, got display_name=%q", got.DisplayName)
	}
}
