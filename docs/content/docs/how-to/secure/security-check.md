---
title: Security Check
weight: 1
aliases:
  - /docs/guides/security-check/
---

`seccheck` is a Go test helper that turns the SDK's security invariants into standard test assertions. You add a `security_test.go` file to your service, call the provided assertion functions, and `seccheck.RunT` maps any findings to `t.Errorf` or `t.Logf` calls — so security properties fail your existing `go test` run with no separate harness.

Use `seccheck` when you want CI to catch a security regression the moment a change is merged, rather than discovering it in a manual review.

## Findings and RunT

Each assertion returns a `[]seccheck.Finding`. A finding carries the method name, a severity, and a human-readable message:

```go
type Finding struct {
    Method   string
    Severity Severity // Notice, Warning, Error
    Message  string
}

// RunT: Error+Warning → t.Errorf (fails the test); Notice → t.Logf.
func RunT(t testing.TB, findings []Finding)
```

Pass the slice directly to `seccheck.RunT` to convert findings into test failures:

```go
findings := seccheck.AssertCrossAccountIsolation(ctx, cfg)
seccheck.RunT(t, findings)
```

## Assertions

### AssertRulesComplete

`AssertRulesComplete` checks that every method in an authz rules slice has a non-empty verb and resource. It fails if the rules slice is empty or if any non-public rule omits either field. This is the static counterpart to the fail-closed boot gate.

```go
func TestSecurity_RulesComplete(t *testing.T) {
    findings := seccheck.AssertRulesComplete(apikeyv1.APIKeyServiceAuthzRules)
    seccheck.RunT(t, findings)
}
```

### AssertUnknownPrincipalDenied

`AssertUnknownPrincipalDenied` calls every non-public method with a principal that has no grants (account ID `__seccheck_unknown__`) and asserts that each call returns `codes.PermissionDenied`. You provide a `CallFn` per method. Methods with no `CallFn` produce a `Notice` and are skipped.

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

### AssertCrossAccountIsolation

`AssertCrossAccountIsolation` verifies [tenant isolation](../../../concepts/tenant-isolation/): it creates a resource as Principal A and asserts that Principal B cannot read it (`codes.NotFound`) or list it (count 0). See [Tenant Isolation](../../../concepts/tenant-isolation/) for a full example.

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

### AssertErrorMessagesClean

`AssertErrorMessagesClean` runs each trigger and fails if the resulting gRPC error message contains internal details. The forbidden substrings include `persistence:`, SQL keywords (`SELECT `, `INSERT `, `WHERE `, `ERROR:`), filesystem paths (`/home/`, `/Users/`, `/app/`), and Go internals (`goroutine `, `.go:`).

```go
findings := seccheck.AssertErrorMessagesClean(ctx, []seccheck.ErrorTrigger{
    {Method: "GetAPIKey/notfound", Fn: func(ctx context.Context) error {
        _, err := client.GetAPIKey(ctx, &apikeyv1.GetAPIKeyRequest{Id: "missing"})
        return err
    }},
})
seccheck.RunT(t, findings)
```

### AssertNoSecretFieldsLeaked

`AssertNoSecretFieldsLeaked` walks each response proto and returns an `Error` for any `secret` field that holds a value other than the literal `[REDACTED]`.

```go
created, _ := client.CreateAPIKey(ctx, req)
got, _ := client.GetAPIKey(ctx, &apikeyv1.GetAPIKeyRequest{Id: created.Id})
findings := seccheck.AssertNoSecretFieldsLeaked(created, got)
seccheck.RunT(t, findings) // GetAPIKey must NOT return key_value
```

## Complete security_test.go

The following example runs all five assertions in a single `TestSecurity` test function:

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

## CI integration

`seccheck` runs as part of `go test` — no special harness. A typical CI step:

```yaml {filename=".github/workflows/ci.yml"}
- name: Security checks
  run: go test ./... -run TestSecurity -v
```

{{< callout type="info" >}}
**Run dynamic checks against real boundaries, not mocks.** The dynamic assertions (`AssertUnknownPrincipalDenied`, `AssertCrossAccountIsolation`, `AssertErrorMessagesClean`) exercise a real gRPC server and a real repository. Use an in-memory SQLite database or a `MemoryRepository` so they need no external services. The SDK's own apikey fixture runs the isolation check against both the GORM and ent repositories and expects zero findings.
{{< /callout >}}
