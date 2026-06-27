package fleetv1_test

// postgres_test.go — Phase-2 validation of the aggregate / CAS / cascade
// machinery on REAL Postgres (the production target). Each test either runs
// against a testcontainers postgres:16 server or SKIPS cleanly when Docker is
// unavailable (see pgtest_test.go).
//
// THE HEADLINE PROOF (TestPG_*_ConcurrentAggregateSave_ExactlyOneWinner):
// two goroutines Load the SAME Fleet aggregate (same root etag), each add a
// DIFFERENT vehicle, and Save concurrently. The Phase-1 If-Match CAS makes the
// root-etag bump an UPDATE ... WHERE etag = <loaded etag>, so on Postgres under
// READ COMMITTED EXACTLY ONE Save commits and the other gets
// persistence.ErrPreconditionFailed. This is the lost-update race SQLite cannot
// exhibit (SQLite serializes writes), which is why Phase 1 could only be
// *functionally* tested there. Run these under `-race`.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/infobloxopen/devedge-sdk/middleware/etag"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/testdata/fleet/fleetv1"
)

// pgRaceRepeats is how many times the concurrent-Save race is replayed per test.
// A handful of fresh rounds makes the exactly-one-winner assertion meaningful
// (a single round could pass by luck of scheduling); each round uses a distinct
// fleet id on the same database.
const pgRaceRepeats = 8

// TestPG_EntConcurrentAggregateSave_ExactlyOneWinner is the headline proof on the
// ent backend. Two goroutines Load the same fleet aggregate at the same etag, each
// add a different vehicle, and Save concurrently. Exactly one Save must succeed and
// bump the root etag; the loser must get persistence.ErrPreconditionFailed from the
// Phase-1 If-Match CAS.
//
// On SQLite this race cannot even occur — writes are serialized, so the second Save
// always observes the first's etag bump and the "lost update" the CAS prevents
// never materializes. On Postgres (READ COMMITTED) the two Saves genuinely overlap;
// WITHOUT the CAS both would commit (the classic lost update). The shipped assertion
// is exactly-one-winner.
func TestPG_EntConcurrentAggregateSave_ExactlyOneWinner(t *testing.T) {
	client := openFleetEntPG(t)
	ctx := tenantCtx("acme")
	agg := fleetAggregate(client)
	fleets := fleetv1.NewFleetEntRepository(client)

	for round := 0; round < pgRaceRepeats; round++ {
		fleetID := fleetIDForRound("ent", round)
		// display_name is unique per tenant, so each round needs a distinct one.
		if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: fleetID, DisplayName: "East-" + itoa(round)}); err != nil {
			t.Fatalf("round %d seed fleet: %v", round, err)
		}

		// Both writers Load the SAME aggregate version (same root etag).
		rootA, err := agg.Load(ctx, fleetID)
		if err != nil {
			t.Fatalf("round %d Load A: %v", round, err)
		}
		rootB, err := agg.Load(ctx, fleetID)
		if err != nil {
			t.Fatalf("round %d Load B: %v", round, err)
		}
		if rootA.GetEtag() != rootB.GetEtag() {
			t.Fatalf("round %d: both Loads must observe the same etag, got %q vs %q", round, rootA.GetEtag(), rootB.GetEtag())
		}

		// vin is unique per tenant, so each round (and each writer) needs a distinct one.
		rootA.Vehicles = append(rootA.Vehicles, &fleetv1.Vehicle{Id: fleetID + "-vA", Vin: fleetID + "-VIN-A"})
		rootB.Vehicles = append(rootB.Vehicles, &fleetv1.Vehicle{Id: fleetID + "-vB", Vin: fleetID + "-VIN-B"})

		succeeded, precondFailed := raceTwoSaves(agg, ctx, rootA, rootB)
		assertExactlyOneWinner(t, round, succeeded, precondFailed)
	}
}

// TestPG_GormConcurrentAggregateSave_ExactlyOneWinner is the GORM twin of the
// headline proof: same race, same exactly-one-winner assertion, on a real Postgres
// database through the generated GORM repositories + GormTxRunner.
func TestPG_GormConcurrentAggregateSave_ExactlyOneWinner(t *testing.T) {
	db := openFleetGormPG(t)
	ctx := tenantCtx("acme")
	agg := gormFleetAggregate(db)
	fleets := fleetv1.NewFleetRepository(db)

	for round := 0; round < pgRaceRepeats; round++ {
		fleetID := fleetIDForRound("gorm", round)
		// display_name is unique per tenant, so each round needs a distinct one.
		if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: fleetID, AccountId: "acme", DisplayName: "East-" + itoa(round)}); err != nil {
			t.Fatalf("round %d seed fleet: %v", round, err)
		}

		rootA, err := agg.Load(ctx, fleetID)
		if err != nil {
			t.Fatalf("round %d Load A: %v", round, err)
		}
		rootB, err := agg.Load(ctx, fleetID)
		if err != nil {
			t.Fatalf("round %d Load B: %v", round, err)
		}
		if rootA.GetEtag() != rootB.GetEtag() {
			t.Fatalf("round %d: both Loads must observe the same etag, got %q vs %q", round, rootA.GetEtag(), rootB.GetEtag())
		}

		// vin is unique per tenant, so each round (and each writer) needs a distinct one.
		rootA.Vehicles = append(rootA.Vehicles, &fleetv1.Vehicle{Id: fleetID + "-vA", AccountId: "acme", Vin: fleetID + "-VIN-A"})
		rootB.Vehicles = append(rootB.Vehicles, &fleetv1.Vehicle{Id: fleetID + "-vB", AccountId: "acme", Vin: fleetID + "-VIN-B"})

		succeeded, precondFailed := raceTwoSaves(agg, ctx, rootA, rootB)
		assertExactlyOneWinner(t, round, succeeded, precondFailed)
	}
}

// raceTwoSaves runs two aggregate Saves concurrently and reports how many
// succeeded and how many failed with ErrPreconditionFailed (any other error fails
// the test indirectly via the caller's assertion). It is generic over the root key
// type so it serves both the ent and gorm aggregate repositories.
func raceTwoSaves[A any](agg interface {
	Save(context.Context, A) (A, error)
}, ctx context.Context, a, b A) (succeeded, precondFailed int) {
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []error
	)
	run := func(root A) {
		defer wg.Done()
		_, err := agg.Save(ctx, root)
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	wg.Add(2)
	go run(a)
	go run(b)
	wg.Wait()
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, persistence.ErrPreconditionFailed):
			precondFailed++
		}
	}
	return succeeded, precondFailed
}

// assertExactlyOneWinner is the shared CAS invariant: of two concurrent Saves on
// the same aggregate version, exactly one commits and exactly one is rejected with
// ErrPreconditionFailed. Anything else (both win = lost update; both fail; an
// unexpected error) is a failure.
func assertExactlyOneWinner(t *testing.T, round, succeeded, precondFailed int) {
	t.Helper()
	if succeeded != 1 || precondFailed != 1 {
		t.Fatalf("round %d: CAS must yield EXACTLY ONE winner: got %d succeeded, %d ErrPreconditionFailed (want 1/1) — "+
			"succeeded>1 would be the lost update the Phase-1 CAS exists to prevent", round, succeeded, precondFailed)
	}
}

func fleetIDForRound(backend string, round int) string {
	return backend + "-f" + string(rune('0'+round%10)) + "-" + backend + itoa(round)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestPG_EntPerRowCAS_ExactlyOneWinner proves the Phase-1 per-row If-Match CAS on
// the ent backend over real Postgres: two concurrent Updates of the SAME fleet,
// each carrying the SAME loaded etag as If-Match, must resolve to exactly one
// success and one persistence.ErrPreconditionFailed (the stale writer).
func TestPG_EntPerRowCAS_ExactlyOneWinner(t *testing.T) {
	client := openFleetEntPG(t)
	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetEntRepository(client)

	for round := 0; round < pgRaceRepeats; round++ {
		id := "ent-row-" + itoa(round)
		seeded, err := fleets.Create(ctx, &fleetv1.Fleet{Id: id, DisplayName: "ent-row-v0-" + itoa(round)})
		if err != nil {
			t.Fatalf("round %d seed: %v", round, err)
		}
		loadedEtag := seeded.GetEtag()
		succeeded, precondFailed := racePerRowUpdate(ctx, fleets, id, loadedEtag, round)
		assertExactlyOneWinner(t, round, succeeded, precondFailed)
	}
}

// TestPG_GormPerRowCAS_ExactlyOneWinner is the GORM twin of the per-row CAS proof.
func TestPG_GormPerRowCAS_ExactlyOneWinner(t *testing.T) {
	db := openFleetGormPG(t)
	ctx := tenantCtx("acme")
	fleets := fleetv1.NewFleetRepository(db)

	for round := 0; round < pgRaceRepeats; round++ {
		id := "gorm-row-" + itoa(round)
		seeded, err := fleets.Create(ctx, &fleetv1.Fleet{Id: id, AccountId: "acme", DisplayName: "gorm-row-v0-" + itoa(round)})
		if err != nil {
			t.Fatalf("round %d seed: %v", round, err)
		}
		loadedEtag := seeded.GetEtag()
		succeeded, precondFailed := racePerRowUpdate(ctx, fleets, id, loadedEtag, round)
		assertExactlyOneWinner(t, round, succeeded, precondFailed)
	}
}

// racePerRowUpdate fires two concurrent Updates of the same fleet, both stamping
// the same If-Match (the loaded etag), and reports the success / precondition-fail
// tally. The repo interface is satisfied by both the ent and gorm FleetRepository.
// Each writer targets a distinct, round-unique display_name so the per-tenant
// unique index never spuriously rejects a winner.
func racePerRowUpdate(ctx context.Context, repo interface {
	Update(context.Context, string, *fleetv1.Fleet, ...string) (*fleetv1.Fleet, error)
}, id, ifMatch string, round int) (succeeded, precondFailed int) {
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []error
	)
	upd := func(name string) {
		defer wg.Done()
		cctx := etag.SetIfMatch(ctx, ifMatch)
		_, err := repo.Update(cctx, id, &fleetv1.Fleet{Id: id, AccountId: "acme", DisplayName: name}, "display_name")
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	wg.Add(2)
	go upd(id + "-writerA-" + itoa(round))
	go upd(id + "-writerB-" + itoa(round))
	wg.Wait()
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, persistence.ErrPreconditionFailed):
			precondFailed++
		}
	}
	return succeeded, precondFailed
}

// TestPG_EntCascadeOnRootDelete proves the Fleet→Vehicle ON DELETE CASCADE on
// native Postgres referential integrity (no SQLite foreign_keys pragma needed):
// hard-deleting the root removes its owned members.
func TestPG_EntCascadeOnRootDelete(t *testing.T) {
	client := openFleetEntPG(t)
	ctx := tenantCtx("acme")
	agg := fleetAggregate(client)
	fleets := fleetv1.NewFleetEntRepository(client)

	if _, err := fleets.Create(ctx, &fleetv1.Fleet{Id: "f1", DisplayName: "East"}); err != nil {
		t.Fatalf("seed fleet: %v", err)
	}
	root, err := agg.Load(ctx, "f1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	root.Vehicles = append(root.Vehicles,
		&fleetv1.Vehicle{Id: "v1", Vin: "VIN-1"},
		&fleetv1.Vehicle{Id: "v2", Vin: "VIN-2"},
	)
	if _, err := agg.Save(ctx, root); err != nil {
		t.Fatalf("Save members: %v", err)
	}

	if err := client.Fleet.DeleteOneID("f1").Exec(ctx); err != nil {
		t.Fatalf("hard-delete root: %v", err)
	}
	remaining, err := client.Vehicle.Query().All(ctx)
	if err != nil {
		t.Fatalf("query vehicles: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("deleting the root must cascade to its members on Postgres; %d vehicle(s) survived", len(remaining))
	}
}

// TestPG_GormCascadeOnRootDelete is the GORM twin of the cascade proof on Postgres.
func TestPG_GormCascadeOnRootDelete(t *testing.T) {
	db := openFleetGormPG(t)
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

	var before int64
	if err := db.Model(&fleetv1.VehicleModel{}).Where("fleet_id = ?", "f1").Count(&before).Error; err != nil {
		t.Fatalf("count before: %v", err)
	}
	if before != 2 {
		t.Fatalf("expected 2 vehicles before cascade, got %d", before)
	}

	// Hard-delete the root (Unscoped → real DELETE); the native PG FK cascade
	// removes its members.
	if err := db.Unscoped().Where("id = ?", "f1").Delete(&fleetv1.FleetModel{}).Error; err != nil {
		t.Fatalf("hard-delete root: %v", err)
	}
	var remaining int64
	if err := db.Unscoped().Model(&fleetv1.VehicleModel{}).Count(&remaining).Error; err != nil {
		t.Fatalf("count vehicles: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("deleting the root must cascade to its members on Postgres; %d vehicle(s) survived", remaining)
	}
}
