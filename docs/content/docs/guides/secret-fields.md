---
title: Secret Fields
weight: 3
---

A field marked `secret` in proto is never stored as plaintext and never returned after creation.
This guide follows the full lifecycle: proto annotation → generated columns → encryptor → Vault.

## 1. Annotate the field

```proto
message APIKey {
  string id         = 1;
  string name       = 2;
  string account_id = 3;
  // key_value is raw API key material — hashed for lookup, encrypted for recovery.
  string key_value  = 4 [(infoblox.authz.v1.field).secret = true];
  string key_prefix = 5; // first 8 chars, for display — NOT secret
}
```

## 2. Generated storage: hash + cipher columns, no plaintext

`protoc-gen-storage` does **not** emit a `key_value` column. Instead it emits two columns and
omits the plaintext from the model entirely:

```go
type APIKeyModel struct {
    ID             string `gorm:"primaryKey;type:varchar(36)"`
    Name           string `gorm:"column:name"`
    AccountId      string `gorm:"column:account_id"`
    KeyValueHash   string `gorm:"column:key_value_hash;index"`   // for lookup
    KeyValueCipher string `gorm:"column:key_value_cipher"`        // for recovery
    KeyPrefix      string `gorm:"column:key_prefix"`
    ETag           string `gorm:"column:etag"`
    CreatedAt      time.Time
    UpdatedAt      time.Time
    DeletedAt      gorm.DeletedAt `gorm:"index"`
}
```

The `toModel` / `fromModel` helpers **skip** secret fields, so the plaintext can never round-trip
through the model. Encryption happens explicitly in `Create` and `Update`:

```go
func (r *APIKeyRepository) Create(ctx context.Context, entity *APIKey) (*APIKey, error) {
    m := toModel_APIKey(entity)
    if entity.KeyValue != "" {
        h, err := r.enc.Hash(ctx, entity.KeyValue)
        if err != nil { return nil, fmt.Errorf("hash key_value: %w", err) }
        c, err := r.enc.Encrypt(ctx, entity.KeyValue)
        if err != nil { return nil, fmt.Errorf("encrypt key_value: %w", err) }
        m.KeyValueHash = h
        m.KeyValueCipher = c
    }
    if err := r.db.WithContext(ctx).Create(m).Error; err != nil {
        return nil, fmt.Errorf("create APIKey: %w", err)
    }
    return fromModel_APIKey(m), nil // fromModel never sets KeyValue
}
```

Because a `secret` field means the repository needs an `Encryptor`, the generated constructor
takes one:

```go
func NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor) *APIKeyRepository
```

### Lookup by hash

Since the plaintext is gone, looking up a key by its raw value uses the deterministic hash. The
generator emits a `LookupBy<Field>Hash` method for each secret field:

```go
// hash the presented key, then look it up — tenant-scoped.
h, _ := enc.Hash(ctx, presentedKey)
key, err := repo.LookupByKeyValueHash(ctx, h)
```

## 3. Choose an encryptor

Both implementations satisfy the same one interface:

```go
type Encryptor interface {
    Encrypt(ctx context.Context, plaintext string) (ciphertext string, err error)
    Decrypt(ctx context.Context, ciphertext string) (plaintext string, err error)
    Hash(ctx context.Context, plaintext string) (hash string, err error)
}
```

### Dev mode — AES-256-GCM (no external service)

```go
import "github.com/infobloxopen/devedge-sdk/secret"

enc := secret.NewDev(devKey) // devKey must be >= 32 bytes, else it panics
repo := NewAPIKeyRepository(db, enc)
```

`NewDev` uses AES-256-GCM for encrypt/decrypt and HMAC-SHA256 for the lookup hash. Perfect for
local development and tests; the key lives in process. **Do not use it in production.**

### Production — Vault Transit

```go
import "github.com/infobloxopen/devedge-sdk/secret"

enc := secret.NewVaultTransit(
    "http://vault:8200", // Vault address
    vaultToken,           // token with encrypt/decrypt policy on the key
    "apikey",             // Transit key name (must already exist in Vault)
)
repo := NewAPIKeyRepository(db, enc)
```

`VaultTransitEncryptor` calls Vault's Transit Secrets Engine over plain HTTP — **no Vault SDK
dependency**. Encrypt/decrypt round-trip through Vault; the lookup hash is computed locally with
HMAC keyed on `sha256(token)`, so it is stable without a Vault round-trip. It also exposes
`Rewrap` to re-encrypt ciphertext under the latest key version without revealing plaintext. See
[Vault Transit](../vault-transit/).

## 4. It's also redacted in logs and checked for leaks

The `secret` annotation does more than storage:

- **Logs** — `middleware/redact` replaces the value with `[REDACTED]` before logging
  (`redact.Message(m)` or the `redact.UnaryServerInterceptor`).
- **Responses** — `seccheck.AssertNoSecretFieldsLeaked(resp...)` walks every response proto and
  fails if a secret field holds any value other than the literal `[REDACTED]`. Wire it into your
  tests — see [Security check](../security-check/).

{{< callout type="warning" >}}
The encryptor abstraction means *the framework never sees a plaintext column*, but your handler
still receives the raw value on the **request**. Return it to the caller **once**, at creation
time if at all — never on `Get`/`List`. `AssertNoSecretFieldsLeaked` is your safety net.
{{< /callout >}}
