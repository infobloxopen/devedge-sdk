---
title: "Tutorial: API Key Manager"
weight: 1
---

This tutorial walks you through building an **API Key Manager** service: a multi-tenant store of
customer API keys that are never stored as plaintext, never returned after creation, and invisible
across accounts.

Follow this tutorial to learn how the proto annotation contract, storage codegen, and the `seccheck`
security suite fit together. Every snippet comes from the
[`testdata/apikey`](https://github.com/infobloxopen/devedge-sdk/tree/main/testdata/apikey)
fixture that ships with the SDK.

{{< callout type="info" >}}
**What you learn:** the verify-only `credential` mode (the right way to store an API key), the
retrievable `secret` mode for values you must read back, codegen for two storage shapes at once, the
full `seccheck` suite, and booting against Postgres with `de project up`.
{{< /callout >}}

## Two ways to protect key material

Before writing the proto, choose how the value is protected — by whether the service ever needs it
*back*:

- **`credential: true` — the right default for an API key.** The server **mints** the key, returns it
  to the client **once**, and thereafter only *verifies* a presented token. Storage keeps a public
  lookup id plus a salted one-way **hash** — there is **no reversible copy**, so nothing decryptable
  can leak. This tutorial features it below.
- **`secret: true` — for retrievable secrets.** For a value the service must read back and replay
  (an upstream password, say), the field is hashed for lookup and **encrypted** (a reversible cipher)
  for recovery. The rest of this tutorial (the `Encryptor`, Vault) covers that mode; use it only when
  the plaintext genuinely has to come back.

### A verify-only credential field

Annotate the minted value `credential`. The `testdata/apikey` fixture's `ServiceToken` resource
exercises this end to end on both storage backends:

```proto {filename="apikey.proto"}
message ServiceToken {
  string id           = 1;
  string label        = 2;
  // The server mints this; the client never supplies it. credential_prefix sets
  // the scanner-recognizable token prefix ("st_..."); it defaults to "ib".
  string secret_value = 3 [(infoblox.field.v1.opts) = {credential: true, credential_prefix: "st"}];
}
```

Codegen emits **four** columns — `secret_value_public_id` (UNIQUE), `secret_value_salt`,
`secret_value_hash`, `secret_value_hashspec` — and **no** plaintext or cipher column. The constructor
takes a `*secret.CredentialMinter`, `Create` mints and returns the token once, and a `Verify<Field>`
helper verifies a presented token in constant time:

```go
minter := &secret.CredentialMinter{Prefix: "st"} // default: SHA-512/256 over a 256-bit secret
repo := apikeyv1.NewServiceTokenEntRepository(client, minter)

// Create MINTS the value and returns the full token exactly once.
created, _ := repo.Create(ctx, &apikeyv1.ServiceToken{Id: "st-1", Label: "ci deploy"})
token := created.SecretValue // "st_9f3k…_Xq7…" — hand to the client now; never retrievable again

// Verify: parse → look up by public_id (tenant-independent) → constant-time compare.
rec, ok, err := apikeyv1.VerifySecretValue(ctx, client, token) // ok=true; wrong/tampered → ok=false
```

`Get` and `List` never return `secret_value`, and a nil minter fails loud
(`persistence.ErrNoMinter`) instead of storing an unusable credential. That is the whole credential
lifecycle. The rest of the tutorial builds the fuller service — authz, tenancy, an HTTP surface, and
the `seccheck` suite — using the **retrievable `secret`** mode for contrast.

{{< callout type="info" >}}
See [Secret fields](../../how-to/secure/secret-fields/#verify-only-credentials-credential-true) and
the [secret reference](../../reference/secret/#verify-only-credentials) for the credential API,
hash choices (SHA-512/256 default; SHA-384 and PBKDF2 available), and token format.
{{< /callout >}}

## Step 1 — Define `apikey.proto`

For the fuller service below, `key_value` uses the **retrievable `secret`** mode (a value the service
could read back) so the tutorial can also cover the `Encryptor` seam and Vault. For a pure API key,
prefer the [verify-only `credential`](#a-verify-only-credential-field) mode shown above. The
`account_id` field makes the resource tenant-scoped. Each RPC declares its authz rule and HTTP
mapping.

```proto {filename="apikey.proto"}
syntax = "proto3";
package apikey.v1;

option go_package = "github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1;apikeyv1";

import "google/api/annotations.proto";
import "infoblox/authz/v1/authz.proto";
import "infoblox/field/v1/field.proto";

message APIKey {
  string id         = 1;
  string name       = 2;
  string account_id = 3;
  // key_value is raw API key material. The storage layer hashes and encrypts it;
  // it is cleared in all responses after creation.
  string key_value  = 4 [(infoblox.field.v1.opts) = {secret: true}];
  string key_prefix = 5; // first 8 chars, for display (NOT secret)
}

service APIKeyService {
  rpc CreateAPIKey(CreateAPIKeyRequest) returns (APIKey) {
    option (google.api.http) = {post: "/v1/apikeys", body: "api_key"};
    option (infoblox.authz.v1.rule) = {verb: "create", resource: "api_keys"};
  }
  rpc GetAPIKey(GetAPIKeyRequest) returns (APIKey) {
    option (google.api.http) = {get: "/v1/apikeys/{id}"};
    option (infoblox.authz.v1.rule) = {verb: "read", resource: "api_keys"};
  }
  rpc ListAPIKeys(ListAPIKeysRequest) returns (ListAPIKeysResponse) {
    option (google.api.http) = {get: "/v1/apikeys"};
    option (infoblox.authz.v1.rule) = {verb: "read", resource: "api_keys"};
  }
  rpc DeleteAPIKey(DeleteAPIKeyRequest) returns (DeleteAPIKeyResponse) {
    option (google.api.http) = {delete: "/v1/apikeys/{id}"};
    option (infoblox.authz.v1.rule) = {verb: "delete", resource: "api_keys"};
  }
}

message CreateAPIKeyRequest  { APIKey api_key = 1; }
message GetAPIKeyRequest     { string id = 1; }
message ListAPIKeysRequest   { int32 page_size = 1; string page_token = 2; }
message ListAPIKeysResponse  { repeated APIKey api_keys = 1; string next_page_token = 2; }
message DeleteAPIKeyRequest  { string id = 1; }
message DeleteAPIKeyResponse {}
```

## Step 2 — Run `buf generate`

Configure all plugins in a `buf.gen.yaml` template. The storage shapes pull in `gorm.io/gorm` and
`entgo.io/ent`, so generate into a module that already has those dependencies. The fixture uses its
own `go.mod` for exactly this reason.

```yaml {filename="buf.gen.apikey.yaml"}
version: v2
inputs:
  - directory: testdata/apikey
plugins:
  - local: protoc-gen-go
    out: testdata/apikey
    opt: module=github.com/infobloxopen/devedge-sdk/testdata/apikey
  - local: protoc-gen-go-grpc
    out: testdata/apikey
    opt: module=github.com/infobloxopen/devedge-sdk/testdata/apikey
  - local: protoc-gen-devedge-authz
    out: testdata/apikey
    opt: module=github.com/infobloxopen/devedge-sdk/testdata/apikey
  - local: protoc-gen-svc
    out: testdata/apikey
    opt: module=github.com/infobloxopen/devedge-sdk/testdata/apikey
  - local: protoc-gen-storage
    out: testdata/apikey
    opt: module=github.com/infobloxopen/devedge-sdk/testdata/apikey
  - local: protoc-gen-grpc-gateway
    out: testdata/apikey
    opt: module=github.com/infobloxopen/devedge-sdk/testdata/apikey
  - local: protoc-gen-ent
    out: testdata/apikey
```

```bash
buf generate --template buf.gen.apikey.yaml
go generate ./testdata/apikey/ent   # build the ent client from the generated schema
```

After generation you have, among others:

- `apikeyv1/apikey.authz.go` — `APIKeyServiceAuthzRules`
- `apikeyv1/apikey.svc.go` — the service scaffold
- `apikeyv1/apikey.storage.go` — `APIKeyModel` + `APIKeyRepository` (GORM)
- `ent/schema/api_key.go` + the ent client — the ent shape

Because `key_value` is annotated `secret`, the GORM model has **no `key_value` column** — only
`key_value_hash` (indexed) and `key_value_cipher`. The constructor takes an `Encryptor`:

```go
func NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor) *APIKeyRepository
```

## Step 3 — Wire the encryptor

Select the encryptor based on the environment. Dev uses AES-256-GCM in-process; production uses
Vault Transit. Both satisfy `secret.Encryptor`, so the repository code is the same in both cases.

```go {filename="wire.go"}
package main

import (
    "os"

    "gorm.io/gorm"

    "github.com/infobloxopen/devedge-sdk/secret"
    "github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
)

func newRepo(db *gorm.DB) *apikeyv1.APIKeyRepository {
    var enc secret.Encryptor
    if addr := os.Getenv("VAULT_ADDR"); addr != "" {
        // Production: Vault Transit. The key "apikey" must already exist.
        enc = secret.NewVaultTransit(addr, os.Getenv("VAULT_TOKEN"), "apikey")
    } else {
        // Dev: AES-256-GCM. Key must be exactly 32 bytes.
        enc = secret.NewDev([]byte(os.Getenv("DEV_SECRET_KEY")))
    }
    return apikeyv1.NewAPIKeyRepository(db, enc)
}
```

In the create handler, pass the raw `key_value` to the repository once. The framework hashes and
encrypts it, and the create response returns it to the caller that one time. On subsequent
`GetAPIKey` and `ListAPIKeys` calls the field is empty — `fromModel` does not set it.

## Step 4 — Write `security_test.go`

The `seccheck` package turns the SDK's security invariants into runnable test assertions. Wire all
five checks as shown below. The isolation check is taken from the fixture's
`security_isolation_test.go`.

```go {filename="security_test.go"}
package apikeyv1_test

import (
    "context"
    "errors"
    "testing"

    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"

    "github.com/infobloxopen/devedge-sdk/middleware"
    "github.com/infobloxopen/devedge-sdk/persistence"
    "github.com/infobloxopen/devedge-sdk/seccheck"
    "github.com/infobloxopen/devedge-sdk/secret"
    "github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
)

func mapToNotFound(err error) error {
    if errors.Is(err, persistence.ErrNotFound) {
        return status.Error(codes.NotFound, "not found")
    }
    return err
}

func TestSecurity(t *testing.T) {
    // 1. Static: every method declares a verb + resource.
    t.Run("RulesComplete", func(t *testing.T) {
        seccheck.RunT(t, seccheck.AssertRulesComplete(apikeyv1.APIKeyServiceAuthzRules))
    })

    // Set up an in-memory GORM repo with a dev encryptor (see fixture for the
    // SQLite dialector). enc/repo are reused by the dynamic checks below.
    enc := secret.NewDev(make([]byte, 32))
    repo := apikeyv1.NewAPIKeyRepository(openTestDB(t), enc)

    // 2. Cross-account isolation: alice's keys are invisible to bob.
    t.Run("CrossAccountIsolation", func(t *testing.T) {
        cfg := seccheck.IsolationConfig{
            PrincipalA: "alice",
            PrincipalB: "bob",
            CreateFn: func(ctx context.Context) (string, error) {
                aliceCtx := middleware.WithTenantID(ctx, "alice")
                k := &apikeyv1.APIKey{
                    Id: "k1", Name: "alice key", AccountId: "alice", KeyValue: "sk_alice",
                }
                created, err := repo.Create(aliceCtx, k)
                if err != nil {
                    return "", err
                }
                return created.Id, nil
            },
            ReadFn: func(ctx context.Context, id string) error {
                bobCtx := middleware.WithTenantID(ctx, "bob")
                _, err := repo.Get(bobCtx, id)
                return mapToNotFound(err) // expect codes.NotFound
            },
            ListFn: func(ctx context.Context) (int, error) {
                bobCtx := middleware.WithTenantID(ctx, "bob")
                items, _, err := repo.List(bobCtx, persistence.ListOptions{})
                return len(items), err // expect 0
            },
        }
        seccheck.RunT(t, seccheck.AssertCrossAccountIsolation(context.Background(), cfg))
    })

    // 3. No secret fields leaked: GetAPIKey must never return key_value.
    t.Run("NoSecretFieldsLeaked", func(t *testing.T) {
        ctx := middleware.WithTenantID(context.Background(), "alice")
        created, _ := repo.Create(ctx, &apikeyv1.APIKey{
            Id: "k2", Name: "leak check", AccountId: "alice", KeyValue: "sk_secret",
        })
        got, _ := repo.Get(ctx, created.Id)
        // created may echo key_value once; got must not carry it.
        seccheck.RunT(t, seccheck.AssertNoSecretFieldsLeaked(got))
    })

    // 4. Fail-closed: an ungranted principal is denied on every non-public method.
    //    (Stand up a real server + client; provide a CallFn per method.)
    t.Run("UnknownPrincipalDenied", func(t *testing.T) {
        seccheck.RunT(t, seccheck.AssertUnknownPrincipalDenied(
            context.Background(), apikeyv1.APIKeyServiceAuthzRules, calls))
    })

    // 5. Clean errors: a not-found error must not leak SQL or paths.
    t.Run("ErrorMessagesClean", func(t *testing.T) {
        seccheck.RunT(t, seccheck.AssertErrorMessagesClean(context.Background(), triggers))
    })
}
```

Run the suite:

```bash
go test ./testdata/apikey/... -run TestSecurity -v
```

All five checks report **zero failures**. The fixture runs the isolation check against **both** the
GORM and the ent repository, verifying that the same invariant holds in each storage shape.

## Step 5 — Boot with Postgres via `de project up`

To run end-to-end against a real database, use `de project up` to start the service and its
Postgres backing store. The dev environment wires the indirect-DSN hotload convention so that
rotated credentials reload without a service restart.

```bash
de project up        # boots the service + a Postgres backing store
```

Then exercise the HTTP gateway, which is mapped from the proto's `google.api.http` options. The
service is wired with `PrincipalFunc: grpcauthz.DevPrincipalFunc()` and a dev grant for
`group:admins` (see the [server reference](../../reference/server.md)). The `server.New` gateway
forwards the `account-id` and `groups` headers so the caller's identity reaches authz:

```bash
# Create a key (returns key_value exactly once). The mapping is {post: "/v1/apikeys",
# body: "api_key"}, so the body is the BARE APIKey object — grpc-gateway decodes it
# straight into the api_key field. Do NOT wrap it in {"api_key": {...}}.
curl -s -X POST localhost:8080/v1/apikeys \
  -H 'account-id: alice' -H 'groups: admins' \
  -d '{"name": "ci-bot", "key_value": "sk_live_xxx"}'

# Read it back — key_value is now empty.
curl -s localhost:8080/v1/apikeys/$ID -H 'account-id: alice' -H 'groups: admins'

# bob cannot see alice's key. With the default single-tenant dev grant
# (Tenant: "alice"), authz denies bob BEFORE the repository runs → 403.
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/v1/apikeys/$ID -H 'account-id: bob' -H 'groups: admins'
```

{{< callout type="info" >}}
**403 vs 404 here.** Authz runs before the tenant-scoped repository (see
[Tenant isolation](../../concepts/tenant-isolation/#authorization-and-scoping-layers)).
With a single-tenant grant, `bob` matches no grant and gets **403**. To see the repository's
existence-hiding **404** instead — `bob` is authorized for `api_keys` but still cannot see `alice`'s
specific key — grant both tenants, e.g. `authz.Grant{Tenant: "*", Subjects: []string{"group:admins"}, …}`.
The `seccheck.AssertCrossAccountIsolation` check exercises that repository layer directly, which is
why it asserts `NotFound`.
{{< /callout >}}

When you are done:

```bash
de project down
```

## What you built

- **Contract-first service** — authz and field-protection semantics are declared in proto and
  enforced by the framework.
- **The right protection per value** — a verify-only `credential` (minted, returned once, salted
  hash, no reversible copy, `Verify<Field>`) for API keys; a retrievable `secret` (hash + encrypted
  cipher) only for values that must be read back.
- **Nothing in plaintext at rest** — neither mode stores or returns the raw value after creation.
- **Two storage shapes from one proto** — GORM and ent, both tenant-isolated.
- **Security suite** — asserts authz completeness, fail-closed denial, cross-account isolation,
  clean errors, and no-leak responses in CI.

## See also

- [Secret fields](../../how-to/secure/secret-fields/) — verify-only credentials and retrievable
  secrets (Vault Transit).
- [Storage shapes](../../how-to/model-and-persist/storage-shapes/) — when to use ent's privacy layer.
- [Server reference](../../reference/server.md) — every `Config` option.
