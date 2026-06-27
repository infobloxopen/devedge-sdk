package apikeyv1_test

// etag_cas_sqlite_test.go — AIP-154 optimistic-concurrency (If-Match) compare-and-set
// tests for the GENERATED ent and GORM repositories on SQLite. Before this work the
// If-Match precondition was enforced ONLY on the in-memory backend: the generated SQL
// repos stamped a fresh etag and wrote by id(+tenant) with NO `WHERE etag=<if-match>`
// guard, so a STALE If-Match on a real backend silently overwrote a row that had
// changed under the caller. These tests pin the fix: a per-row Update is now a true
// CAS whenever an If-Match is present, on BOTH SQL generators.
//
// The precondition is injected with persistence.SetIfMatchExpectation (the same etag
// seam the PreconditionUnary interceptor uses at runtime) so the tests exercise the
// repository CAS directly, without standing up a gRPC server.

import (
	"errors"
	"testing"

	_ "modernc.org/sqlite" // register SQLite driver for enttest + GORM

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"
)

// TestETagCAS_Ent_StaleIfMatchFails drives the generated ent APIKey repository:
// a STALE If-Match → ErrPreconditionFailed; the CORRECT current etag → succeeds;
// NO If-Match → succeeds (back-compat). It also proves a wrong-id Update with an
// If-Match returns ErrNotFound (the 0-row CAS disambiguation), not PreconditionFailed.
func TestETagCAS_Ent_StaleIfMatchFails(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:etag_cas_ent?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyEntRepository(client, enc)
	ctx := tenantCtx("t1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{Id: "k1", AccountId: "t1", KeyValue: "sk_1", Label: "a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.GetEtag() == "" {
		t.Fatal("created etag empty")
	}

	// STALE If-Match: a token that does not match the stored etag must fail CLOSED.
	staleCtx := persistence.SetIfMatchExpectation(ctx, "wrong-etag")
	if _, err := repo.Update(staleCtx, "k1", &apikeyv1.APIKey{Id: "k1", Label: "stale"}, "label"); !errors.Is(err, persistence.ErrPreconditionFailed) {
		t.Fatalf("stale If-Match must yield ErrPreconditionFailed, got %v", err)
	}
	// The stale update must NOT have mutated the row.
	if got, gerr := repo.Get(ctx, "k1"); gerr != nil {
		t.Fatalf("get after stale update: %v", gerr)
	} else if got.GetLabel() != "a" || got.GetEtag() != created.GetEtag() {
		t.Fatalf("stale If-Match must not mutate the row: label=%q etag=%q (want label=a etag=%q)", got.GetLabel(), got.GetEtag(), created.GetEtag())
	}

	// CORRECT current etag: the CAS matches → the update succeeds and the etag rotates.
	okCtx := persistence.SetIfMatchExpectation(ctx, created.GetEtag())
	updated, err := repo.Update(okCtx, "k1", &apikeyv1.APIKey{Id: "k1", Label: "b"}, "label")
	if err != nil {
		t.Fatalf("correct If-Match must succeed, got %v", err)
	}
	if updated.GetLabel() != "b" {
		t.Fatalf("update did not apply: label=%q", updated.GetLabel())
	}
	if updated.GetEtag() == "" || updated.GetEtag() == created.GetEtag() {
		t.Fatalf("successful CAS must rotate the etag: %q (was %q)", updated.GetEtag(), created.GetEtag())
	}

	// NO If-Match: back-compat — an Update with no precondition still succeeds.
	if _, err := repo.Update(ctx, "k1", &apikeyv1.APIKey{Id: "k1", Label: "c"}, "label"); err != nil {
		t.Fatalf("no If-Match must succeed (back-compat), got %v", err)
	}

	// Missing row WITH an If-Match: the 0-row CAS must resolve to NotFound, not Precondition.
	if _, err := repo.Update(persistence.SetIfMatchExpectation(ctx, "anything"), "nope", &apikeyv1.APIKey{Id: "nope", Label: "x"}, "label"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("absent row with If-Match must yield ErrNotFound, got %v", err)
	}
}

// TestETagCAS_GORM_StaleIfMatchFails is the GORM twin of the ent test above, driving
// the generated APIKeyRepository on SQLite with the same three cases plus the
// missing-row disambiguation.
func TestETagCAS_GORM_StaleIfMatchFails(t *testing.T) {
	db := openSoftDeleteDB(t, "etag_cas_gorm")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyRepository(db, enc)
	ctx := tenantCtx("t1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{Id: "k1", AccountId: "t1", KeyValue: "sk_1", Label: "a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.GetEtag() == "" {
		t.Fatal("created etag empty")
	}

	// STALE If-Match → fail closed.
	staleCtx := persistence.SetIfMatchExpectation(ctx, "wrong-etag")
	if _, err := repo.Update(staleCtx, "k1", &apikeyv1.APIKey{Id: "k1", Label: "stale"}, "label"); !errors.Is(err, persistence.ErrPreconditionFailed) {
		t.Fatalf("stale If-Match must yield ErrPreconditionFailed, got %v", err)
	}
	if got, gerr := repo.Get(ctx, "k1"); gerr != nil {
		t.Fatalf("get after stale update: %v", gerr)
	} else if got.GetLabel() != "a" || got.GetEtag() != created.GetEtag() {
		t.Fatalf("stale If-Match must not mutate the row: label=%q etag=%q (want label=a etag=%q)", got.GetLabel(), got.GetEtag(), created.GetEtag())
	}

	// CORRECT current etag → succeed, etag rotates.
	okCtx := persistence.SetIfMatchExpectation(ctx, created.GetEtag())
	updated, err := repo.Update(okCtx, "k1", &apikeyv1.APIKey{Id: "k1", Label: "b"}, "label")
	if err != nil {
		t.Fatalf("correct If-Match must succeed, got %v", err)
	}
	if updated.GetLabel() != "b" {
		t.Fatalf("update did not apply: label=%q", updated.GetLabel())
	}
	if updated.GetEtag() == "" || updated.GetEtag() == created.GetEtag() {
		t.Fatalf("successful CAS must rotate the etag: %q (was %q)", updated.GetEtag(), created.GetEtag())
	}

	// NO If-Match → back-compat success.
	if _, err := repo.Update(ctx, "k1", &apikeyv1.APIKey{Id: "k1", Label: "c"}, "label"); err != nil {
		t.Fatalf("no If-Match must succeed (back-compat), got %v", err)
	}

	// Missing row WITH an If-Match → NotFound (0-row CAS disambiguation).
	if _, err := repo.Update(persistence.SetIfMatchExpectation(ctx, "anything"), "nope", &apikeyv1.APIKey{Id: "nope", Label: "x"}, "label"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("absent row with If-Match must yield ErrNotFound, got %v", err)
	}
}

// TestETagCAS_GORM_FullUpdatePath exercises the no-field-mask (full) Update arm,
// which is a distinct code path from the masked arm above: with no fieldMask the
// generated repo writes a column map. The CAS guard must hold there too.
func TestETagCAS_GORM_FullUpdatePath(t *testing.T) {
	db := openSoftDeleteDB(t, "etag_cas_gorm_full")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyRepository(db, enc)
	ctx := tenantCtx("t1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{Id: "k1", AccountId: "t1", KeyValue: "sk_1", Label: "a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Full update (no mask) with a STALE If-Match → ErrPreconditionFailed.
	stale := persistence.SetIfMatchExpectation(ctx, "wrong-etag")
	if _, err := repo.Update(stale, "k1", &apikeyv1.APIKey{Id: "k1", AccountId: "t1", Label: "z"}); !errors.Is(err, persistence.ErrPreconditionFailed) {
		t.Fatalf("full-update stale If-Match must yield ErrPreconditionFailed, got %v", err)
	}
	// Full update with the CORRECT etag → succeeds.
	ok := persistence.SetIfMatchExpectation(ctx, created.GetEtag())
	if _, err := repo.Update(ok, "k1", &apikeyv1.APIKey{Id: "k1", AccountId: "t1", Label: "z"}); err != nil {
		t.Fatalf("full-update correct If-Match must succeed, got %v", err)
	}
}
