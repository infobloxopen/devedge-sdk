---
title: seccheck
weight: 5
---

```go
import "github.com/infobloxopen/devedge-sdk/seccheck"
```

Package `seccheck` provides Go test assertions for the SDK's security invariants. Each `Assert*` function returns a `[]Finding` slice. Pass that slice to `RunT` to convert findings into standard `testing.TB` calls. Use this package in your service's test suite to verify authorization, cross-account isolation, and secret-field handling.

See the [Security check guide](../../how-to/secure/security-check/) for end-to-end examples.

## Quick reference

| Function | Kind | Passing condition |
|---|---|---|
| `AssertRulesComplete` | static | Rules slice non-empty; every non-public rule has a non-empty `Verb` and `Resource` |
| `AssertUnknownPrincipalDenied` | dynamic | Every non-public method returns `PermissionDenied` for a principal with no grants |
| `AssertCrossAccountIsolation` | dynamic | `ReadFn` returns `NotFound`; `ListFn` (if provided) returns count 0 |
| `AssertErrorMessagesClean` | dynamic | No forbidden substring appears in any error message |
| `AssertNoSecretFieldsLeaked` | static (over responses) | No secret-annotated field carries a non-redacted value |

## Core types

```go
type Severity int
const ( Notice Severity = iota; Warning; Error )

type Finding struct {
    Method   string
    Severity Severity
    Message  string
}

func RunT(t testing.TB, findings []Finding)
```

`RunT` maps severity levels to `testing.TB` calls:

- `Error` and `Warning` → `t.Errorf` (the test fails).
- `Notice` → `t.Logf` (informational; the test continues).

## AssertRulesComplete

```go
func AssertRulesComplete(rules []authz.MethodRule) []Finding
```

Statically validates that every non-public method rule is complete. An empty `rules` slice is itself an `Error` finding. For each non-public rule, the function checks that `Verb` and `Resource` are non-empty; each missing value yields a separate `Error`. This is the static counterpart to the runtime fail-closed boot gate: it checks the same rule completeness at test time that the server enforces at startup.

## AssertUnknownPrincipalDenied

```go
type CallFn func(ctx context.Context) error

func AssertUnknownPrincipalDenied(
    ctx context.Context,
    rules []authz.MethodRule,
    calls map[string]CallFn,
) []Finding
```

Calls every non-public method with a principal that has no grants and asserts that each returns `codes.PermissionDenied`. The function appends `account-id: __seccheck_unknown__` to the outgoing context before each call.

- A method that has no corresponding `CallFn` in `calls` produces a `Notice` finding and is skipped.
- Public methods are skipped automatically.

## AssertCrossAccountIsolation

```go
type IsolationConfig struct {
    PrincipalA string
    PrincipalB string
    CreateFn   func(ctx context.Context) (id string, err error) // create as A
    ReadFn     func(ctx context.Context, id string) error        // read as B → must be NotFound
    ListFn     func(ctx context.Context) (count int, err error)  // optional: list as B → must be 0
}

func AssertCrossAccountIsolation(ctx context.Context, cfg IsolationConfig) []Finding
```

Creates a resource as `PrincipalA`, then verifies that `PrincipalB` cannot see it. The function appends `account-id` metadata for each principal to the context it passes to your callbacks.

- `ReadFn` must return `codes.NotFound`; any other result is an `Error`.
- `ListFn` is optional. When provided, it must return count 0; any other result is an `Error`.

## AssertErrorMessagesClean

```go
type ErrorTrigger struct {
    Method string
    Fn     func(ctx context.Context) error
}

func AssertErrorMessagesClean(ctx context.Context, triggers []ErrorTrigger) []Finding
```

Runs each trigger and inspects the gRPC status message for substrings that indicate a leaked internal detail. A match on any forbidden substring is an `Error`. A trigger that returns a nil error produces a `Warning`.

The forbidden substrings are:

```
"persistence:", "SELECT ", "INSERT ", "UPDATE ", "WHERE ", "ERROR:",
"/home/", "/Users/", "/app/", "goroutine ", ".go:"
```

## AssertNoSecretFieldsLeaked

```go
func AssertNoSecretFieldsLeaked(responses ...proto.Message) []Finding
```

Walks each response proto, recursing into nested messages, and returns an `Error` for any field annotated `(infoblox.field.v1.opts) = {secret: true}` that holds a non-empty string (other than the literal `[REDACTED]`) or a non-zero value of another kind. Nil messages are skipped.
