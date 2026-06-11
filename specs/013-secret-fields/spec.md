# Feature Specification: Secret field annotation, encryption at rest, and log redaction

**Feature Branch**: `013-secret-fields`
**Created**: 2026-06-11
**Status**: Draft

## Context

The API Key Manager service (coming in F014) needs to store sensitive values — API key
material — without ever persisting plaintext, without ever returning the raw value after
initial creation, and without ever letting the value appear in logs. These are not
application-layer concerns the developer should have to remember; they are framework-level
invariants the annotation contract should enforce.

This feature adds the mechanism:

1. **`(infoblox.authz.v1.field).secret = true`** — a proto field annotation declaring that
   a field's value is sensitive. The framework uses it in four places: storage (encrypt/hash),
   logging (redact), responses (security-check gate), and Vault key rotation.
2. **`secret/` package** — a pluggable `Encryptor` interface with two implementations:
   - `Dev` — AES-256-GCM with a local key (stdlib only; no external deps; suitable for
     development and testing).
   - `VaultTransit` — HashiCorp Vault Transit Secrets Engine (HTTP calls only; no Vault
     SDK dependency; optional — services that don't configure it default to `Dev`).
3. **Updated `protoc-gen-storage`** — generates hash + ciphertext columns for `secret: true`
   fields; the generated `Create`/`Update` methods call the `Encryptor` injected at repository
   construction time.
4. **`middleware/redact`** — a proto-reflection-based helper that replaces `secret: true`
   field values with `"[REDACTED]"` before logging; shipped as both a standalone function and
   a gRPC unary interceptor wrapper.
5. **`seccheck.AssertNoSecretFieldsLeaked`** — verifies that Get and List responses do not
   contain the raw value of a `secret: true` field.
6. **Updated canonical `authz.proto`** — adds `FieldRule` message + `field` extension on
   `google.protobuf.FieldOptions` (extension number 50002); released via apx to both
   `infobloxopen/apis` and `Infoblox-CTO/apis`.

## Clarifications

- **Extension number 50002**: used for the new field-level extension alongside the existing
  method-level `rule = 50001`. Both are in the "internal use" range pending formal registration.
- **`Encryptor` contract**: `Encrypt` is reversible (needed if a service ever needs to
  display/rotate secrets). `Hash` is one-way and stable — suitable for equality lookup
  (e.g., validating a submitted API key by hashing and comparing). A service that only needs
  validation (never recovery) may store only the hash; a service needing recovery (e.g., a
  webhook secret that must be delivered as an `Authorization` header) stores ciphertext.
- **`Dev` encryptor key**: derived from a caller-supplied `[]byte` (minimum 32 bytes).
  `NewDev(key)` — panics on short key, not an error, because a misconfigured dev encryptor is
  a programming error not a runtime condition.
- **Vault Transit — no SDK dep**: the Vault integration makes plain HTTP calls to
  `POST /v1/transit/encrypt/<keyName>` and `POST /v1/transit/decrypt/<keyName>`. The only
  dep is `net/http` (stdlib). `NewVaultTransit(addr, token, keyName string) Encryptor`.
- **Vault key rotation**: `VaultTransitEncryptor` also implements `Rewrap(ctx, ciphertext)
  string` — calls `POST /v1/transit/rewrap/<keyName>`. This is not on the `Encryptor`
  interface (it's a Vault-specific operation) but available via type assertion.
- **Hash algorithm**: HMAC-SHA256 with the same key used for encryption (for `Dev`) or with
  a caller-supplied HMAC key (for `VaultTransit`, since hashing is done locally). The hash is
  stable — same plaintext + same key always produces the same hash.
- **`protoc-gen-storage` storage model**: for a `secret: true` field named `key_value`,
  generate two columns: `KeyValueHash string` (HMAC-SHA256, indexed) and
  `KeyValueCipher string` (ciphertext from `Encrypt`, empty if hash-only). The proto field
  `key_value` itself is NOT stored in the DB. The repository constructor gains an
  `Encryptor` parameter: `NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor)`.
- **Log redaction**: `redact.Message(m proto.Message) proto.Message` clones the message and
  replaces any field with `(infoblox.authz.v1.field).secret = true` with `"[REDACTED]"` (for
  string fields) or the zero value (for other types). The `redact.UnaryServerInterceptor()`
  wraps handlers and redacts both the request and response before passing them to any logger.
  Redaction uses proto reflection — no codegen required.
- **`AssertNoSecretFieldsLeaked`**: calls Get and List RPCs and inspects the proto response
  via reflection; if any `secret: true` field is non-empty (and non-`"[REDACTED]"`), emits
  an Error finding. Callers provide the populated Get/List responses; the assertion does the
  reflection walk.
- **New canonical release**: the `authz.proto` mirror in `proto/infoblox/authz/v1/authz.proto`
  is updated first; then the canonical repo (`infobloxopen/apis`) is updated via PR + apx
  release; devedge-sdk's `go.mod` is bumped to consume the new alpha release. This follows
  the same flow as the W1-2 annotation contract release.
- **Existing toy fixture**: `Widget` is not changed. A new `APIKey` message is added to
  `testdata/toy/` to exercise secret field codegen (or the secret field is added to a separate
  `testdata/apikey/` fixture with its own `go.mod`). The existing Widget tests must pass
  unchanged.

## User Scenarios & Testing

### User Story 1 — Secret field never stored as plaintext (P1) 🎯 MVP

A developer annotates `string key_value = 3 [(infoblox.authz.v1.field).secret = true]` on
their proto resource. The generated repository's `Create` method hashes and optionally
encrypts the field; the GORM model has no `key_value` column.

**Acceptance Scenarios**:

1. **Given** a repository constructed with a `Dev` encryptor, **When** `Create` is called
   with `key_value = "sk_live_abc123"`, **Then** the stored `WidgetModel` has a non-empty
   `KeyValueHash` and empty `KeyValueCipher` (dev mode is hash-only by default), and
   `key_value` in the returned proto is cleared.
2. **Given** a repository with a `VaultTransit` encryptor, **When** `Create` is called,
   **Then** `KeyValueCipher` is a `vault:v1:...` ciphertext and `KeyValueHash` matches
   `HMAC-SHA256(hmacKey, "sk_live_abc123")`.
3. **Given** the stored row, **When** the DB is queried directly, **Then** no `key_value`
   column exists.

**Independent Test**: unit test with `Dev` encryptor + `MemoryRepository` equivalent;
integration test with real Vault skipped unless `VAULT_ADDR` env var is set.

---

### User Story 2 — Secret field redacted in logs (P1)

A developer adds `redact.UnaryServerInterceptor()` to their server chain (or the framework
wires it automatically). Any gRPC handler that receives a request or returns a response
containing a `secret: true` field logs `"[REDACTED]"`, never the raw value.

**Acceptance Scenarios**:

1. **Given** a proto message with `key_value = "sk_live_abc123"` and `secret: true`,
   **When** `redact.Message(m)` is called, **Then** the returned message has
   `key_value = "[REDACTED]"` and all other fields unchanged.
2. **Given** a message with no `secret: true` fields, **When** `redact.Message(m)` is called,
   **Then** the message is returned unchanged.
3. **Given** a nested message where an inner field has `secret: true`, **When** `redact.Message`
   is called on the outer, **Then** the inner field is redacted.

**Independent Test**: unit test of `redact.Message` with a synthetic proto message using the
`FieldRule` annotation.

---

### User Story 3 — Security-check catches secret field in response (P1)

A developer accidentally returns the raw `key_value` in a `GetAPIKey` response. Running
`seccheck.AssertNoSecretFieldsLeaked` against the live service catches it.

**Acceptance Scenarios**:

1. **Given** a Get response with `key_value = "sk_live_abc123"` (non-empty, non-redacted),
   **When** `AssertNoSecretFieldsLeaked(responses...)` is called, **Then** an Error finding
   is returned naming the field.
2. **Given** a Get response with `key_value = ""` (cleared by handler), **When** the assertion
   runs, **Then** zero findings.
3. **Given** a List response where each item has `key_value = ""`, **When** the assertion
   runs, **Then** zero findings.

**Independent Test**: unit test of `AssertNoSecretFieldsLeaked` with synthetic proto messages.

---

### User Story 4 — Vault Transit: roundtrip encrypt/decrypt (P2)

A developer configures `secret.NewVaultTransit(addr, token, keyName)` and the repository
stores ciphertext, which can be decrypted back to plaintext.

**Acceptance Scenarios**:

1. **Given** a `VaultTransit` encryptor with a live Vault at `$VAULT_ADDR`, **When**
   `Encrypt(ctx, "hello")` is called, **Then** the result starts with `"vault:v1:"`.
2. **Given** the ciphertext from scenario 1, **When** `Decrypt(ctx, ciphertext)` is called,
   **Then** the result is `"hello"`.
3. **Given** a `VaultTransit` encryptor and a ciphertext encrypted with key version 1,
   **When** `Rewrap(ctx, ciphertext)` is called after a key rotation, **Then** the result
   is a valid ciphertext under the latest key version.

**Independent Test**: integration test guarded by `VAULT_ADDR` env var; skipped otherwise.

---

### Edge Cases

- What if `Encryptor.Encrypt` returns an error? → `Create`/`Update` return the error; no
  partial write.
- What if Vault is unreachable? → `VaultTransit.Encrypt` returns an error after one attempt
  (no retry — let the caller decide retry policy); `Create` propagates it.
- What if the HMAC key is shorter than 32 bytes? → `NewDev` panics with a clear message.
- What if a `secret: true` field is an `int32` or `bool`? → `redact.Message` sets it to zero
  value; `AssertNoSecretFieldsLeaked` checks for non-zero value as the leak signal.
- What if the proto message has no `secret: true` fields? → `redact.Message` is a no-op
  (returns a clone); `AssertNoSecretFieldsLeaked` returns zero findings immediately.

## Requirements

### Functional Requirements

- **FR-001**: `proto/infoblox/authz/v1/authz.proto` MUST add a `FieldRule` message
  (`bool secret = 1`) and `extend google.protobuf.FieldOptions { FieldRule field = 50002; }`.
- **FR-002**: `secret.Encryptor` interface MUST declare `Encrypt(ctx, plaintext string)
  (ciphertext string, err error)`, `Decrypt(ctx, ciphertext string) (plaintext string, err
  error)`, and `Hash(ctx, plaintext string) (hash string, err error)`.
- **FR-003**: `secret.NewDev(key []byte) Encryptor` MUST implement `Encryptor` using
  AES-256-GCM for `Encrypt`/`Decrypt` (stdlib `crypto/aes` + `crypto/cipher`) and
  HMAC-SHA256 for `Hash` (stdlib `crypto/hmac` + `crypto/sha256`). MUST panic if
  `len(key) < 32`.
- **FR-004**: `secret.NewVaultTransit(addr, token, keyName string) Encryptor` MUST implement
  `Encryptor` using HTTP calls to the Vault Transit API (no Vault SDK dep). `Encrypt` calls
  `POST {addr}/v1/transit/encrypt/{keyName}` with base64-encoded plaintext and returns the
  `data.ciphertext` value. `Decrypt` calls `POST .../decrypt/...`. `Hash` uses local
  HMAC-SHA256 with `token` as the HMAC key (token is secret but stable per environment).
- **FR-005**: `(*VaultTransitEncryptor).Rewrap(ctx context.Context, ciphertext string)
  (string, error)` MUST call `POST .../rewrap/{keyName}` with the old ciphertext and return
  the new ciphertext under the current key version.
- **FR-006**: `protoc-gen-storage` MUST detect fields with `(infoblox.authz.v1.field).secret
  = true` and generate: (a) `<FieldName>Hash string` GORM column (indexed); (b)
  `<FieldName>Cipher string` GORM column. The original `<FieldName>` column MUST NOT be
  generated. The generated repository constructor MUST accept `enc secret.Encryptor` as a
  parameter. `Create` and `Update` MUST call `enc.Hash` and `enc.Encrypt` for each secret
  field; the proto field MUST be cleared in the returned entity.
- **FR-007**: `redact.Message(m proto.Message) proto.Message` MUST clone `m` and replace
  every field where `proto.HasExtension(field.Options(), authzv1.E_Field)` and
  `fieldRule.Secret == true` with `"[REDACTED]"` (string) or zero value (other kinds). MUST
  walk nested messages recursively.
- **FR-008**: `redact.UnaryServerInterceptor() grpc.UnaryServerInterceptor` MUST call
  `redact.Message` on both the incoming request and the outgoing response. The redacted
  copies are for logging only; the real request/response to the handler are unchanged.
- **FR-009**: `seccheck.AssertNoSecretFieldsLeaked(responses ...proto.Message) []Finding`
  MUST walk each response via proto reflection; for each field with `secret: true`, if the
  field value is non-empty (string) or non-zero (other), emit an Error finding naming the
  field path.
- **FR-010**: Vault integration tests MUST be guarded by `os.Getenv("VAULT_ADDR") != ""`
  and skipped otherwise. `go test ./secret/... -count=1` MUST pass without a running Vault.
- **FR-011**: The canonical `authz.proto` in `infobloxopen/apis` MUST be updated and a new
  `v1.0.0-alpha.3` release cut via the apx pipeline. `devedge-sdk/go.mod` MUST be updated
  to `github.com/infobloxopen/apis/proto/infoblox/authz@v1.0.0-alpha.3`.

### Key Entities

- **`secret.Encryptor`**: interface — `Encrypt`, `Decrypt`, `Hash`.
- **`secret.DevEncryptor`**: AES-256-GCM + HMAC-SHA256, stdlib only.
- **`secret.VaultTransitEncryptor`**: HTTP-based Vault Transit client; also has `Rewrap`.
- **`FieldRule`**: new proto message `{bool secret = 1}` on field options.

## Success Criteria

- **SC-001**: `redact.Message` on a proto message with a `secret: true` string field returns
  the field as `"[REDACTED]"` (unit test).
- **SC-002**: `secret.NewDev(key).Encrypt` + `Decrypt` roundtrip produces the original
  plaintext (unit test).
- **SC-003**: `secret.NewDev(key).Hash` is stable — same inputs always produce the same hash
  (unit test).
- **SC-004**: `protoc-gen-storage` generates `KeyValueHash` + `KeyValueCipher` columns (not
  `KeyValue`) for a `secret: true` field; generated code compiles (unit test of template +
  `go build`).
- **SC-005**: `seccheck.AssertNoSecretFieldsLeaked` returns an Error finding when a response
  contains a non-empty `secret: true` field (unit test).
- **SC-006**: `go build ./... && go vet ./... && make test` clean; no new external deps except
  the bumped `infobloxopen/apis` module.
- **SC-007**: Vault integration test passes when `VAULT_ADDR` is set, skipped otherwise.
