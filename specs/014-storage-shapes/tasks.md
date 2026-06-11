# Tasks: dual storage shapes — GORM + ent with tenant isolation

**Branch**: `014-storage-shapes`
**Spec**: `specs/014-storage-shapes/spec.md`
**Plan**: `specs/014-storage-shapes/plan.md`

---

## Track A: GORM improvements

- [X] T001 [S] Update `cmd/protoc-gen-storage/render.go` to inject tenant scoping
  (FR-001). Detect `account_id` field presence via `msg.hasTenantField()` helper.
  When present, add to `List`, `Get`, `Update`, `Delete`:
  ```go
  tenantID := middleware.TenantIDFromContext(ctx)
  if tenantID != "" {
      q = q.Where("account_id = ?", tenantID)
  }
  ```
  Update `render_test.go`: add test asserting the tenant WHERE clause appears when
  `account_id` field is present and is absent when the field is missing.
  Import `github.com/infobloxopen/devedge-sdk/middleware` in generated files.
  Run `go test ./cmd/protoc-gen-storage/... -count=1`.

- [X] T002 [S] Add `LookupBy<GoFieldName>Hash` method generation (FR-002).
  For each `IsSecret:true` field in the message, emit a method:
  ```go
  func (r *APIKeyRepository) LookupByKeyValueHash(ctx context.Context, hash string) (*APIKey, error) {
      if hash == "" { return nil, persistence.ErrNotFound }
      tenantID := middleware.TenantIDFromContext(ctx)
      q := r.db.WithContext(ctx).Where("key_value_hash = ?", hash)
      if tenantID != "" { q = q.Where("account_id = ?", tenantID) }
      var m APIKeyModel
      if err := q.First(&m).Error; err != nil {
          if err == gorm.ErrRecordNotFound { return nil, persistence.ErrNotFound }
          return nil, fmt.Errorf("lookup %s hash: %%w", err)
      }
      return fromModel_APIKey(&m), nil
  }
  ```
  Add render test asserting the method appears for secret fields.
  Run `go test ./cmd/protoc-gen-storage/... -count=1`.

- [X] T003 [S] Regenerate `testdata/apikey` with updated `protoc-gen-storage`:
  `make build && PATH=$PATH:$(go env GOPATH)/bin buf generate --template buf.gen.apikey.yaml`
  Verify `apikey.storage.go` contains tenant scoping and `LookupByKeyValueHash`.
  Run `cd testdata/apikey && go build ./... && go test ./... -count=1`.

---

## Track B: ent shape

- [X] T004 [S] Add `entgo.io/ent` to `go.mod`:
  `go get entgo.io/ent && go mod tidy`
  Verify `go build ./...` passes (no existing code broken).

- [X] T005 [S] Create framework-provided ent mixin and TenantFilterer interface
  in `persistence/entrepo/mixin.go`:
  - `TenantMixin` struct (implements `mixin.Schema`): `Fields()` returns
    `field.String("account_id").NotEmpty().Immutable()`;
    `Interceptors()` returns one interceptor that reads
    `middleware.TenantIDFromContext(ctx)` and calls
    `setTenantFilter(q, tenantID)` when non-empty.
  - `TenantFilterer` interface: `WhereAccountID(id string)` — generated ent
    query types implement this.
  - `setTenantFilter(q ent.Query, tenantID string)`: type-asserts to
    `TenantFilterer`; calls `WhereAccountID` if assertion succeeds.
  Write `persistence/entrepo/mixin_test.go`: unit test that TenantMixin fields
  include `account_id`.

- [X] T006 [S] Implement `persistence/entrepo/repository.go`:
  `EntRepository[T any, K comparable]` with function fields (creator, getter,
  lister, updater, deleter) + `enc secret.Encryptor`. Implement
  `persistence.Repository[T,K]` by calling the respective function field.
  `List` applies `ListOptions` (PageSize, PageToken, Filter) via a
  `ListOptionsApplier` interface that callers implement.
  Write unit tests in `persistence/entrepo/repository_test.go` with mock
  function fields.

- [X] T007 [C] Implement `cmd/protoc-gen-ent/main.go` — ent schema generator
  (FR-003). For each resource message in the proto:
  - Emit `ent/schema/<snake_resource>.go` with `Fields()`, `Indexes()`, and
    `Mixin()` (include `TenantMixin` when `account_id` field present).
  - Fields: mirror proto fields; skip `id` (ent handles primary keys);
    for `secret:true` fields emit `<Name>Hash` + `<Name>Cipher` string fields;
    skip `account_id` (comes from TenantMixin).
  - Indexes: `key_value_hash` index for secret fields; `account_id` index from
    mixin.
  - Emit `ent/generate.go`: `//go:generate go run entgo.io/ent/cmd/ent generate ./schema`
  Write `cmd/protoc-gen-ent/render_test.go`: assert schema output contains
  correct fields, mixin, and indexes.
  Run `go test ./cmd/protoc-gen-ent/... -count=1`.

- [X] T008 [S] Generate ent schema for testdata/apikey and run entc:
  - Add `protoc-gen-ent` to `buf.gen.apikey.yaml`
  - `make build && buf generate --template buf.gen.apikey.yaml`
    → produces `testdata/apikey/ent/schema/apikey.go` + `generate.go`
  - Copy framework mixin to `testdata/apikey/ent/schema/mixin.go`
  - `cd testdata/apikey && go generate ./ent/... && go build ./... 2>&1`
    → ent client generated; builds clean.

- [X] T009 [S] Write `persistence/entrepo/` ent wiring for apikey and an
  integration test (`testdata/apikey/apikeyv1/ent_repository_test.go`):
  - `NewAPIKeyEntRepository(client *ent.Client, enc secret.Encryptor)`
    — generates an `EntRepository` wired to the ent-generated client methods.
  - Test: create alice's key, create bob's key; list as alice → 1 result; list
    as bob → 1 result; get alice's key as bob → ErrNotFound (SC-003/SC-005).
  Uses SQLite in-memory: `enttest.Open(t, "sqlite3", "file:ent?mode=memory")`.
  Run `cd testdata/apikey && go test -run TestEnt -v -count=1 -timeout 30s`.

---

## Track C: Security isolation test passes for both shapes

- [X] T010 [S] Update `testdata/apikey/apikeyv1/apikey_test.go` to add
  `TestSecurity_CrossAccountIsolation_GORM` and
  `TestSecurity_CrossAccountIsolation_Ent` — both use `seccheck.AssertCrossAccountIsolation`
  and both must return zero findings (SC-005).
  Run `cd testdata/apikey && go test -run TestSecurity -v -count=1 -timeout 30s`.

---

## Phase 4: Verify + commit

- [X] T011 [S] `go build ./... && go vet ./... && make test` from repo root — clean.

- [X] T012 [S] `cd testdata/apikey && go test ./... -count=1 -timeout 60s` — all pass.

- [X] T013 [S] Commit all + merge.
  Message: `014: dual storage shapes — GORM tenant isolation + ent privacy layer`.

---

## Dependencies & Execution Order

Tracks A and B are independent; run in parallel.
- T001 → T002 → T003 (GORM track, sequential)
- T004 → T005 → T006 → T007 → T008 → T009 (ent track, sequential)
- T003 + T009 → T010 (both shapes needed for isolation test)
- T010 → T011 → T012 → T013

## Complexity Tags

| Task | Tag | Reason |
|------|-----|--------|
| T001 | [S] | Mechanical template addition; WHERE clause injection |
| T002 | [S] | Template method generation; ~15 LOC addition |
| T003 | [S] | Mechanical: make build + buf generate + go test |
| T004 | [S] | go get + go mod tidy |
| T005 | [S] | Mixin struct + interceptor; ~40 LOC; pattern from authz |
| T006 | [S] | Function-based adapter; ~60 LOC; straightforward |
| T007 | [C] | ent schema template: field mapping + mixin detection + index generation |
| T008 | [S] | Mechanical: buf generate + go generate + go build |
| T009 | [S] | Wiring + integration test; SQLite in-memory; mechanical |
| T010 | [S] | Security test using existing seccheck primitives |
| T011 | [S] | Run commands |
| T012 | [S] | Run tests |
| T013 | [S] | Git commit |
