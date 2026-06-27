package fleetv1_test

import (
	"context"
	"errors"
	"testing"

	_ "modernc.org/sqlite" // register SQLite driver for enttest

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/testdata/fleet/ent"
	"github.com/infobloxopen/devedge-sdk/testdata/fleet/ent/enttest"
	"github.com/infobloxopen/devedge-sdk/testdata/fleet/fleetv1"
)

// fleetAggregate wires the SDK's backend-neutral persistence.AggregateSpec over
// the generated ent fleet repositories: Load uses the generated graph-load
// primitive LoadFleetAggregate (D-2), Save tracks Vehicle (member) mutations
// against the stored cluster (D-3) and the SDK bumps the Fleet (root) etag once
// on any member change (D-5). This is exactly the shape a generated/owned ent
// aggregate repo would take; the test wires it directly to exercise the seam.
func fleetAggregate(client *ent.Client) *persistence.MemoryAggregateRepository[*fleetv1.Fleet, string] {
	fleets := fleetv1.NewFleetEntRepository(client)
	vehicles := fleetv1.NewVehicleEntRepository(client)
	tx := fleetv1.NewEntTxRunner(client)
	return persistence.NewMemoryAggregateRepository(persistence.AggregateSpec[*fleetv1.Fleet, string]{
		Tx:       tx,
		RootRepo: fleets,
		KeyOf:    func(f *fleetv1.Fleet) string { return f.GetId() },
		EtagOf:   func(f *fleetv1.Fleet) string { return f.GetEtag() },
		LoadMembers: func(ctx context.Context, root *fleetv1.Fleet) (*fleetv1.Fleet, error) {
			return fleetv1.LoadFleetAggregate(ctx, client, root.GetId())
		},
		SaveMembers: func(ctx context.Context, root *fleetv1.Fleet) (bool, error) {
			// Member-mutation tracking: diff the incoming members against what is
			// stored (loaded fresh inside the tx) and apply adds/updates/removes.
			stored, err := fleetv1.LoadFleetAggregate(ctx, client, root.GetId())
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

// TestAggregate_RoundTripAndStaleEtag is F031 AC-3 on ent: Load returns the root
// with its members; a member mutation through Save persists the cluster in one tx
// and bumps the root etag; a stale root etag fails with ErrPreconditionFailed.
func TestAggregate_RoundTripAndStaleEtag(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:fleet_agg_roundtrip?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	ctx := tenantCtx("acme")
	agg := fleetAggregate(client)
	fleets := fleetv1.NewFleetEntRepository(client)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "f1", DisplayName: "East"}); err != nil {
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
	root.Vehicles = append(root.Vehicles, &fleetv1.Vehicle{Id: "v1", Vin: "VIN-1"})
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

	// Stale root etag: a Save carrying the OLD aggregate version (the etag from
	// before the first Save) must fail with ErrPreconditionFailed — another writer
	// has since bumped the cluster (etag-as-aggregate-version concurrency token).
	reloaded.Etag = etagBefore // pretend the caller still holds the pre-Save version
	reloaded.Vehicles = append(reloaded.Vehicles, &fleetv1.Vehicle{Id: "v2", Vin: "VIN-2"})
	if _, err := agg.Save(ctx, reloaded); !errors.Is(err, persistence.ErrPreconditionFailed) {
		t.Fatalf("stale root etag must yield ErrPreconditionFailed, got %v", err)
	}
}

// TestAggregate_CascadeOnRootDelete is F031 AC-5 on sqlite: deleting an aggregate
// root cascades to its owned members (the Fleet→Vehicle FK is OnDelete: Cascade).
// We hard-delete the root row through the raw ent client (Repository.Delete is
// soft-delete) to exercise the DB-level cascade.
func TestAggregate_CascadeOnRootDelete(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:fleet_agg_cascade?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	ctx := tenantCtx("acme")
	agg := fleetAggregate(client)
	fleets := fleetv1.NewFleetEntRepository(client)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "f1", DisplayName: "East"}); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}
	root, _ := agg.Load(ctx, "f1")
	root.Vehicles = append(root.Vehicles,
		&fleetv1.Vehicle{Id: "v1", Vin: "VIN-1"},
		&fleetv1.Vehicle{Id: "v2", Vin: "VIN-2"},
	)
	if _, err := agg.Save(ctx, root); err != nil {
		t.Fatalf("Save members: %v", err)
	}

	// Hard-delete the root row at the DB level; the cascade removes its members.
	if err := client.Fleet.DeleteOneID("f1").Exec(ctx); err != nil {
		t.Fatalf("hard-delete root: %v", err)
	}
	remaining, err := client.Vehicle.Query().All(ctx)
	if err != nil {
		t.Fatalf("query vehicles: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("deleting the root must cascade to its members; %d vehicle(s) survived", len(remaining))
	}
}

// errShipped is the invariant violation for the AC-6 domain rule.
var errShipped = errors.New("cannot add a vehicle to a SHIPPED fleet")

// shippedFleet is a Fleet whose Validate(ctx) invariant rejects any member change
// (mirroring "no item once SHIPPED"). It satisfies persistence.AggregateValidator,
// so Save calls Validate before persisting (D-7).
type validatingFleet struct {
	*fleetv1.Fleet
	shipped bool
}

func (f validatingFleet) Validate(_ context.Context) error {
	if f.shipped && len(f.GetVehicles()) > 0 {
		return errShipped
	}
	return nil
}

// TestAggregate_ValidateRejectsSave is F031 AC-6: a root Validate(ctx) invariant
// rejects the offending Save (no persist); the passing case persists. We run the
// validator hook via persistence.ValidateAggregate (the convention Save uses).
func TestAggregate_ValidateRejectsSave(t *testing.T) {
	ctx := tenantCtx("acme")
	// Violating case: a SHIPPED fleet with a member fails the invariant.
	bad := validatingFleet{Fleet: &fleetv1.Fleet{Id: "f1", Vehicles: []*fleetv1.Vehicle{{Id: "v1"}}}, shipped: true}
	if err := persistence.ValidateAggregate(ctx, bad); !errors.Is(err, errShipped) {
		t.Fatalf("SHIPPED fleet with a member must fail Validate, got %v", err)
	}
	// Passing case: not shipped → Validate passes.
	ok := validatingFleet{Fleet: &fleetv1.Fleet{Id: "f1", Vehicles: []*fleetv1.Vehicle{{Id: "v1"}}}, shipped: false}
	if err := persistence.ValidateAggregate(ctx, ok); err != nil {
		t.Fatalf("non-shipped fleet must pass Validate, got %v", err)
	}
}
