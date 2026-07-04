---
title: secret
weight: 3
---

```go
import "github.com/infobloxopen/devedge-sdk/secret"
```

Package `secret` covers the two ways the framework keeps a sensitive field out of plaintext storage:

- **Verify-only credentials** (`(infoblox.field.v1.opts).credential`) — an API key or token that the
  server mints, hands to the client once, and only ever *verifies*. The `CredentialMinter` produces
  a prefixed split token and a salted one-way hash; there is **no reversible copy**. Use this for API
  keys.
- **Retrievable secrets** (`(infoblox.field.v1.opts).secret`) — a value the service must read back
  and replay. The `Encryptor` interface hashes it for lookup and encrypts it (a reversible cipher)
  for recovery. Use this only when the plaintext genuinely has to come back.

Generated storage code calls into this package: it mints via `CredentialMinter` for a `credential`
field and hashes/encrypts via `Encryptor` for a `secret` field, and it never stores either as
plaintext.

## Verify-only credentials

The `CredentialMinter` implements the gold-standard "hash, never encrypt" storage for an API key: a
prefixed split token, minted by the server, returned to the client once, verified in constant time,
with no reversible copy at rest. It is stdlib-only (FIPS-clean, no new dependencies).

```go
// HashSpec is self-describing so a stored credential still verifies after the
// process default changes. Algo is one of AlgoSHA512_256 (default), AlgoSHA384,
// or "pbkdf2-sha256:<iters>:<keylen>".
type HashSpec struct{ Algo string }

// StoredCredential is the at-rest form: the public lookup id (UNIQUE), the salt
// and salted hash (base64), and the hash spec. It holds NO reversible secret.
type StoredCredential struct {
    PublicID string
    Salt     string
    Hash     string
    Spec     HashSpec
}

// CredentialMinter mints verify-only credentials. The zero value is usable:
// an empty Prefix defaults to DefaultPrefix ("ib"), an empty Spec.Algo to
// AlgoSHA512_256, and a non-positive SecretBits to DefaultSecretBits (256).
type CredentialMinter struct {
    Prefix     string
    Spec       HashSpec
    SecretBits int
}

// Mint returns the full token "<prefix>_<public_id>_<secret>" — hand it to the
// client ONCE — and the StoredCredential to persist. The secret is never retained.
func (m *CredentialMinter) Mint() (token string, stored StoredCredential, err error)

// Parse splits a presented token into prefix, publicID, and secret.
func Parse(token string) (prefix, publicID, secret string, err error)

// Verify recomputes stored.Spec's hash of (salt||secret) and constant-time-compares
// it to stored.Hash. It returns (false, nil) on a mismatch and an error only when a
// stored field is malformed or the algorithm is unsupported.
func Verify(secret string, stored StoredCredential) (ok bool, err error)
```

**Default hash — a fast FIPS hash, not a slow KDF.** Because the SDK mints the secret with ≥256 bits
of entropy, a slow KDF (PBKDF2) buys nothing and adds a per-verify CPU cost. The default is
`SHA-512/256` over the salted 256-bit secret: fast, FIPS-approved, length-extension-safe. `SHA-384`
and `pbkdf2-sha256:<iters>:<keylen>` are available for CNSA-2.0 or low-entropy/defense-in-depth use;
select them via `HashSpec.Algo`. The chosen spec is stored per credential, so an algorithm change
never invalidates existing credentials.

```go
minter := &secret.CredentialMinter{Prefix: "ib"} // defaults: SHA-512/256, 256-bit secret
token, stored, err := minter.Mint()
// token → client, once. stored → your credential columns.
// Later, to verify a presented token:
_, publicID, presented, _ := secret.Parse(token)
// load StoredCredential by publicID (an indexed, UNIQUE lookup), then:
ok, err := secret.Verify(presented, stored)
```

Generated storage code (see [Storage integration](#storage-integration)) wires all of this for a
`credential` field: it stores the split columns, mints on `Create`, and emits `Verify<Field>`.

## Retrievable secrets

Package `secret` also provides encrypt, decrypt, and hash operations for **retrievable** secret
fields. It implements the `Encryptor` interface that generated storage code calls when a proto field
carries the `(infoblox.field.v1.opts).secret` annotation. The generated storage code calls the
`Encryptor` to hash and encrypt those fields and never stores their plaintext. Use this when you need
to supply or swap the encryption backend for a service that stores retrievable secret fields.

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
is held in process memory and must be **exactly 32 bytes** (a 256-bit key).

{{< callout type="warning" >}}
**Do not use `NewDev` in production.** Use `NewVaultTransit` instead. `NewDev` panics if
`len(key) != 32`: both a short key and an over-length key are rejected. An over-length key is
**not** truncated to its first 32 bytes — silently discarding the tail would make two keys that
share a 32-byte prefix interchangeable and make a partial rotation a no-op.
{{< /callout >}}

```go
enc := secret.NewDev(devKey) // devKey must be exactly 32 bytes
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

Both storage generators (`protoc-gen-storage` and `protoc-gen-ent`) wire this package for you.

**A `credential` field** gets `<field>_public_id` (UNIQUE), `<field>_salt`, `<field>_hash`, and
`<field>_hashspec` columns — no plaintext column and no cipher. `Create` mints via the
`CredentialMinter` and returns the token once; a `Verify<Field>` helper parses a presented token,
looks the record up by `public_id` (tenant-independent), and calls `secret.Verify`:

```go
token, stored, _ := minter.Mint()          // → PublicID/Salt/Hash/Hashspec columns; token returned once
rec, ok, err := VerifySecretValue(ctx, client, token) // parse → lookup by public_id → constant-time verify
```

The repository constructor accepts a `*secret.CredentialMinter` when the message has credential
fields; a nil minter fails loud with `persistence.ErrNoMinter` rather than minting nothing.

**A `secret` field** gets `<field>_hash` and `<field>_cipher` columns, and the generator calls the
`Encryptor` in `Create` and `Update`:

```go
h, _ := enc.Hash(ctx, entity.KeyValue)    // → KeyValueHash  (indexed, for lookup)
c, _ := enc.Encrypt(ctx, entity.KeyValue) // → KeyValueCipher (recoverable)
```

The repository constructor accepts the `Encryptor` when the message has secret fields; a nil
encryptor with a non-empty secret fails loud with `persistence.ErrNoEncryptor`. See
[Secret fields](../../how-to/secure/secret-fields/).
