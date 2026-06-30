---
title: secret
weight: 3
---

```go
import "github.com/infobloxopen/devedge-sdk/secret"
```

Package `secret` provides encrypt, decrypt, and hash operations for secret fields. It implements
the `Encryptor` interface that generated storage code calls when a proto field carries the
`(infoblox.field.v1.opts).secret` annotation. The generated storage code calls the `Encryptor` to
hash and encrypt those fields and never stores their plaintext. Use this package when you need to
supply or swap the encryption backend for a service that stores secret fields.

## Encryptor

`Encryptor` is the interface that both built-in backends implement.

```go
type Encryptor interface {
    Encrypt(ctx context.Context, plaintext string) (ciphertext string, err error)
    Decrypt(ctx context.Context, ciphertext string) (plaintext string, err error)
    Hash(ctx context.Context, plaintext string) (hash string, err error)
}
```

The three methods serve distinct storage roles:

- **`Encrypt` / `Decrypt`** — reversible operations that write and read the `<field>_cipher` column.
- **`Hash`** — a deterministic one-way operation that writes the `<field>_hash` column. Because the
  same plaintext always produces the same hash, `LookupBy<Field>Hash` can locate a record without
  decrypting it.

Both built-in implementations satisfy `Encryptor`, so you can swap backends without changing any
other code.

## NewDev

```go
func NewDev(key []byte) Encryptor
```

Returns an `Encryptor` that uses AES-256-GCM for encrypt/decrypt and HMAC-SHA256 for hash. The key
is held in process memory and is copied to exactly 32 bytes: if you pass a longer key, only the
first 32 bytes are used.

{{< callout type="warning" >}}
**Do not use `NewDev` in production.** Use `NewVaultTransit` instead. `NewDev` panics if
`len(key) < 32`.
{{< /callout >}}

```go
enc := secret.NewDev(devKey) // devKey must be >= 32 bytes
```

**How it works:** `Encrypt` generates a fresh random GCM nonce on each call and prepends it to the
ciphertext, then base64-encodes the result. `Decrypt` reverses that process. `Hash` returns
`base64(HMAC-SHA256(key, plaintext))`.

## NewVaultTransit

```go
func NewVaultTransit(addr, token, keyName string) *VaultTransitEncryptor
```

Returns an `Encryptor` backed by HashiCorp Vault's Transit Secrets Engine. Use this backend in
production. The implementation uses plain HTTP with no Vault SDK dependency.

| Parameter | Description |
|---|---|
| `addr` | Vault server address, e.g. `http://localhost:8200` |
| `token` | A Vault token with encrypt/decrypt policy on `keyName` |
| `keyName` | The Transit key name — must already exist in Vault |

```go
type VaultTransitEncryptor struct { /* unexported */ }

func (v *VaultTransitEncryptor) Encrypt(ctx context.Context, plaintext string) (string, error)
func (v *VaultTransitEncryptor) Decrypt(ctx context.Context, ciphertext string) (string, error)
func (v *VaultTransitEncryptor) Hash(ctx context.Context, plaintext string) (string, error)
func (v *VaultTransitEncryptor) Rewrap(ctx context.Context, ciphertext string) (string, error)
```

- **`Encrypt`** calls `POST /v1/transit/encrypt/<keyName>`.
- **`Decrypt`** calls `POST /v1/transit/decrypt/<keyName>`.
- **`Hash`** runs locally as `HMAC-SHA256` keyed on `sha256(token)`, so hash lookups require no
  Vault round-trip and remain deterministic.
- **`Rewrap`** calls `POST /v1/transit/rewrap/<keyName>` to re-encrypt existing ciphertext under
  the latest key version without exposing the plaintext. Use this during key rotation.

Each request sets `X-Vault-Token` and `Content-Type: application/json`. A non-200 response becomes
an error that carries Vault's status code and response body.

See the [Secret Fields guide](../../how-to/secure/secret-fields/) for Vault engine setup and policy
configuration.

## Storage integration

`protoc-gen-storage` emits `<field>_hash` and `<field>_cipher` columns for each secret field and
calls the `Encryptor` in `Create` and `Update`:

```go
h, _ := enc.Hash(ctx, entity.KeyValue)    // → KeyValueHash  (indexed, for lookup)
c, _ := enc.Encrypt(ctx, entity.KeyValue) // → KeyValueCipher (recoverable)
```

The `Repository` constructor accepts the `Encryptor` when the message has secret fields. See
[Secret fields](../../how-to/model-and-persist/model-a-resource/#secret-fields).
