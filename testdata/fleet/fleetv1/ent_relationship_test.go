package fleetv1_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite" // register SQLite driver for enttest

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/testdata/fleet/ent/enttest"
	"github.com/infobloxopen/devedge-sdk/testdata/fleet/fleetv1"
)

func tenantCtx(accountID string) context.Context {
	return middleware.WithTenantID(context.Background(), accountID)
}

// TestEntRelationship is the regression guard for issues #30/#31: the ent
// has_many/belongs_to edge codegen. It only compiles if protoc-gen-ent emitted
// a buildable two-resource ent client (singular related type, a proper
// edge.From(...).Ref(...) inverse, and a single SetFleetID from .Field()), and
// it only passes if that edge actually links a Fleet to its Vehicles.
func TestEntRelationship(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:fleet_edge?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetEntRepository(client)
	vehicles := fleetv1.NewVehicleEntRepository(client)

	fleet, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "fleet-1", DisplayName: "East Coast"})
	if err != nil {
		t.Fatalf("create fleet: %v", err)
	}

	// Two vehicles belonging to the fleet, set via the FK-backed belongs_to edge.
	for _, vin := range []string{"VIN-AAA", "VIN-BBB"} {
		if _, err := vehicles.Create(ctx, &fleetv1.Vehicle{Id: "veh-" + vin, Vin: vin, FleetId: fleet.Id}); err != nil {
			t.Fatalf("create vehicle %s: %v", vin, err)
		}
	}

	// belongs_to: the scalar FK round-trips through the edge field.
	got, err := vehicles.Get(ctx, "veh-VIN-AAA")
	if err != nil {
		t.Fatalf("get vehicle: %v", err)
	}
	if got.FleetId != fleet.Id {
		t.Errorf("vehicle.FleetId = %q, want %q", got.FleetId, fleet.Id)
	}

	// belongs_to edge traversal: vehicle -> fleet.
	entVeh, err := client.Vehicle.Get(ctx, "veh-VIN-AAA")
	if err != nil {
		t.Fatalf("ent get vehicle: %v", err)
	}
	parent, err := entVeh.QueryFleet().Only(ctx)
	if err != nil {
		t.Fatalf("query fleet edge: %v", err)
	}
	if parent.ID != fleet.Id {
		t.Errorf("vehicle.QueryFleet().ID = %q, want %q", parent.ID, fleet.Id)
	}

	// has_many edge traversal: fleet -> vehicles (the inverse of belongs_to).
	entFleet, err := client.Fleet.Get(ctx, fleet.Id)
	if err != nil {
		t.Fatalf("ent get fleet: %v", err)
	}
	kids, err := entFleet.QueryVehicles().All(ctx)
	if err != nil {
		t.Fatalf("query vehicles edge: %v", err)
	}
	if len(kids) != 2 {
		t.Fatalf("fleet.QueryVehicles() = %d vehicles, want 2", len(kids))
	}
}

// TestEntTenantIsolation confirms the TenantMixin still scopes both resources in
// the two-resource fixture (the relationship change must not weaken isolation).
func TestEntTenantIsolation(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:fleet_tenant?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	vehicles := fleetv1.NewVehicleEntRepository(client)

	// No fleet_id here: this test isolates tenant scoping, not the FK edge
	// (fleet_id is enforced as a real foreign key when set).
	if _, err := vehicles.Create(tenantCtx("acme"), &fleetv1.Vehicle{Id: "v-acme", Vin: "ACME-1"}); err != nil {
		t.Fatalf("create acme vehicle: %v", err)
	}
	if _, err := vehicles.Create(tenantCtx("globex"), &fleetv1.Vehicle{Id: "v-globex", Vin: "GLOBEX-1"}); err != nil {
		t.Fatalf("create globex vehicle: %v", err)
	}

	// acme lists only its own vehicle.
	list, _, err := vehicles.List(tenantCtx("acme"), persistence.ListOptions{})
	if err != nil {
		t.Fatalf("list acme: %v", err)
	}
	if len(list) != 1 || list[0].Id != "v-acme" {
		t.Fatalf("acme sees %d vehicles (%v), want just v-acme", len(list), list)
	}
}

// TestEntPerTenantUnique is the regression guard for GH #44 and #45: a `unique`
// business field on a tenant-scoped ent resource must be unique PER TENANT
// (composite (account_id, vin)), not globally — and a same-tenant duplicate must
// surface as a clean persistence.ErrConflict with NO raw SQL, not a 500 leaking
// the constraint text.
func TestEntPerTenantUnique(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:fleet_unique?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	vehicles := fleetv1.NewVehicleEntRepository(client)

	// #44: the SAME vin is reusable across tenants — a global unique index would
	// (wrongly) reject the second tenant.
	if _, err := vehicles.Create(tenantCtx("acme"), &fleetv1.Vehicle{Id: "v-acme", Vin: "DUP-VIN"}); err != nil {
		t.Fatalf("acme create vin=DUP-VIN: %v", err)
	}
	if _, err := vehicles.Create(tenantCtx("globex"), &fleetv1.Vehicle{Id: "v-globex", Vin: "DUP-VIN"}); err != nil {
		t.Fatalf("globex reuse of vin=DUP-VIN must be allowed (per-tenant unique), got: %v", err)
	}

	// Within one tenant the vin is still unique → a duplicate must be a clean
	// ErrConflict (#45), and the error must not leak the raw driver constraint text.
	_, err := vehicles.Create(tenantCtx("acme"), &fleetv1.Vehicle{Id: "v-acme-2", Vin: "DUP-VIN"})
	if err == nil {
		t.Fatal("same-tenant duplicate vin must fail")
	}
	if !errors.Is(err, persistence.ErrConflict) {
		t.Errorf("same-tenant duplicate: want ErrConflict, got %v", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "constraint failed") {
		t.Errorf("error leaks raw SQL constraint text: %v", err)
	}
}

// #49 follow-up (partial unique index, PostgreSQL/SQLite): on a soft-delete +
// per-tenant-unique resource, a unique display_name must be re-creatable once
// the holding Fleet is soft-deleted — while two LIVE fleets still can't share
// it. The generated schema carries entsql.IndexWhere("delete_time IS NULL") on
// the (account_id, display_name) composite; this proves it works end-to-end on
// SQLite (same partial-index path as PostgreSQL). Exec is used for the
// soft-delete so the post-update hydrate SELECT (which the soft-delete
// interceptor would filter) is skipped.
func TestEntFleet_PartialUnique_RecreateAfterSoftDelete(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:fleet_partial?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()
	ctx := context.Background()

	if err := client.Fleet.Create().SetID("f1").SetAccountID("t1").SetDisplayName("ops").Exec(ctx); err != nil {
		t.Fatalf("create f1: %v", err)
	}
	// Two LIVE fleets with the same (account_id, display_name) must still conflict.
	if err := client.Fleet.Create().SetID("f1b").SetAccountID("t1").SetDisplayName("ops").Exec(ctx); err == nil {
		t.Fatal("two live fleets sharing display_name must violate the unique index")
	}
	// Soft-delete f1 → its row leaves the partial index.
	if err := client.Fleet.UpdateOneID("f1").SetDeleteTime(time.Now()).Exec(ctx); err != nil {
		t.Fatalf("soft-delete f1: %v", err)
	}
	// The key is now free: a fresh live fleet may reuse display_name "ops".
	// Before the fix this failed with a unique violation (the dead row kept it).
	if err := client.Fleet.Create().SetID("f2").SetAccountID("t1").SetDisplayName("ops").Exec(ctx); err != nil {
		t.Fatalf("re-create display_name=ops after soft-delete must succeed (partial unique index): %v", err)
	}
}
