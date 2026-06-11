---
title: Security Check
weight: 4
---

`seccheck` turns the SDK's security invariants into ordinary Go test assertions, so they run in
CI on every change. Each assertion returns a `[]seccheck.Finding`; `seccheck.RunT` maps those to
test failures.

## Findings and `RunT`

```go
type Finding struct {
    Method   string
    Severity Severity // Notice, Warning, Error
    Message  string
}

// RunT: Error+Warning → t.Errorf (fails the test); Notice → t.Logf.
func RunT(t testing.TB, findings []Finding)
```

The usual pattern is "collect findings, then `RunT`, expecting zero failures":

```go
findings := seccheck.AssertCrossAccountIsolation(ctx, cfg)
seccheck.RunT(t, findings)
```

## The assertions

### `AssertRulesComplete` — every method declares authz (static)

Fails if the rules slice is empty, or if any non-public rule has an empty verb or resource. This
is the static counterpart to the fail-closed boot gate.

```go
func TestSecurity_RulesComplete(t *testing.T) {
    findings := seccheck.AssertRulesComplete(apikeyv1.APIKeyServiceAuthzRules)
    seccheck.RunT(t, findings)
}
```

### `AssertUnknownPrincipalDenied` — fail-closed at runtime (dynamic)

Calls every non-public method with a principal that has no grants (account-id
`__seccheck_unknown__`) and asserts each returns `codes.PermissionDenied`. You provide a `CallFn`
per method; methods with no `CallFn` produce a `Notice` and are skipped.

```go
func TestSecurity_UnknownPrincipalDenied(t *testing.T) {
    calls := map[string]seccheck.CallFn{
        "/apikey.v1.APIKeyService/CreateAPIKey": func(ctx context.Context) error {
            _, err := client.CreateAPIKey(ctx, &apikeyv1.CreateAPIKeyRequest{})
            return err
        },
        "/apikey.v1.APIKeyService/GetAPIKey": func(ctx context.Context) error {
            _, err := client.GetAPIKey(ctx, &apikeyv1.GetAPIKeyRequest{Id: "x"})
            return err
        },
        // ... one per non-public method ...
    }
    findings := seccheck.AssertUnknownPrincipalDenied(context.Background(),
        apikeyv1.APIKeyServiceAuthzRules, calls)
    seccheck.RunT(t, findings)
}
```

### `AssertCrossAccountIsolation` — tenant isolation (dynamic)

Creates a resource as Principal A and asserts Principal B cannot read it (`codes.NotFound`) or
list it (count 0). See [Tenant Isolation](../../concepts/tenant-isolation/) for a full example.

```go
findings := seccheck.AssertCrossAccountIsolation(context.Background(), seccheck.IsolationConfig{
    PrincipalA: "alice",
    PrincipalB: "bob",
    CreateFn:   func(ctx context.Context) (string, error) { /* create as alice */ },
    ReadFn:     func(ctx context.Context, id string) error { /* read as bob → NotFound */ },
    ListFn:     func(ctx context.Context) (int, error) { /* list as bob → 0 */ },
})
seccheck.RunT(t, findings)
```

### `AssertErrorMessagesClean` — no internal leakage

Runs each trigger and fails if the resulting gRPC error message contains internal details. The
forbidden substrings include `persistence:`, SQL keywords (`SELECT `, `INSERT `, `WHERE `,
`ERROR:`), filesystem paths (`/home/`, `/Users/`, `/app/`), and Go internals (`goroutine `,
`.go:`).

```go
findings := seccheck.AssertErrorMessagesClean(ctx, []seccheck.ErrorTrigger{
    {Method: "GetAPIKey/notfound", Fn: func(ctx context.Context) error {
        _, err := client.GetAPIKey(ctx, &apikeyv1.GetAPIKeyRequest{Id: "missing"})
        return err
    }},
})
seccheck.RunT(t, findings)
```

### `AssertNoSecretFieldsLeaked` — secret fields never returned

Walks each response proto and returns an `Error` for any `secret` field that holds a value other
than the literal `[REDACTED]`.

```go
created, _ := client.CreateAPIKey(ctx, req)
got, _ := client.GetAPIKey(ctx, &apikeyv1.GetAPIKeyRequest{Id: created.Id})
findings := seccheck.AssertNoSecretFieldsLeaked(created, got)
seccheck.RunT(t, findings) // GetAPIKey must NOT return key_value
```

## A complete `security_test.go`

```go {filename="security_test.go"}
package apikeyv1_test

import (
    "context"
    "testing"

    "github.com/infobloxopen/devedge-sdk/seccheck"
    "github.com/infobloxopen/devedge-sdk/testdata/apikey/apikeyv1"
)

func TestSecurity(t *testing.T) {
    // 1. Static: every method declares authz.
    t.Run("RulesComplete", func(t *testing.T) {
        seccheck.RunT(t, seccheck.AssertRulesComplete(apikeyv1.APIKeyServiceAuthzRules))
    })

    // (stand up a server + client and an Encryptor-backed repo here)

    // 2. Fail-closed for unknown principals.
    t.Run("UnknownPrincipalDenied", func(t *testing.T) {
        seccheck.RunT(t, seccheck.AssertUnknownPrincipalDenied(
            context.Background(), apikeyv1.APIKeyServiceAuthzRules, calls))
    })

    // 3. Cross-account isolation.
    t.Run("CrossAccountIsolation", func(t *testing.T) {
        seccheck.RunT(t, seccheck.AssertCrossAccountIsolation(context.Background(), isoCfg))
    })

    // 4. Clean error messages.
    t.Run("ErrorMessagesClean", func(t *testing.T) {
        seccheck.RunT(t, seccheck.AssertErrorMessagesClean(context.Background(), triggers))
    })

    // 5. No secret fields leaked.
    t.Run("NoSecretFieldsLeaked", func(t *testing.T) {
        seccheck.RunT(t, seccheck.AssertNoSecretFieldsLeaked(created, got))
    })
}
```

## Wire it into CI

`seccheck` runs as part of `go test` — no special harness. A typical CI step:

```yaml {filename=".github/workflows/ci.yml"}
- name: Security checks
  run: go test ./... -run TestSecurity -v
```

Because the dynamic checks exercise real boundaries (a real gRPC server, a real repository),
run them as end-to-end tests with an in-memory SQLite or a `MemoryRepository` so they need no
external services. The SDK's own apikey fixture runs the isolation check against **both** the
GORM and ent repositories and expects zero findings.
