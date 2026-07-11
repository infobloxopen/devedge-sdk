package entrepo_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/persistence/entrepo"
)

// trivialDedupStore returns a store whose closures are wired to harmless no-ops, so tests
// can reach the store's own guards (key validation) without a real ent client.
func trivialDedupStore() *entrepo.EntDurableDedupStore {
	s := entrepo.NewEntDurableDedupStore()
	s.InsertFn = func(context.Context, entrepo.EntIdempotencyRow) error { return nil }
	s.ReadFn = func(context.Context, string) (entrepo.EntIdempotencyRow, bool, error) {
		return entrepo.EntIdempotencyRow{}, false, nil
	}
	s.ReclaimFn = func(context.Context, entrepo.EntIdempotencyRow, time.Time) (int64, error) { return 0, nil }
	s.CompleteFn = func(context.Context, string, string, []byte) (int64, error) { return 1, nil }
	s.AbandonFn = func(context.Context, string) (int64, error) { return 0, nil }
	s.GCDeleteFn = func(context.Context, time.Time, int) (int64, error) { return 0, nil }
	return s
}

// TestEntDedup_NilClosureFailsLoud (L2): a mis-wired store returns a clear error instead of
// a raw nil-func panic.
func TestEntDedup_NilClosureFailsLoud(t *testing.T) {
	s := entrepo.NewEntDurableDedupStore() // no closures wired
	key := persistence.IdempotencyKey{Tenant: "t", Method: "m", RequestID: "r"}
	_, _, err := s.Claim(context.Background(), key, "fp", time.Hour)
	if err == nil || !strings.Contains(err.Error(), "missing one or more closures") {
		t.Fatalf("want a clear missing-closures error, got: %v", err)
	}
	if _, _, err := s.Lookup(context.Background(), key); err == nil {
		t.Fatal("Lookup on a mis-wired store must error, not panic")
	}
}

// TestEntDedup_NULKeyRejected (S2): a NUL byte in any key component breaks the encoded-id
// injectivity, so it is rejected fail-loud before any storage op.
func TestEntDedup_NULKeyRejected(t *testing.T) {
	s := trivialDedupStore()
	for _, tc := range []persistence.IdempotencyKey{
		{Tenant: "t\x00x", Method: "m", RequestID: "r"},
		{Tenant: "t", Method: "m\x00x", RequestID: "r"},
		{Tenant: "t", Method: "m", RequestID: "r\x00x"},
	} {
		if _, _, err := s.Claim(context.Background(), tc, "fp", time.Hour); err == nil || !strings.Contains(err.Error(), "NUL") {
			t.Fatalf("Claim(%q) must reject a NUL component, got: %v", tc, err)
		}
		if _, _, err := s.Lookup(context.Background(), tc); err == nil || !strings.Contains(err.Error(), "NUL") {
			t.Fatalf("Lookup(%q) must reject a NUL component, got: %v", tc, err)
		}
	}
	// A NUL-free key passes the guard (reaches the no-op closures → fresh claim).
	ok := persistence.IdempotencyKey{Tenant: "t", Method: "m", RequestID: "r"}
	if _, claimed, err := s.Claim(context.Background(), ok, "fp", time.Hour); err != nil || !claimed {
		t.Fatalf("NUL-free key must claim: claimed=%v err=%v", claimed, err)
	}
}
