# Tasks: secret field annotation, encryption at rest, log redaction

**Branch**: `013-secret-fields`
**Spec**: `specs/013-secret-fields/spec.md`
**Plan**: `specs/013-secret-fields/plan.md`

---

## Phase 1: Proto annotation update

- [ ] T001 [S] Update `proto/infoblox/authz/v1/authz.proto` (the mirror copy in this repo):
  Add after the existing `Rule` message and before the `extend` block:
  ```proto
  // FieldRule declares field-level security properties.
  message FieldRule {
    // If true, this field contains sensitive data that must be encrypted at rest,
    // redacted in logs, and never returned in list/get responses after initial creation.
    bool secret = 1;
  }

  extend google.protobuf.FieldOptions {
    FieldRule field = 50002;
  }
  ```
  Then regenerate the authzpb test fixture:
  `PATH=$PATH:$(go env GOPATH)/bin buf generate --template buf.gen.yaml`
  The generated `authzpb/internal/testpb` files should now include the `E_Field` extension.
  Run `go build ./... && go test ./authz/authzpb/... -count=1` — must pass.

---

## Phase 2: Tests (red first)

- [ ] T002 [S] Write `secret/dev_test.go` — unit tests for `DevEncryptor` (must be red until T004):
  - `TestDev_EncryptDecrypt_Roundtrip`: `Encrypt` then `Decrypt` returns original plaintext.
  - `TestDev_Hash_IsStable`: same plaintext + same key always returns the same hash.
  - `TestDev_Hash_DiffersAcrossKeys`: different key → different hash.
  - `TestDev_Encrypt_ProducesDifferentCiphertexts`: same plaintext encrypted twice → different
    ciphertexts (due to random nonce).
  - `TestNewDev_ShortKey_Panics`: `NewDev(make([]byte, 16))` → panics.

- [ ] T003 [S] Write `middleware/redact/redact_test.go` — unit tests for `redact.Message`
  (must be red until T005). These tests need a proto message with a `secret: true` field.
  Use the `authzpb` test fixture approach: create a synthetic proto in
  `middleware/redact/internal/testpb/` with a message that has a `secret: true` field.
  Tests:
  - `TestMessage_RedactsSecretStringField`: message with `key_value = "abc"` and
    `secret: true` → redacted clone has `key_value = "[REDACTED]"`, original unchanged.
  - `TestMessage_LeavesNonSecretFieldsAlone`: non-secret field unchanged after `redact.Message`.
  - `TestMessage_NilMessage`: `redact.Message(nil)` returns nil without panic.
  - `TestMessage_RedactsNestedSecretField`: outer message with an inner message that has a
    `secret: true` field → inner field is redacted.

- [ ] T004 [S] Write `seccheck/secret_test.go` — tests for `AssertNoSecretFieldsLeaked`
  (must be red until T007). Use the same testpb fixture:
  - `TestAssertNoSecretFieldsLeaked_Clean`: response with `key_value = ""` → 0 findings.
  - `TestAssertNoSecretFieldsLeaked_Leaks`: response with `key_value = "sk_abc"` → Error
    finding naming the field.
  - `TestAssertNoSecretFieldsLeaked_NoSecretFields`: message with no `secret: true` fields →
    0 findings regardless of field values.

---

## Phase 3: Implementation

- [ ] T005 [S] Implement `secret/encryptor.go` (interface) + `secret/dev.go` (DevEncryptor,
  FR-002/003):
  - `Encryptor` interface with `Encrypt`, `Decrypt`, `Hash`.
  - `DevEncryptor` struct: holds `key []byte` (32+ bytes).
  - `NewDev(key []byte) Encryptor` — panics if `len(key) < 32`.
  - `Encrypt`: random 12-byte nonce + AES-256-GCM seal; output = `base64(nonce || ciphertext)`.
  - `Decrypt`: base64-decode, split nonce, AES-256-GCM open.
  - `Hash`: `base64.StdEncoding.EncodeToString(hmac.New(sha256.New, key).Sum([]byte(plaintext)))`.
    Wait — correct HMAC: `mac := hmac.New(sha256.New, key); mac.Write([]byte(plaintext)); return base64(mac.Sum(nil))`.
  Run T002 tests — all must pass.

- [ ] T006 [S] Implement `secret/vault.go` (VaultTransitEncryptor, FR-004/005):
  - `VaultTransitEncryptor` struct: `addr, token, keyName string`, `client *http.Client`,
    `hmacKey []byte` (= `sha256.Sum256([]byte(token))[:32]`).
  - `NewVaultTransit(addr, token, keyName string) Encryptor`.
  - `Encrypt(ctx, plaintext)`: `POST {addr}/v1/transit/encrypt/{keyName}` with
    `{"plaintext": base64(plaintext)}`, header `X-Vault-Token: {token}`;
    parse JSON response `data.ciphertext`.
  - `Decrypt(ctx, ciphertext)`: `POST .../decrypt/{keyName}` with `{"ciphertext": ct}`;
    base64-decode `data.plaintext`.
  - `Hash`: local HMAC-SHA256 using `hmacKey` (same implementation as `DevEncryptor.Hash`).
  - `Rewrap(ctx, ciphertext string) (string, error)`: `POST .../rewrap/{keyName}`.
  Write `secret/vault_test.go`: skip all tests if `os.Getenv("VAULT_ADDR") == ""`;
  integration tests for roundtrip encrypt/decrypt/rewrap.
  Run `go test ./secret/... -count=1` — passes without Vault (vault tests skip).

- [ ] T007 [S] Implement `middleware/redact/redact.go` (FR-007/008):
  - `Message(m proto.Message) proto.Message` — proto.Clone + `walkAndRedact`.
  - `walkAndRedact(msg protoreflect.Message)` — iterates fields; for `MessageKind`, recurse;
    for fields with `E_Field.secret == true`: set string fields to `"[REDACTED]"`, clear others.
  - `UnaryServerInterceptor() grpc.UnaryServerInterceptor` — calls `redact.Message` on request
    (if it implements `proto.Message`) before logging; calls `redact.Message` on response
    before logging. The real req/resp passed to the handler are untouched.
    For now, log via `slog.Debug` (stdlib); the caller can replace the logger.
  Import: `google.golang.org/protobuf/proto`, `google.golang.org/protobuf/reflect/protoreflect`,
  `github.com/infobloxopen/apis/proto/infoblox/authz/v1`.
  Run T003 tests — all must pass.

- [ ] T008 [S] Add `AssertNoSecretFieldsLeaked(responses ...proto.Message) []Finding` to
  `seccheck/seccheck.go` (FR-009):
  Walk each response via `proto.ProtoReflect().Range`; for each field with `secret: true`,
  if string field is non-empty (and != `"[REDACTED]"`) → Error finding with field path.
  Run T004 tests — all must pass.

---

## Phase 4: protoc-gen-storage secret field support

- [ ] T009 [S] Update `cmd/protoc-gen-storage/render.go` to detect `secret: true` fields
  (FR-006). In the field-rendering loop, check
  `proto.HasExtension(field.Options(), authzv1.E_Field)` + `fieldRule.Secret`. If true:
  - Emit `<GoName>Hash string \`gorm:"column:<snake>_hash;index"\`` instead of the plain column.
  - Emit `<GoName>Cipher string \`gorm:"column:<snake>_cipher"\``.
  - In `toModel_<Resource>`: skip assigning the plain field.
  - In `fromModel_<Resource>`: skip assigning from model (never return stored value).
  - In `Create` template: emit the hash+encrypt block; `enc` is a field on the repository struct.
  - Change constructor template to `func New<Resource>Repository(db *gorm.DB, enc secret.Encryptor)`.
  Update `cmd/protoc-gen-storage/render_test.go` to add a test case: a field with `secret: true`
  must produce `Hash` + `Cipher` columns, not the plain column, and the constructor must accept `enc`.

---

## Phase 5: Test fixture + apx release

- [ ] T010 [S] Create `testdata/apikey/` fixture:
  - `testdata/apikey/apikey.proto`: package `apikey.v1`; `APIKey` message with
    `string id = 1`, `string name = 2`, `string key_value = 3 [(infoblox.authz.v1.field).secret = true]`;
    `APIKeyService` with `CreateAPIKey`/`GetAPIKey`/`ListAPIKeys` with authz annotations.
  - `testdata/apikey/go.mod`: mirrors toy's pattern (replace devedge-sdk → `../..`, has gorm).
  - `testdata/apikey/buf.gen.yaml`: runs protoc-gen-go + protoc-gen-svc + protoc-gen-storage + protoc-gen-devedge-authz.
  - Run `buf generate --template testdata/apikey/buf.gen.yaml` (or equivalent).
  - `testdata/apikey/apikeyv1/apikey_test.go`: (a) compile check; (b) create APIKey with
    `NewAPIKeyRepository(db, secret.NewDev(key))` using an in-memory gorm (sqlite or just
    verify the generated code compiles without a real DB).
  Run `cd testdata/apikey && go build ./... && go test ./... -count=1`.

- [ ] T011 [S] Update the canonical `authz.proto` in `infobloxopen/apis` and cut `alpha.3`:
  - Clone/update `~/go/src/github.com/infobloxopen/apis` with the same `FieldRule` addition.
  - Open PR, merge, let CI finalize cut the tag.
  - `go get github.com/infobloxopen/apis/proto/infoblox/authz@v1.0.0-alpha.3 && go mod tidy`.
  - Update the proto mirror in `proto/infoblox/authz/v1/authz.proto` if not already matching.
  Run `go build ./... && make test`.

---

## Phase 6: Verify + commit

- [ ] T012 [S] `go build ./... && go vet ./... && make test` — clean (SC-006).
  `grep "gorm" go.mod` → absent (GORM stays in testdata/apikey/go.mod only).

- [ ] T013 [S] `cd testdata/apikey && go build ./... && go test ./... -count=1` — passes (SC-004).

- [ ] T014 [S] Commit all + merge.
  Message: `013: secret field annotation — encrypt at rest, log redaction, Vault Transit support`.

---

## Dependencies & Execution Order

- T001 (proto update, needed by T005-T008 for E_Field extension) → T002, T003, T004 (red tests)
- T002 → T005 (green dev tests)
- T003 → T007 (green redact tests)
- T004 → T008 (green seccheck tests)
- T005 → T006 (Vault reuses Encryptor interface)
- T005 + T006 → T009 (storage codegen uses Encryptor)
- T007 + T008 + T009 → T010 (fixture exercises all)
- T010 → T011 (apx release + go.mod bump)
- T011 → T012 → T013 → T014

## Complexity Tags

| Task | Tag | Reason |
|------|-----|--------|
| T001 | [S] | Mechanical proto edit + buf regenerate |
| T002 | [S] | Unit tests, pure crypto logic |
| T003 | [S] | Unit tests with synthetic proto fixture |
| T004 | [S] | Unit tests for seccheck assertion |
| T005 | [S] | AES-GCM + HMAC-SHA256 (~50 LOC stdlib) |
| T006 | [S] | HTTP JSON calls to Vault API (~80 LOC) |
| T007 | [S] | Proto reflection walk (~60 LOC; pattern from authzpb) |
| T008 | [S] | Proto reflection walk + Finding emit (~30 LOC) |
| T009 | [S] | Template conditional in render.go (~40 LOC addition) |
| T010 | [S] | New fixture, mirrors toy pattern exactly |
| T011 | [S] | apx PR + merge + go get; same flow as W1-2 |
| T012 | [S] | Run commands |
| T013 | [S] | Run testdata build |
| T014 | [S] | Git commit |
