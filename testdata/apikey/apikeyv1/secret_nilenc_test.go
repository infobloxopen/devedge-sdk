package apikeyv1_test

import (
	"context"
	"errors"
	"testing"

	_ "modernc.org/sqlite" // register SQLite driver for enttest

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"
)

// TestSEC006_EntSingularFailsLoudWhenEncNil is the SEC-006 regression: constructing
// the ent repository with a nil encryptor and then writing a NON-EMPTY secret must
// now FAIL LOUD (persistence.ErrNoEncryptor) rather than silently dropping the
// secret. Before the fix the Create succeeded and the secret vanished with no error.
func TestSEC006_EntSingularFailsLoudWhenEncNil(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:sec006_singular?mode=memory&cache=shared&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	repo := apikeyv1.NewAPIKeyEntRepository(client, nil) // nil encryptor
	ctx := middleware.WithSystemContext(context.Background())

	_, err := repo.Create(ctx, &apikeyv1.APIKey{
		Id:        "k-nil-enc",
		AccountId: "t1",
		KeyValue:  "sk_live_THIS_SHOULD_NOT_VANISH",
	})
	if err == nil {
		t.Fatal("SEC-006 regression: Create with nil enc + non-empty secret SUCCEEDED (silent drop)")
	}
	if !errors.Is(err, persistence.ErrNoEncryptor) {
		t.Fatalf("want errors.Is(err, persistence.ErrNoEncryptor); got %v", err)
	}

	// A create with NO secret value still succeeds with a nil encryptor.
	if _, err := repo.Create(ctx, &apikeyv1.APIKey{Id: "k-no-secret", AccountId: "t1"}); err != nil {
		t.Fatalf("create with no secret value should still succeed with nil enc, got: %v", err)
	}
}

// TestSEC006_EntBatchFailsLoudWhenEncNil is the SEC-006 consistency regression: the
// ent BATCH path used to PANIC (nil-pointer) on a nil encryptor; it must now return
// the same persistence.ErrNoEncryptor error as the singular and GORM paths.
func TestSEC006_EntBatchFailsLoudWhenEncNil(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:sec006_batch?mode=memory&cache=shared&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	ctx := middleware.WithSystemContext(context.Background())

	// Seed a row (no secret) with a REAL encryptor so BatchUpdate has a target.
	seed := apikeyv1.NewAPIKeyEntRepository(client, secret.NewDev(make([]byte, 32)))
	if _, err := seed.Create(ctx, &apikeyv1.APIKey{Id: "k-batch", AccountId: "t1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	batch := apikeyv1.NewAPIKeyEntBatchRepository(client, nil) // nil encryptor
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SEC-006 regression: ent batch PANICKED on nil enc instead of erroring: %v", r)
		}
	}()

	_, err := batch.BatchUpdate(ctx, []persistence.BatchUpdateItem[*apikeyv1.APIKey, string]{
		{Key: "k-batch", Entity: &apikeyv1.APIKey{KeyValue: "sk_batch_secret"}, FieldMask: []string{"key_value"}},
	})
	if err == nil {
		t.Fatal("SEC-006 regression: batch update with nil enc + secret returned no error")
	}
	if !errors.Is(err, persistence.ErrNoEncryptor) {
		t.Fatalf("want errors.Is(err, persistence.ErrNoEncryptor); got %v", err)
	}
}
