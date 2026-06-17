package apikeyv1_test

// batch_conformance_test.go — F026 cross-backend conformance suite (T501, AC-020).
// The same AIP-137 batch behaviour matrix runs against all three backends through
// the persistence.BatchRepository interface — MemoryRepository, the generated GORM
// repository, and the generated ent wrapper — proving identical semantics
// (ordering, atomic all-or-nothing, soft-delete awareness) and, for the SQL
// backends, tenant scoping. This is the anti-drift guard for the three
// implementations.

import (
	"testing"

	_ "modernc.org/sqlite" // register SQLite driver for enttest

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"
)

type confBackend struct {
	name           string
	repo           persistence.BatchRepository[*apikeyv1.APIKey, string]
	enforcesTenant bool // SQL backends scope by account_id; MemoryRepository does not
}

func conformanceBackends(t *testing.T) []confBackend {
	t.Helper()
	newEnc := func() secret.Encryptor { return secret.NewDev(make([]byte, 32)) }

	gdb := openSoftDeleteDB(t, "conf_gorm")
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			sqlDB.Close()
		}
	})

	client := enttest.Open(t, "sqlite3", "file:conf_ent?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	t.Cleanup(func() { client.Close() })

	return []confBackend{
		{"memory", persistence.NewMemoryRepository(func(k *apikeyv1.APIKey) string { return k.GetId() }), false},
		{"gorm", apikeyv1.NewAPIKeyRepository(gdb, newEnc()), true},
		{"ent", apikeyv1.NewAPIKeyEntBatchRepository(client, newEnc()), true},
	}
}

func TestBatchConformance(t *testing.T) {
	for _, be := range conformanceBackends(t) {
		be := be
		t.Run(be.name, func(t *testing.T) {
			ctx := tenantCtx("t1")
			seed := func(id string) {
				k := &apikeyv1.APIKey{Id: id, Name: id, AccountId: "t1", KeyPrefix: "p_" + id, KeyValue: "sk_" + id}
				if _, err := be.repo.Create(ctx, k); err != nil {
					t.Fatalf("seed %s: %v", id, err)
				}
			}
			seed("a")
			seed("b")

			// BatchGet — request order preserved.
			got, err := be.repo.BatchGet(ctx, []string{"a", "b"})
			if err != nil || len(got) != 2 || got[0].GetId() != "a" || got[1].GetId() != "b" {
				t.Fatalf("BatchGet: err=%v got=%+v", err, got)
			}
			// BatchGet — a missing key fails the whole call.
			if _, err := be.repo.BatchGet(ctx, []string{"a", "nope"}); err != persistence.ErrNotFound {
				t.Fatalf("BatchGet missing: want ErrNotFound, got %v", err)
			}

			// BatchUpdate — atomic: a missing key fails, the survivor is untouched.
			if _, err := be.repo.BatchUpdate(ctx, []persistence.BatchUpdateItem[*apikeyv1.APIKey, string]{
				{Key: "a", Entity: &apikeyv1.APIKey{Id: "a", Name: "a", AccountId: "t1", KeyPrefix: "DOOMED"}, FieldMask: []string{"key_prefix"}},
				{Key: "nope", Entity: &apikeyv1.APIKey{Id: "nope"}, FieldMask: []string{"key_prefix"}},
			}); err != persistence.ErrNotFound {
				t.Fatalf("BatchUpdate missing: want ErrNotFound, got %v", err)
			}
			if a, gerr := be.repo.Get(ctx, "a"); gerr != nil || a.GetKeyPrefix() == "DOOMED" {
				t.Fatalf("BatchUpdate not atomic: a=%+v err=%v", a, gerr)
			}

			// BatchUpdate — success: items returned in order, change applied.
			upd, err := be.repo.BatchUpdate(ctx, []persistence.BatchUpdateItem[*apikeyv1.APIKey, string]{
				{Key: "a", Entity: &apikeyv1.APIKey{Id: "a", Name: "a", AccountId: "t1", KeyPrefix: "pa2"}, FieldMask: []string{"key_prefix"}},
				{Key: "b", Entity: &apikeyv1.APIKey{Id: "b", Name: "b", AccountId: "t1", KeyPrefix: "pb2"}, FieldMask: []string{"key_prefix"}},
			})
			if err != nil || len(upd) != 2 || upd[0].GetId() != "a" || upd[1].GetId() != "b" {
				t.Fatalf("BatchUpdate success: err=%v upd=%+v", err, upd)
			}
			if a, _ := be.repo.Get(ctx, "a"); a.GetKeyPrefix() != "pa2" {
				t.Fatalf("BatchUpdate success: a.KeyPrefix = %q, want pa2", a.GetKeyPrefix())
			}

			// BatchDelete — atomic: a missing key fails, the survivor stays live.
			if err := be.repo.BatchDelete(ctx, []string{"a", "nope"}); err != persistence.ErrNotFound {
				t.Fatalf("BatchDelete missing: want ErrNotFound, got %v", err)
			}
			if _, err := be.repo.Get(ctx, "a"); err != nil {
				t.Fatalf("BatchDelete not atomic: a should be live, got %v", err)
			}
			// BatchDelete — success soft-deletes; the keys then read as not found.
			if err := be.repo.BatchDelete(ctx, []string{"a", "b"}); err != nil {
				t.Fatalf("BatchDelete: %v", err)
			}
			if _, err := be.repo.BatchGet(ctx, []string{"a"}); err != persistence.ErrNotFound {
				t.Fatalf("BatchGet after delete: want ErrNotFound, got %v", err)
			}
			// BatchDelete on already-deleted keys is NotFound.
			if err := be.repo.BatchDelete(ctx, []string{"a"}); err != persistence.ErrNotFound {
				t.Fatalf("BatchDelete already-deleted: want ErrNotFound, got %v", err)
			}

			// Tenant isolation — SQL backends only (MemoryRepository has no tenant).
			if be.enforcesTenant {
				other := tenantCtx("t2")
				if _, err := be.repo.Create(other, &apikeyv1.APIKey{Id: "x2", Name: "x2", AccountId: "t2", KeyValue: "sk_x2"}); err != nil {
					t.Fatalf("seed t2: %v", err)
				}
				if _, err := be.repo.BatchGet(ctx, []string{"x2"}); err != persistence.ErrNotFound {
					t.Fatalf("cross-tenant BatchGet: want ErrNotFound, got %v", err)
				}
				if err := be.repo.BatchDelete(ctx, []string{"x2"}); err != persistence.ErrNotFound {
					t.Fatalf("cross-tenant BatchDelete: want ErrNotFound, got %v", err)
				}
				if _, err := be.repo.Get(other, "x2"); err != nil {
					t.Fatalf("t2 key must survive cross-tenant BatchDelete, got %v", err)
				}
			}
		})
	}
}
