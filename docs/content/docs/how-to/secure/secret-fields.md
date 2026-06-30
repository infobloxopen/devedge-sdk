---
title: Secret Fields
weight: 2
aliases:
  - /docs/guides/vault-transit/
---

A secret field is a proto field annotated with `(infoblox.field.v1.opts).secret = true`. The SDK
ensures that the field value is never stored as plaintext and never returned in a response after
creation. Use secret fields for credentials, tokens, and other values that must remain
confidential throughout their lifecycle.

For the conceptual security model see [Security Posture](../../explanation/security-posture/).

## Declaring a secret field

```proto
string key_value = 4 [(infoblox.field.v1.opts) = {secret: true}];
```

## Storage

A secret field is stored as a hash and a cipher, never as plaintext. `protoc-gen-storage` does not
emit a plaintext column. It emits two columns:

- a deterministic **hash** column (for indexed lookup), and
- a recoverable **cipher** column.

The `toModel`/`fromModel` helpers skip secret fields entirely — plaintext cannot round-trip through
the model. Encryption happens explicitly in `Create`/`Update`:

```go
type APIKeyModel struct {
    ID             string `gorm:"primaryKey;type:varchar(36)"`
    // ...
    KeyValueHash   string `gorm:"column:key_value_hash;index"` // for lookup
    KeyValueCipher string `gorm:"column:key_value_cipher"`      // for recovery
    // ...
}
```

Because the plaintext is not stored, look a record up by its raw value through the generated
`LookupBy<Field>Hash` method (tenant-scoped):

```go
h, _ := enc.Hash(ctx, presentedKey)
key, err := repo.LookupByKeyValueHash(ctx, h)
```

The generated repository constructor requires an `Encryptor` when the message has secret fields:

```go
func NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor) *APIKeyRepository
```

## Logging and responses

The `secret` annotation applies beyond storage:

- **Logs** — `middleware/redact` replaces the value with `[REDACTED]` before logging.
- **Responses** — `seccheck.AssertNoSecretFieldsLeaked(resp...)` walks every response proto and
  fails if a secret field holds anything but `[REDACTED]`. Wire it into your tests — see
  [Security Check](../security-check/).

{{< callout type="warning" >}}
**Your handler receives the raw value on the request.** Return it to the caller once at most, and
only at creation time if at all — never on `Get`/`List`. Returning it zero times is the preferred
posture. `AssertNoSecretFieldsLeaked` is your safety net.
{{< /callout >}}

## Choosing an encryptor

Both implementations satisfy one interface (`Encrypt` / `Decrypt` / `Hash`):

### Development — `secret.NewDev`

```go
enc := secret.NewDev(devKey) // devKey must be >= 32 bytes
```

Uses **AES-256-GCM** for encrypt/decrypt and **HMAC-SHA256** for hash. Runs entirely in-process —
ideal for local dev and tests.

{{< callout type="warning" >}}
**Do not use `secret.NewDev` in production.** It relies on an in-process key with no rotation
support.
{{< /callout >}}

### Production — Vault Transit

In production, secret fields are encrypted by **HashiCorp Vault's Transit Secrets Engine** rather
than an in-process key. Transit operates as encryption as a service: the plaintext is sent to
Vault, Vault returns ciphertext, and the encryption key never leaves Vault.

The SDK's `secret.NewVaultTransit` talks to Vault over plain HTTP — there is no Vault SDK
dependency in the SDK.

#### 1. Enable the Transit engine and create a key

```bash
# Enable the transit secrets engine (once per Vault).
vault secrets enable transit

# Create a named encryption key for this service's secret fields.
vault write -f transit/keys/apikey
```

The key name (`apikey` here) is what you pass to `NewVaultTransit`. **It must already exist** —
the encryptor does not create it.

#### 2. Grant a policy and issue a token

The token you give the service needs encrypt/decrypt (and, for rotation, rewrap) on that key:

```hcl {filename="apikey-policy.hcl"}
path "transit/encrypt/apikey" { capabilities = ["update"] }
path "transit/decrypt/apikey" { capabilities = ["update"] }
path "transit/rewrap/apikey"  { capabilities = ["update"] }
```

```bash
vault policy write apikey-encrypt apikey-policy.hcl
vault token create -policy=apikey-encrypt
```

#### 3. Construct the encryptor

```go
import "github.com/infobloxopen/devedge-sdk/secret"

enc := secret.NewVaultTransit(
    os.Getenv("VAULT_ADDR"),   // e.g. "http://vault:8200"
    os.Getenv("VAULT_TOKEN"),  // token with the policy above
    "apikey",                  // Transit key name (must already exist)
)

repo := apikeyv1.NewAPIKeyRepository(db, enc) // same constructor as dev mode
```

`NewVaultTransit` returns a `*VaultTransitEncryptor` that satisfies the `secret.Encryptor`
interface, so swapping it for `secret.NewDev(key)` (or vice versa) requires no other change.

#### Operation mapping

| `Encryptor` method | Vault call | Notes |
|---|---|---|
| `Encrypt` | `POST /v1/transit/encrypt/<key>` | base64-encodes the plaintext, returns Vault's `vault:v1:...` ciphertext |
| `Decrypt` | `POST /v1/transit/decrypt/<key>` | base64-decodes Vault's plaintext back to the string |
| `Hash` | *(local)* | HMAC-SHA256 keyed on `sha256(token)` — computed locally so lookups need no Vault round-trip |
| `Rewrap` | `POST /v1/transit/rewrap/<key>` | re-encrypts existing ciphertext under the latest key version without revealing plaintext |

Every HTTP call sets `X-Vault-Token` and `Content-Type: application/json`, and a non-200 response
becomes an error carrying Vault's status and body.

#### Local hash

The lookup hash is computed locally using HMAC-SHA256 keyed on `sha256(token)`. Because the hash
is deterministic, the same plaintext always maps to the same hash value, which supports indexed
lookups without a Vault round-trip. The confidentiality of the field still depends on Vault: the
recoverable ciphertext is what Transit produces.

#### Key rotation

When you rotate the Transit key in Vault (`vault write -f transit/keys/apikey/rotate`), existing
ciphertext stays decryptable (Transit keeps prior versions). You can re-encrypt existing ciphertext
under the newest key version without exposing the plaintext:

```go
newCipher, err := enc.Rewrap(ctx, oldCipher)
// store newCipher; the plaintext was never exposed to the service
```

{{< callout type="info" >}}
Pair key rotation with the SDK's **DSN hotload** convention (`fsnotify://<driver>/<abs-path>`) so
rotated database credentials reload without a restart — see
[Storage Shapes](../model-and-persist/storage-shapes/).
{{< /callout >}}

## Local development

You do not need Vault for local dev or tests — use `secret.NewDev(key)` (AES-256-GCM) instead.
Switch to `NewVaultTransit` only where a real Vault is available; nothing else in the service
changes.

## Reference

The full `Encryptor` API is documented in the [secret reference](../../../reference/secret/).
