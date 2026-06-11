---
title: secret
weight: 3
---

```go
import "github.com/infobloxopen/devedge-sdk/secret"
```

Package `secret` provides encrypt, decrypt, and hash operations for secret fields. It is the seam
behind the `(infoblox.authz.v1.field).secret` annotation: generated storage code calls an
`Encryptor` to hash and encrypt secret fields and never stores plaintext.

## Encryptor

```go
type Encryptor interface {
    Encrypt(ctx context.Context, plaintext string) (ciphertext string, err error)
    Decrypt(ctx context.Context, ciphertext string) (plaintext string, err error)
    Hash(ctx context.Context, plaintext string) (hash string, err error)
}
```

Three operations:

- **`Encrypt` / `Decrypt`** — recoverable, for storing and retrieving the value (the `_cipher`
  column).
- **`Hash`** — deterministic and one-way, for indexed lookups by value (the `_hash` column). The
  same plaintext always yields the same hash, so `LookupBy<Field>Hash` can find a record without
  the plaintext.

Both shipped implementations satisfy this interface, so they are interchangeable with no other
code change.

## NewDev — AES-256-GCM (development)

```go
func NewDev(key []byte) Encryptor
```
Returns a dev-suitable `Encryptor` using **AES-256-GCM** for encrypt/decrypt and **HMAC-SHA256**
for hash. **Panics if `len(key) < 32`** (the key is truncated/copied to 32 bytes). The key lives
in process — fine for local dev and tests, **not** for production.

```go
enc := secret.NewDev(devKey) // devKey must be >= 32 bytes
```

Implementation notes: `Encrypt` generates a fresh random GCM nonce per call and prepends it to the
ciphertext, base64-encoding the result; `Decrypt` reverses it; `Hash` is `base64(HMAC-SHA256(key,
plaintext))`.

## NewVaultTransit — HashiCorp Vault (production)

```go
func NewVaultTransit(addr, token, keyName string) *VaultTransitEncryptor
```
Returns an `Encryptor` backed by Vault's Transit Secrets Engine over plain HTTP — **no Vault SDK
dependency**.

| Arg | Meaning |
|---|---|
| `addr` | Vault server address, e.g. `http://localhost:8200` |
| `token` | a Vault token with encrypt/decrypt policy on `keyName` |
| `keyName` | the Transit key name — **must already exist** in Vault |

```go
type VaultTransitEncryptor struct { /* unexported */ }

func (v *VaultTransitEncryptor) Encrypt(ctx context.Context, plaintext string) (string, error)
func (v *VaultTransitEncryptor) Decrypt(ctx context.Context, ciphertext string) (string, error)
func (v *VaultTransitEncryptor) Hash(ctx context.Context, plaintext string) (string, error)
func (v *VaultTransitEncryptor) Rewrap(ctx context.Context, ciphertext string) (string, error)
```

- `Encrypt` → `POST /v1/transit/encrypt/<keyName>`; `Decrypt` → `POST /v1/transit/decrypt/<keyName>`.
- `Hash` is computed **locally** (HMAC-SHA256 keyed on `sha256(token)`) so lookups need no Vault
  round-trip and stay deterministic.
- `Rewrap` (`POST /v1/transit/rewrap/<keyName>`) re-encrypts existing ciphertext under the latest
  key version **without revealing plaintext** — use it for key rotation.

Each request sets `X-Vault-Token` and `Content-Type: application/json`; a non-200 response becomes
an error carrying Vault's status and body.

See the [Vault Transit guide](../../guides/vault-transit/) for engine setup and policy.

## How storage uses it

`protoc-gen-storage` emits `<field>_hash` and `<field>_cipher` columns for each secret field and
calls the `Encryptor` in `Create`/`Update`:

```go
h, _ := enc.Hash(ctx, entity.KeyValue)    // → KeyValueHash  (indexed, for lookup)
c, _ := enc.Encrypt(ctx, entity.KeyValue) // → KeyValueCipher (recoverable)
```

The `Repository` constructor takes the `Encryptor` when the message has secret fields. See
[Secret fields](../../guides/secret-fields/).
