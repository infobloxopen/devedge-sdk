package apikeyv1_test

// etag_ttl_sqlite_test.go — GORM integration tests for the framework-managed
// ETag (issue #33) and the seam-stamped TTL purge timezone fix (issue #34),
// using the same inline pure-Go SQLite database as the soft-delete tests.

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
)

// TestETag_GORM_StampedAndChanges is the regression guard for issue #33: the
// generated storage must stamp a non-empty ETag on Create, surface it on read
// (so a client can echo it as If-Match), keep it stable between writes, and
// change it on Update (so a stale If-Match yields a 412). Before the fix the
// generated toModel/fromModel never bridged the etag column and nothing stamped
// it, so the proto ETag was always empty.
func TestETag_GORM_StampedAndChanges(t *testing.T) {
	db := openSoftDeleteDB(t, "etag_rt")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyRepository(db, enc)
	ctx := middleware.WithTenantID(context.Background(), "t1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{Id: "k1", AccountId: "t1", KeyValue: "sk_1", Label: "a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Etag == "" {
		t.Fatal("Create response ETag is empty; want a stamped token")
	}

	got, err := repo.Get(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Etag != created.Etag {
		t.Errorf("Get ETag = %q, want %q (must be stable until the next write)", got.Etag, created.Etag)
	}

	updated, err := repo.Update(ctx, "k1", &apikeyv1.APIKey{Id: "k1", Label: "b"}, "label")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Etag == "" || updated.Etag == created.Etag {
		t.Errorf("Update ETag = %q, want a fresh non-empty token (was %q)", updated.Etag, created.Etag)
	}
}

// TestPurgeExpired_GORM_SeamStampedReapsAcrossTimezones is the regression guard
// for issue #34: a row whose expire_time is stamped THROUGH the Create seam
// (stored in UTC via timestamppb.AsTime) must be reaped by PurgeExpired even
// when the cutoff is expressed in a non-UTC zone — the generated PurgeExpired
// normalizes the cutoff to UTC. Before the fix the SQLite TEXT comparison of a
// UTC-stored value against a local-zone cutoff silently reaped nothing.
func TestPurgeExpired_GORM_SeamStampedReapsAcrossTimezones(t *testing.T) {
	db := openSoftDeleteDB(t, "ttl_seam")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	enc := secret.NewDev(make([]byte, 32))
	repo := apikeyv1.NewAPIKeyRepository(db, enc)
	ctx := middleware.WithTenantID(context.Background(), "t1")

	// Stamp expire_time in the past through the seam (Create) — the path a
	// docs-following consumer uses. toModel stores it in UTC.
	if _, err := repo.Create(ctx, &apikeyv1.APIKey{
		Id:         "expired-1",
		AccountId:  "t1",
		KeyValue:   "sk_exp",
		ExpireTime: timestamppb.New(time.Now().Add(-time.Hour)),
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Cutoff = now, but expressed in a fixed non-UTC zone (so the test guards the
	// fix regardless of the host's local timezone, including a UTC CI runner).
	cutoff := time.Now().In(time.FixedZone("UTC-5", -5*3600))
	count, err := repo.PurgeExpired(ctx, cutoff)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if count != 1 {
		t.Fatalf("PurgeExpired reaped %d rows, want 1 (seam-stamped UTC value vs non-UTC cutoff)", count)
	}
}
