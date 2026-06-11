---
title: "Tutorial: API Key Manager"
weight: 1
---

In this tutorial you build an **API Key Manager** service end to end: a multi-tenant store of
customer API keys where the raw key material is encrypted at rest, never returned after creation,
and invisible across accounts. Along the way you exercise every part of the SDK.

This mirrors the [`testdata/apikey`](https://github.com/infobloxopen/devedge-sdk/tree/main/testdata/apikey)
fixture that ships with the SDK, so every snippet is real.

{{< callout type="info" >}}
**What you'll learn:** the proto annotation contract, codegen for three shapes at once, the
`Encryptor` seam, the full `seccheck` suite, and booting against Postgres with `de project up`.
{{< /callout >}}

## Step 1 — Define `apikey.proto`

The `key_value` field holds the raw key material, so it is annotated `secret`. `account_id` makes
the resource tenant-scoped. Each RPC declares its authz rule and HTTP mapping.

```proto {filename="apikey.proto"}
syntax = "proto3";
package apikey.v1;

option go_package = "github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1;apikeyv1";

import "google/api/annotations.proto";
import "infoblox/authz/v1/authz.proto";

message APIKey {
  string id         = 1;
  string name       = 2;
  string account_id = 3;
  // key_value is raw API key material. The storage layer hashes and encrypts it;
  // it is cleared in all responses after creation.
  string key_value  = 4 [(infoblox.authz.v1.field).secret = true];
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

Configure every plugin. Because the storage shapes pull in `gorm.io/gorm` / `entgo.io/ent`,
generate into a module that has those deps (the fixture has its own `go.mod`).

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

You now have, among others:

- `apikeyv1/apikey.authz.go` — `APIKeyServiceAuthzRules`
- `apikeyv1/apikey.svc.go` — the service scaffold
- `apikeyv1/apikey.storage.go` — `APIKeyModel` + `APIKeyRepository` (GORM)
- `ent/schema/api_key.go` + the ent client — the ent shape

Because `key_value` is `secret`, the GORM model has **no `key_value` column** — only
`key_value_hash` (indexed) and `key_value_cipher`. The constructor takes an `Encryptor`:

```go
func NewAPIKeyRepository(db *gorm.DB, enc secret.Encryptor) *APIKeyRepository
```

## Step 3 — Wire the encryptor

Pick the encryptor by environment. Dev uses AES-256-GCM in-process; production uses Vault Transit.
Both satisfy `secret.Encryptor`, so the repository code is identical.

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
        // Dev: AES-256-GCM. Key must be >= 32 bytes.
        enc = secret.NewDev([]byte(os.Getenv("DEV_SECRET_KEY")))
    }
    return apikeyv1.NewAPIKeyRepository(db, enc)
}
```

In the create handler you hand the raw `key_value` to the repository **once**; the framework hashes
and encrypts it, and you return it to the caller this one time only. On `GetAPIKey`/`ListAPIKeys`
the field comes back empty — `fromModel` never sets it.

## Step 4 — Write `security_test.go`

This is where the SDK's guarantees become assertions. Wire all of the `seccheck` checks. The
isolation check below is taken from the fixture's real `security_isolation_test.go`.

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

Run it:

```bash
go test ./testdata/apikey/... -run TestSecurity -v
```

All five checks should report **zero failures**. The fixture runs the isolation check against
**both** the GORM and the ent repository — proving the same invariant holds in both shapes.

## Step 5 — Boot with Postgres via `de project up`

For an end-to-end run against a real database, use devedge to bring up the project (server +
Postgres). The dev edge wires the indirect-DSN hotload convention so rotated credentials reload
without a restart.

```bash
de project up        # boots the service + a Postgres backing store
```

Then exercise the HTTP gateway (mapped from the proto's `google.api.http` options):

```bash
# Create a key (returns key_value exactly once).
curl -s -X POST localhost:8080/v1/apikeys \
  -H 'account-id: alice' \
  -d '{"api_key": {"name": "ci-bot", "key_value": "sk_live_xxx"}}'

# Read it back — key_value is now empty.
curl -s localhost:8080/v1/apikeys/$ID -H 'account-id: alice'

# bob cannot see alice's key → 404.
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/v1/apikeys/$ID -H 'account-id: bob'
```

When you're done:

```bash
de project down
```

## What you built

- A **contract-first** service: authz and secret semantics declared in proto, enforced by the
  framework.
- **Secret-at-rest**: `key_value` hashed + encrypted, never stored or returned as plaintext.
- **Two storage shapes** from one proto: GORM and ent, both tenant-isolated.
- A **security suite** that proves authz completeness, fail-closed denial, cross-account
  isolation, clean errors, and no-leak responses — all in CI.

## Where to go next

- [Vault Transit](../../guides/vault-transit/) — production secret handling.
- [Storage shapes](../../guides/storage-shapes/) — when to reach for ent's privacy layer.
- [server reference](../../reference/server.md) — every `Config` knob.
