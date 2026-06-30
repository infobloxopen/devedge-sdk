package apikeyv1_test

// resource_identity_test.go — BC-12 first-class resource identity. Proves the
// generated Create on BOTH backends (ent + GORM) realizes server-generated ids by
// default and honors the (infoblox.field.v1.opts).id annotation:
//
//	(1) SERVER_GENERATED (default, no annotation) + empty id -> a fresh UUIDv7 is
//	    minted and the row is retrievable by that id;
//	(2) a caller-supplied id is honored verbatim;
//	(3) USER_SETTABLE + empty id -> codes.InvalidArgument and nothing is persisted;
//	(4) the WithIDGenerator constructor option overrides the generator.
//
// APIKey carries no id annotation (the default SERVER_GENERATED/UUID7 path);
// Token's id is annotated STRATEGY_USER_SETTABLE.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/infobloxopen/devedge-sdk/middleware"
	"github.com/infobloxopen/devedge-sdk/persistence"
	"github.com/infobloxopen/devedge-sdk/secret"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
	"github.com/infobloxopen/devedge-sdk/testdata/apikey/ent/enttest"
)

// openTokenDB opens an in-memory SQLite GORM database with the TokenModel schema
// migrated, for the USER_SETTABLE Token storage tests.
func openTokenDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(openTestSQLite("file:"+dsn+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("open token test db: %v", err)
	}
	if err := db.AutoMigrate(&apikeyv1.TokenModel{}); err != nil {
		t.Fatalf("automigrate token: %v", err)
	}
	return db
}

// assertUUIDv7 fails unless s parses as a UUID and is version 7 (time-ordered).
func assertUUIDv7(t *testing.T, label, s string) {
	t.Helper()
	if s == "" {
		t.Fatalf("%s: id is empty; a server-generated id must be minted", label)
	}
	u, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("%s: id %q is not a valid UUID: %v", label, s, err)
	}
	if u.Version() != 7 {
		t.Errorf("%s: id %q is UUID v%d, want v7 (the default generator)", label, s, u.Version())
	}
}

// --- Behavior 1: SERVER_GENERATED (default) + empty id -> fresh UUIDv7, retrievable ---

// TestIdentity_ENT_ServerGeneratedDefault: APIKey has no id annotation, so the ent
// Create mints a UUIDv7 when the caller leaves id empty, and the row is retrievable
// by the minted id.
func TestIdentity_ENT_ServerGeneratedDefault(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:id_ent_servergen?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	repo := apikeyv1.NewAPIKeyEntRepository(client, secret.NewDev(make([]byte, 32)))
	ctx := tenantCtx("t1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{AccountId: "t1", KeyValue: "sk_1"}) // no Id
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	assertUUIDv7(t, "ent create", created.Id)

	got, err := repo.Get(ctx, created.Id)
	if err != nil {
		t.Fatalf("get by minted id %q: %v", created.Id, err)
	}
	if got.Id != created.Id {
		t.Errorf("get id = %q, want %q (the minted id must be persisted)", got.Id, created.Id)
	}
}

// TestIdentity_GORM_ServerGeneratedDefault is the GORM counterpart of behavior 1.
func TestIdentity_GORM_ServerGeneratedDefault(t *testing.T) {
	db := openSoftDeleteDB(t, "id_gorm_servergen")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	repo := apikeyv1.NewAPIKeyRepository(db, secret.NewDev(make([]byte, 32)))
	ctx := middleware.WithTenantID(context.Background(), "t1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{AccountId: "t1", KeyValue: "sk_1"}) // no Id
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	assertUUIDv7(t, "gorm create", created.Id)

	got, err := repo.Get(ctx, created.Id)
	if err != nil {
		t.Fatalf("get by minted id %q: %v", created.Id, err)
	}
	if got.Id != created.Id {
		t.Errorf("get id = %q, want %q (the minted id must be persisted)", got.Id, created.Id)
	}
}

// --- Behavior 2: a caller-supplied id is honored ---

// TestIdentity_ENT_CallerSuppliedHonored: a non-empty caller id is persisted as-is
// (never overwritten by the generator).
func TestIdentity_ENT_CallerSuppliedHonored(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:id_ent_supplied?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	repo := apikeyv1.NewAPIKeyEntRepository(client, secret.NewDev(make([]byte, 32)))
	ctx := tenantCtx("t1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{Id: "my-chosen-id", AccountId: "t1", KeyValue: "sk_1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Id != "my-chosen-id" {
		t.Errorf("ent honored id = %q, want my-chosen-id", created.Id)
	}
	if _, err := repo.Get(ctx, "my-chosen-id"); err != nil {
		t.Errorf("get by supplied id: %v", err)
	}
}

// TestIdentity_GORM_CallerSuppliedHonored is the GORM counterpart of behavior 2.
func TestIdentity_GORM_CallerSuppliedHonored(t *testing.T) {
	db := openSoftDeleteDB(t, "id_gorm_supplied")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	repo := apikeyv1.NewAPIKeyRepository(db, secret.NewDev(make([]byte, 32)))
	ctx := middleware.WithTenantID(context.Background(), "t1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{Id: "my-chosen-id", AccountId: "t1", KeyValue: "sk_1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Id != "my-chosen-id" {
		t.Errorf("gorm honored id = %q, want my-chosen-id", created.Id)
	}
	if _, err := repo.Get(ctx, "my-chosen-id"); err != nil {
		t.Errorf("get by supplied id: %v", err)
	}
}

// --- Behavior 3: USER_SETTABLE + empty id -> InvalidArgument, nothing persisted ---

// TestIdentity_ENT_UserSettableRejectsEmpty: Token's id is USER_SETTABLE, so the
// ent Create rejects an empty id with codes.InvalidArgument and persists no row.
func TestIdentity_ENT_UserSettableRejectsEmpty(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:id_ent_usersettable?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	repo := apikeyv1.NewTokenEntRepository(client)
	ctx := context.Background()

	_, err := repo.Create(ctx, &apikeyv1.Token{Label: "no-id"}) // empty Id
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ent create empty USER_SETTABLE id: got %v, want InvalidArgument", err)
	}

	// A subsequent valid create proves the empty-id Create persisted nothing and the
	// repository is otherwise functional.
	if _, err := repo.Create(ctx, &apikeyv1.Token{Id: "tok-1", Label: "ok"}); err != nil {
		t.Fatalf("create with explicit id: %v", err)
	}
	all, _, err := repo.List(ctx, persistence.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].Id != "tok-1" {
		t.Errorf("after a rejected empty-id create, rows = %d %v; want exactly [tok-1] (nothing persisted for the empty id)", len(all), all)
	}
}

// TestIdentity_GORM_UserSettableRejectsEmpty is the GORM counterpart of behavior 3.
func TestIdentity_GORM_UserSettableRejectsEmpty(t *testing.T) {
	db := openTokenDB(t, "id_gorm_usersettable")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	repo := apikeyv1.NewTokenRepository(db)
	ctx := context.Background()

	_, err := repo.Create(ctx, &apikeyv1.Token{Label: "no-id"}) // empty Id
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("gorm create empty USER_SETTABLE id: got %v, want InvalidArgument", err)
	}

	var count int64
	if err := db.Model(&apikeyv1.TokenModel{}).Count(&count).Error; err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if count != 0 {
		t.Errorf("after a rejected empty-id create, %d rows persisted; want 0", count)
	}
}

// --- Behavior 4: the WithIDGenerator constructor option overrides the generator ---

// fixedIDGenerator is a deterministic IDGenerator for asserting the override seam.
type fixedIDGenerator struct{ id string }

func (f fixedIDGenerator) NewID() string { return f.id }

// TestIdentity_ENT_WithIDGeneratorOverride: WithIDGenerator replaces the default
// UUIDv7 generator so an id-less Create gets the injected generator's value.
func TestIdentity_ENT_WithIDGeneratorOverride(t *testing.T) {
	client := enttest.Open(t, "sqlite3", "file:id_ent_override?mode=memory&_pragma=foreign_keys(1)", enttest.WithOptions())
	defer client.Close()

	repo := apikeyv1.NewAPIKeyEntRepository(client, secret.NewDev(make([]byte, 32)),
		persistence.WithIDGenerator(fixedIDGenerator{id: "fixed-ent-id"}))
	ctx := tenantCtx("t1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{AccountId: "t1", KeyValue: "sk_1"}) // no Id
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Id != "fixed-ent-id" {
		t.Errorf("override generator: id = %q, want fixed-ent-id", created.Id)
	}
}

// TestIdentity_GORM_WithIDGeneratorOverride is the GORM counterpart of behavior 4.
func TestIdentity_GORM_WithIDGeneratorOverride(t *testing.T) {
	db := openSoftDeleteDB(t, "id_gorm_override")
	sqlDB, _ := db.DB()
	defer sqlDB.Close()

	repo := apikeyv1.NewAPIKeyRepository(db, secret.NewDev(make([]byte, 32)),
		persistence.WithIDGenerator(fixedIDGenerator{id: "fixed-gorm-id"}))
	ctx := middleware.WithTenantID(context.Background(), "t1")

	created, err := repo.Create(ctx, &apikeyv1.APIKey{AccountId: "t1", KeyValue: "sk_1"}) // no Id
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Id != "fixed-gorm-id" {
		t.Errorf("override generator: id = %q, want fixed-gorm-id", created.Id)
	}
}
