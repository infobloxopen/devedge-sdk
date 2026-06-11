# Implementation Plan: Secret field annotation, encryption at rest, log redaction

**Branch**: `013-secret-fields` | **Date**: 2026-06-11 | **Spec**: `spec.md`

## Summary

Five independent units ship together: (1) updated `authz.proto` with `FieldRule` + apx
release, (2) `secret/` package with `Dev` + `VaultTransit` encryptors, (3) updated
`protoc-gen-storage` generating hash+cipher columns, (4) `middleware/redact` for log
redaction, (5) `seccheck.AssertNoSecretFieldsLeaked`. A new `testdata/apikey/` fixture
(separate go.mod) exercises the secret field codegen end-to-end.

## Technical Context

**Language/Version**: Go 1.25.5
**New deps**: none for the core SDK; `testdata/apikey/go.mod` adds gorm (same pattern as toy)
**Vault**: HTTP calls via stdlib `net/http`; no `github.com/hashicorp/vault-client-go`
**Testing**: unit tests pass without Vault; Vault integration tests gated by `VAULT_ADDR`

## Constitution Check

| Principle | Status |
|-----------|--------|
| **Clean core** — no ORM/policy-engine in core | ✅ secret/ is stdlib-only; GORM stays in testdata go.mod |
| **Pluggable with dev-suitable defaults** | ✅ Dev encryptor works out of the box; Vault is opt-in |
| **Fail closed** | ✅ missing Encryptor → Create returns error (no silent plaintext storage) |

## Project Structure

```
proto/infoblox/authz/v1/authz.proto   MODIFY — add FieldRule + field extension
buf.gen.yaml                           MODIFY — regenerate authzpb test fixture

secret/
├── encryptor.go       NEW — Encryptor interface
├── dev.go             NEW — DevEncryptor (AES-256-GCM + HMAC-SHA256)
├── dev_test.go        NEW — unit tests (no external deps)
├── vault.go           NEW — VaultTransitEncryptor (HTTP only)
└── vault_test.go      NEW — integration tests (gated by VAULT_ADDR)

middleware/redact/
├── redact.go          NEW — Message(), UnaryServerInterceptor()
└── redact_test.go     NEW — unit tests using synthetic proto with FieldRule

seccheck/
├── seccheck.go        MODIFY — add AssertNoSecretFieldsLeaked
└── seccheck_test.go   MODIFY — add tests for new assertion

cmd/protoc-gen-storage/
├── render.go          MODIFY — detect secret: true, emit Hash+Cipher columns
└── render_test.go     MODIFY — add test for secret field template output

testdata/apikey/       NEW (separate go.mod, mirrors toy pattern)
├── go.mod
├── apikey.proto
├── buf.gen.yaml
└── apikeyv1/
    ├── apikey.pb.go        (generated)
    ├── apikey.storage.go   (generated — exercises secret field template)
    └── apikey_test.go      NEW — compile check + Create/hash assertion

go.mod / go.sum        MODIFY — bump infobloxopen/apis to alpha.3 after apx release
```

## Architecture Decisions

### 1. `secret.Encryptor` interface

```go
type Encryptor interface {
    Encrypt(ctx context.Context, plaintext string) (ciphertext string, err error)
    Decrypt(ctx context.Context, ciphertext string) (plaintext string, err error)
    Hash(ctx context.Context, plaintext string) (hash string, err error)
}
```

`Dev`: AES-256-GCM with random nonce prepended to ciphertext; base64-encoded output.
`Hash`: `base64(HMAC-SHA256(key, plaintext))` — same key as encryption for simplicity.

`VaultTransit`: JSON over HTTP.
- `Encrypt`: `{"plaintext": base64(plaintext)}` → `data.ciphertext`
- `Decrypt`: `{"ciphertext": ct}` → base64-decode `data.plaintext`
- `Hash`: local HMAC-SHA256 with the Vault token as HMAC key (stable per environment,
  never sent to Vault for hashing — hashing is always local).

### 2. `protoc-gen-storage` secret field template

For a field `string key_value = 3 [(infoblox.authz.v1.field).secret = true]`:

```go
// Generated GORM model:
KeyValueHash   string `gorm:"column:key_value_hash;index"`
KeyValueCipher string `gorm:"column:key_value_cipher"`

// Generated Create method (added block):
if entity.KeyValue != "" {
    h, err := r.enc.Hash(ctx, entity.KeyValue)
    if err != nil { return nil, fmt.Errorf("hash KeyValue: %w", err) }
    c, err := r.enc.Encrypt(ctx, entity.KeyValue)
    if err != nil { return nil, fmt.Errorf("encrypt KeyValue: %w", err) }
    m.KeyValueHash = h
    m.KeyValueCipher = c
    entity.KeyValue = "" // clear plaintext from returned entity
}
```

Repository constructor: `func NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor) *APIKeyRepository`.

### 3. `redact.Message` via proto reflection

```go
func Message(m proto.Message) proto.Message {
    clone := proto.Clone(m)
    walkAndRedact(clone.ProtoReflect())
    return clone
}

func walkAndRedact(msg protoreflect.Message) {
    msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
        if fd.Kind() == protoreflect.MessageKind {
            walkAndRedact(v.Message())
            return true
        }
        opts := fd.Options()
        if opts != nil && proto.HasExtension(opts, authzv1.E_Field) {
            rule := proto.GetExtension(opts, authzv1.E_Field).(*authzv1.FieldRule)
            if rule.GetSecret() {
                switch fd.Kind() {
                case protoreflect.StringKind:
                    msg.Set(fd, protoreflect.ValueOfString("[REDACTED]"))
                default:
                    msg.Clear(fd)
                }
            }
        }
        return true
    })
}
```

### 4. Vault HTTP client (no SDK)

```go
type vaultClient struct {
    addr    string
    token   string
    keyName string
    http    *http.Client
    hmacKey []byte // = sha256(token) — stable HMAC key derived from the Vault token
}

func (c *vaultClient) encrypt(ctx, plaintext) (string, error) {
    body := map[string]string{"plaintext": base64.StdEncoding.EncodeToString([]byte(plaintext))}
    // POST {addr}/v1/transit/encrypt/{keyName}
    // X-Vault-Token: {token}
    // parse response: data.ciphertext
}
```

### 5. apx release flow (FR-011)

Same flow as W1-2 and W3-4:
1. Update `proto/infoblox/authz/v1/authz.proto` (mirror + canonical repo)
2. Open PR on `infobloxopen/apis` with the updated proto + regenerated Go bindings
3. Merge → CI finalize cuts `v1.0.0-alpha.3` tag automatically
4. `go get github.com/infobloxopen/apis/proto/infoblox/authz@v1.0.0-alpha.3`

### 6. Tradeoffs

| Decision | Chosen | Rejected | Reason |
|----------|--------|----------|--------|
| Vault dep | HTTP only | `vault-client-go` | Heavy dep (~50 transitive); HTTP API is stable and simple |
| Hash key for Vault | `sha256(token)` | Separate HMAC key config | One less config param; token is already per-env; SHA-256 of token gives a stable 32-byte key |
| Secret field in proto response | Clear to `""` on Create/Update | Return masked value | Handler decides what to show; framework ensures plaintext never returns from storage layer |
| `redact.Message` target | Logging only (separate copy) | Mutate real request | Never modify the handler's request/response; redaction is for observability only |
