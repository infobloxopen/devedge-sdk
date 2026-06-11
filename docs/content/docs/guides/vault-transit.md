---
title: Vault Transit
weight: 5
---

In production, secret fields are encrypted by **HashiCorp Vault's Transit Secrets Engine** rather
than an in-process key. Transit is "encryption as a service": the plaintext is sent to Vault,
Vault returns ciphertext, and the encryption key never leaves Vault.

The SDK's `secret.VaultTransitEncryptor` talks to Vault over **plain HTTP** — there is **no Vault
SDK dependency** in the SDK.

## 1. Enable the Transit engine and create a key

```bash
# Enable the transit secrets engine (once per Vault).
vault secrets enable transit

# Create a named encryption key for this service's secret fields.
vault write -f transit/keys/apikey
```

The key name (`apikey` here) is what you pass to `NewVaultTransit`. **It must already exist** —
the encryptor does not create it.

## 2. Grant a policy and issue a token

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

## 3. Construct the encryptor

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
interface, so swapping it for `secret.NewDev(key)` (or vice versa) requires **no other change**.

## How each operation maps to Vault

| `Encryptor` method | Vault call | Notes |
|---|---|---|
| `Encrypt` | `POST /v1/transit/encrypt/<key>` | base64-encodes the plaintext, returns Vault's `vault:v1:...` ciphertext |
| `Decrypt` | `POST /v1/transit/decrypt/<key>` | base64-decodes Vault's plaintext back to the string |
| `Hash` | *(local)* | HMAC-SHA256 keyed on `sha256(token)` — computed locally so lookups need **no** Vault round-trip |
| `Rewrap` | `POST /v1/transit/rewrap/<key>` | re-encrypts existing ciphertext under the latest key version **without revealing plaintext** |

Every HTTP call sets `X-Vault-Token` and `Content-Type: application/json`, and a non-200 response
becomes an error carrying Vault's status and body.

## Why a local hash

The lookup hash (used by `LookupBy<Field>Hash`) must be **deterministic** so the same plaintext
always maps to the same hash for indexed lookups. Computing it locally with an HMAC keyed on
`sha256(token)` keeps it stable and avoids a Vault round-trip on every lookup. The
**confidentiality** of the field still depends on Vault: the recoverable ciphertext is what
Transit produces.

## Key rotation with Rewrap

When you rotate the Transit key in Vault (`vault write -f transit/keys/apikey/rotate`), existing
ciphertext stays decryptable (Transit keeps prior versions), but you can re-encrypt it under the
newest version without ever seeing the plaintext:

```go
newCipher, err := enc.Rewrap(ctx, oldCipher)
// store newCipher; the plaintext was never exposed to the service
```

{{< callout type="info" >}}
Pair this with the SDK's **DSN hotload** convention (`fsnotify://<driver>/<abs-path>`) so rotated
database credentials reload without a restart — see [Storage shapes](../storage-shapes/).
{{< /callout >}}

## Local development

You do **not** need Vault for local dev or tests — use `secret.NewDev(key)` (AES-256-GCM) instead.
See [Secret fields](../secret-fields/). Switch to `NewVaultTransit` only where a real Vault is
available; nothing else in the service changes.
