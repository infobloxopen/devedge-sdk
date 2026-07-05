package fleetv1_test

// getby_sqlite_test.go — #173: the generated GetBy<UniqueField> natural-key lookup.
// Vehicle.vin is a plain per-tenant unique field, so the GORM repository exposes
// GetByVin(ctx, value). This proves it resolves by the unique value AND stays
// tenant-scoped (a cross-tenant lookup returns ErrNotFound, never another tenant's
// row).

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	fleetv1 "github.com/infobloxopen/devedge-sdk/testdata/fleet/fleetv1"
)

func TestGetByVin_GORM(t *testing.T) {
	db, err := gorm.Open(openTestSQLite("file:getbyvin?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&fleetv1.FleetModel{}, &fleetv1.VehicleModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	vehicles := fleetv1.NewVehicleRepository(db)

	ctxT1 := middleware.WithTenantID(context.Background(), "t1")
	if _, err := vehicles.Create(ctxT1, &fleetv1.Vehicle{Id: "v1", Vin: "VIN-1", FleetId: "f1"}); err != nil {
		t.Fatalf("create vehicle: %v", err)
	}

	// GetByVin resolves the natural key within the owning tenant.
	got, err := vehicles.GetByVin(ctxT1, "VIN-1")
	if err != nil {
		t.Fatalf("GetByVin(t1, VIN-1): %v", err)
	}
	if got.GetId() != "v1" {
		t.Errorf("GetByVin returned id %q, want v1", got.GetId())
	}

	// Tenant-scoped: the same VIN is not visible from another tenant.
	ctxT2 := middleware.WithTenantID(context.Background(), "t2")
	if _, err := vehicles.GetByVin(ctxT2, "VIN-1"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("cross-tenant GetByVin = %v, want ErrNotFound (must not leak another tenant's row)", err)
	}

	// Unknown value and empty value both yield ErrNotFound.
	if _, err := vehicles.GetByVin(ctxT1, "nope"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("GetByVin(unknown) = %v, want ErrNotFound", err)
	}
	if _, err := vehicles.GetByVin(ctxT1, ""); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("GetByVin(empty) = %v, want ErrNotFound", err)
	}
}
