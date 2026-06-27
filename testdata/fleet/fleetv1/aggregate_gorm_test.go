package fleetv1_test

// aggregate_gorm_test.go — F031 acceptance tests on the GORM (sqlite) backend,
// the GORM twin of aggregate_test.go. They wire the SDK's backend-neutral
// persistence.GenericAggregateRepository over the GENERATED GORM fleet
// repositories: Load uses the generated graph-load primitive
// LoadFleetAggregateGorm (D-2), Save tracks Vehicle (member) mutations against
// the stored cluster (D-3) and the SDK bumps the Fleet (root) etag once on any
// member change (D-5). This proves the generic aggregate machinery + the
// generated LoadFleetAggregateGorm + the DB-level cascade tag all work on GORM,
// with no aggregate-specific runtime of its own.

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

// openFleetGormDBFK opens an in-memory SQLite GORM DB with FOREIGN KEY
// enforcement ON and a single pinned connection, then migrates the fleet models.
// SQLite enforces foreign keys (and thus OnDelete: CASCADE) only when the
// `foreign_keys` pragma is set PER CONNECTION; the default helper omits it, so
// the cascade test needs this variant. We set the pragma in the modernc DSN
// (_pragma=foreign_keys(1)) AND cap the pool to one connection (with cache=shared)
// so the same FK-enabled connection that ran AutoMigrate also runs the cascade
// DELETE — otherwise a fresh, FK-off connection from the pool would silently skip
// the cascade.
func openFleetGormDBFK(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		openTestSQLite("file:"+dsn+"?mode=memory&cache=shared&_pragma=foreign_keys(1)"),
		&gorm.Config{Logger: logger.Discard},
	)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1) // pin one FK-enabled connection
	// Belt-and-suspenders: also enable it explicitly on the pinned connection.
	if err := db.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
		t.Fatalf("enable foreign_keys: %v", err)
	}
	if err := db.AutoMigrate(&fleetv1.FleetModel{}, &fleetv1.VehicleModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

// gormFleetAggregate wires the SDK's backend-neutral persistence.AggregateSpec
// over the generated GORM fleet repositories — the GORM twin of fleetAggregate
// in aggregate_test.go. Load uses the generated LoadFleetAggregateGorm; Save
// diffs the incoming members against the stored cluster and applies
// adds/updates/deletes through the generated VehicleRepository, all inside the
// GormTxRunner transaction.
func gormFleetAggregate(db *gorm.DB) *persistence.GenericAggregateRepository[*fleetv1.Fleet, string] {
	fleets := fleetv1.NewFleetRepository(db)
	vehicles := fleetv1.NewVehicleRepository(db)
	return persistence.NewGenericAggregateRepository(persistence.AggregateSpec[*fleetv1.Fleet, string]{
		Tx:       gormtx.NewGormTxRunner(db),
		RootRepo: fleets,
		KeyOf:    func(f *fleetv1.Fleet) string { return f.GetId() },
		EtagOf:   func(f *fleetv1.Fleet) string { return f.GetEtag() },
		LoadMembers: func(ctx context.Context, root *fleetv1.Fleet) (*fleetv1.Fleet, error) {
			return fleetv1.LoadFleetAggregateGorm(ctx, db, root.GetId())
		},
		SaveMembers: func(ctx context.Context, root *fleetv1.Fleet) (bool, error) {
			// Member-mutation tracking: diff the incoming members against what is
			// stored (loaded fresh inside the tx) and apply adds/updates/removes.
			stored, err := fleetv1.LoadFleetAggregateGorm(ctx, db, root.GetId())
			if err != nil {
				return false, err
			}
			storedByID := map[string]*fleetv1.Vehicle{}
			for _, v := range stored.GetVehicles() {
				storedByID[v.GetId()] = v
			}
			changed := false
			wantIDs := map[string]struct{}{}
			for _, v := range root.GetVehicles() {
				wantIDs[v.GetId()] = struct{}{}
				v.FleetId = root.GetId()
				if v.AccountId == "" {
					v.AccountId = root.GetAccountId()
				}
				if cur, ok := storedByID[v.GetId()]; !ok {
					if _, cerr := vehicles.Create(ctx, v); cerr != nil {
						return false, cerr
					}
					changed = true
				} else if cur.GetVin() != v.GetVin() {
					if _, uerr := vehicles.Update(ctx, v.GetId(), v); uerr != nil {
						return false, uerr
					}
					changed = true
				}
			}
			for id := range storedByID {
				if _, keep := wantIDs[id]; !keep {
					if derr := vehicles.Delete(ctx, id); derr != nil {
						return false, derr
					}
					changed = true
				}
			}
			return changed, nil
		},
	})
}

// TestGormAggregate_RoundTripAndStaleEtag is F031 AC-3 on GORM: Load returns the
// root with its members; a member mutation through Save persists the cluster in
// one tx and bumps the root etag; a stale root etag fails with
// ErrPreconditionFailed.
func TestGormAggregate_RoundTripAndStaleEtag(t *testing.T) {
	db := openFleetGormDBFK(t, "fleet_gorm_agg_roundtrip")
	ctx := tenantCtx("acme")
	agg := gormFleetAggregate(db)
	fleets := fleetv1.NewFleetRepository(db)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "f1", AccountId: "acme", DisplayName: "East"}); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}

	// Load the (empty) cluster, then add a member and Save.
	root, err := agg.Load(ctx, "f1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(root.GetVehicles()) != 0 {
		t.Fatalf("new fleet should have no vehicles, got %d", len(root.GetVehicles()))
	}
	etagBefore := root.GetEtag()
	root.Vehicles = append(root.Vehicles, &fleetv1.Vehicle{Id: "v1", AccountId: "acme", Vin: "VIN-1"})
	saved, err := agg.Save(ctx, root)
	if err != nil {
		t.Fatalf("Save (add member): %v", err)
	}
	if saved.GetEtag() == etagBefore || saved.GetEtag() == "" {
		t.Fatalf("root etag must be bumped on a member change: before=%q after=%q", etagBefore, saved.GetEtag())
	}

	// Re-load: the member is now part of the cluster.
	reloaded, err := agg.Load(ctx, "f1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.GetVehicles()) != 1 || reloaded.GetVehicles()[0].GetId() != "v1" {
		t.Fatalf("cluster must contain v1, got %+v", reloaded.GetVehicles())
	}

	// Stale root etag: a Save carrying the OLD aggregate version must fail with
	// ErrPreconditionFailed — another writer has since bumped the cluster
	// (etag-as-aggregate-version concurrency token).
	reloaded.Etag = etagBefore // pretend the caller still holds the pre-Save version
	reloaded.Vehicles = append(reloaded.Vehicles, &fleetv1.Vehicle{Id: "v2", AccountId: "acme", Vin: "VIN-2"})
	if _, err := agg.Save(ctx, reloaded); !errors.Is(err, persistence.ErrPreconditionFailed) {
		t.Fatalf("stale root etag must yield ErrPreconditionFailed, got %v", err)
	}
}

// TestGormAggregate_CascadeOnRootDelete is F031 AC-5 on GORM/sqlite: deleting an
// aggregate root cascades to its owned members (the Fleet→Vehicle FK is
// OnDelete: CASCADE). We hard-delete the root row through the raw GORM client
// (Unscoped, since Repository.Delete is soft-delete) to exercise the DB-level
// cascade — which requires the FK pragma the helper enables.
func TestGormAggregate_CascadeOnRootDelete(t *testing.T) {
	db := openFleetGormDBFK(t, "fleet_gorm_agg_cascade")
	ctx := tenantCtx("acme")
	agg := gormFleetAggregate(db)
	fleets := fleetv1.NewFleetRepository(db)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "f1", AccountId: "acme", DisplayName: "East"}); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}
	root, err := agg.Load(ctx, "f1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	root.Vehicles = append(root.Vehicles,
		&fleetv1.Vehicle{Id: "v1", AccountId: "acme", Vin: "VIN-1"},
		&fleetv1.Vehicle{Id: "v2", AccountId: "acme", Vin: "VIN-2"},
	)
	if _, err := agg.Save(ctx, root); err != nil {
		t.Fatalf("Save members: %v", err)
	}

	// Sanity: the members exist before the delete.
	var before int64
	if err := db.Model(&fleetv1.VehicleModel{}).Where("fleet_id = ?", "f1").Count(&before).Error; err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 2 {
		t.Fatalf("expected 2 vehicles before cascade, got %d", before)
	}

	// Hard-delete the root row at the DB level (Unscoped → real DELETE, not
	// soft-delete); the FK cascade removes its members.
	if err := db.Unscoped().Where("id = ?", "f1").Delete(&fleetv1.FleetModel{}).Error; err != nil {
		t.Fatalf("hard-delete root: %v", err)
	}

	var remaining int64
	if err := db.Unscoped().Model(&fleetv1.VehicleModel{}).Count(&remaining).Error; err != nil {
		t.Fatalf("count vehicles: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("deleting the root must cascade to its members; %d vehicle(s) survived", remaining)
	}
}

// errGormShipped is the AC-6 invariant violation.
var errGormShipped = errors.New("cannot add a vehicle to a SHIPPED fleet")

// gormValidatingFleet is a Fleet whose Validate(ctx) invariant rejects any member
// change once shipped. It satisfies persistence.AggregateValidator, so Save calls
// Validate before persisting (D-7).
type gormValidatingFleet struct {
	*fleetv1.Fleet
	shipped bool
}

func (f gormValidatingFleet) Validate(_ context.Context) error {
	if f.shipped && len(f.GetVehicles()) > 0 {
		return errGormShipped
	}
	return nil
}

// TestGormAggregate_ValidateRejectsSave is F031 AC-6 on GORM: a root whose
// Validate(ctx) returns an error is rejected by Save before any persist; a
// non-violating root passes. We drive the seam end-to-end (agg.Save) for the
// rejecting case to prove Save honours the validator hook, and assert nothing was
// written.
func TestGormAggregate_ValidateRejectsSave(t *testing.T) {
	db := openFleetGormDBFK(t, "fleet_gorm_agg_validate")
	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetRepository(db)
	vehicles := fleetv1.NewVehicleRepository(db)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "f1", AccountId: "acme", DisplayName: "East"}); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}

	// Build an aggregate repo over a validating root wrapper.
	agg := persistence.NewGenericAggregateRepository(persistence.AggregateSpec[gormValidatingFleet, string]{
		Tx:       gormtx.NewGormTxRunner(db),
		RootRepo: validatingRootRepo{inner: fleets},
		KeyOf:    func(f gormValidatingFleet) string { return f.GetId() },
		EtagOf:   func(f gormValidatingFleet) string { return f.GetEtag() },
		LoadMembers: func(ctx context.Context, root gormValidatingFleet) (gormValidatingFleet, error) {
			loaded, err := fleetv1.LoadFleetAggregateGorm(ctx, db, root.GetId())
			if err != nil {
				return gormValidatingFleet{}, err
			}
			return gormValidatingFleet{Fleet: loaded, shipped: root.shipped}, nil
		},
		SaveMembers: func(ctx context.Context, root gormValidatingFleet) (bool, error) {
			for _, v := range root.GetVehicles() {
				v.FleetId = root.GetId()
				if v.AccountId == "" {
					v.AccountId = root.GetAccountId()
				}
				if _, cerr := vehicles.Create(ctx, v); cerr != nil {
					return false, cerr
				}
			}
			return len(root.GetVehicles()) > 0, nil
		},
	})

	// Violating case: a SHIPPED fleet with a member must be rejected, and persist
	// nothing (the validator runs before the transaction).
	bad := gormValidatingFleet{
		Fleet:   &fleetv1.Fleet{Id: "f1", AccountId: "acme", DisplayName: "East", Vehicles: []*fleetv1.Vehicle{{Id: "v1", AccountId: "acme", Vin: "VIN-X"}}},
		shipped: true,
	}
	if _, err := agg.Save(ctx, bad); !errors.Is(err, errGormShipped) {
		t.Fatalf("SHIPPED fleet with a member must fail Save with the invariant error, got %v", err)
	}
	if _, gerr := vehicles.Get(ctx, "v1"); !errors.Is(gerr, persistence.ErrNotFound) {
		t.Fatalf("a rejected Save must not persist the member, got %v", gerr)
	}

	// Passing case: not shipped → Save persists the member.
	ok := gormValidatingFleet{
		Fleet:   &fleetv1.Fleet{Id: "f1", AccountId: "acme", DisplayName: "East", Vehicles: []*fleetv1.Vehicle{{Id: "v2", AccountId: "acme", Vin: "VIN-Y"}}},
		shipped: false,
	}
	// Re-fetch the current etag so the precondition passes.
	cur, err := fleets.Get(ctx, "f1")
	if err != nil {
		t.Fatalf("get current fleet: %v", err)
	}
	ok.Etag = cur.GetEtag()
	if _, err := agg.Save(ctx, ok); err != nil {
		t.Fatalf("non-shipped fleet must pass Save, got %v", err)
	}
	if _, gerr := vehicles.Get(ctx, "v2"); gerr != nil {
		t.Fatalf("a passing Save must persist the member, got %v", gerr)
	}
}

// validatingRootRepo adapts the generated *FleetRepository to the
// Repository[gormValidatingFleet, string] the validating aggregate spec needs. It
// wraps/unwraps the gormValidatingFleet around the underlying *fleetv1.Fleet.
type validatingRootRepo struct {
	inner *fleetv1.FleetRepository
}

func (r validatingRootRepo) Get(ctx context.Context, key string) (gormValidatingFleet, error) {
	f, err := r.inner.Get(ctx, key)
	if err != nil {
		return gormValidatingFleet{}, err
	}
	return gormValidatingFleet{Fleet: f}, nil
}

func (r validatingRootRepo) List(ctx context.Context, opts persistence.ListOptions) ([]gormValidatingFleet, string, error) {
	rows, tok, err := r.inner.List(ctx, opts)
	if err != nil {
		return nil, "", err
	}
	out := make([]gormValidatingFleet, len(rows))
	for i, f := range rows {
		out[i] = gormValidatingFleet{Fleet: f}
	}
	return out, tok, nil
}

func (r validatingRootRepo) Create(ctx context.Context, e gormValidatingFleet) (gormValidatingFleet, error) {
	f, err := r.inner.Create(ctx, e.Fleet)
	if err != nil {
		return gormValidatingFleet{}, err
	}
	return gormValidatingFleet{Fleet: f, shipped: e.shipped}, nil
}

func (r validatingRootRepo) Update(ctx context.Context, key string, e gormValidatingFleet, mask ...string) (gormValidatingFleet, error) {
	f, err := r.inner.Update(ctx, key, e.Fleet, mask...)
	if err != nil {
		return gormValidatingFleet{}, err
	}
	return gormValidatingFleet{Fleet: f, shipped: e.shipped}, nil
}

func (r validatingRootRepo) Delete(ctx context.Context, key string) error {
	return r.inner.Delete(ctx, key)
}

func (r validatingRootRepo) Undelete(ctx context.Context, key string) (gormValidatingFleet, error) {
	f, err := r.inner.Undelete(ctx, key)
	if err != nil {
		return gormValidatingFleet{}, err
	}
	return gormValidatingFleet{Fleet: f}, nil
}
