---
title: seccheck
weight: 5
---

```go
import "github.com/infobloxopen/devedge-sdk/seccheck"
```

Package `seccheck` turns the SDK's security invariants into Go test assertions. Each `Assert*`
function returns `[]Finding`; `RunT` maps those to `testing.TB` calls. See the
[Security check guide](../../guides/security-check/) for end-to-end examples.

## Finding, Severity, RunT

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

`RunT` maps **Error and Warning → `t.Errorf`** (failing the test) and **Notice → `t.Logf`**.

## AssertRulesComplete (static)

```go
func AssertRulesComplete(rules []authz.MethodRule) []Finding
```
An **empty** rules slice is itself an `Error` finding. Otherwise, every **non-public** rule must
have a non-empty `Verb` and `Resource`; each missing one yields an `Error`. The static counterpart
to the fail-closed boot gate.

## AssertUnknownPrincipalDenied (dynamic)

```go
type CallFn func(ctx context.Context) error

func AssertUnknownPrincipalDenied(
    ctx context.Context,
    rules []authz.MethodRule,
    calls map[string]CallFn,
) []Finding
```
Calls every non-public method with a principal that has no grants (it appends `account-id:
__seccheck_unknown__` to the outgoing context) and asserts each returns `codes.PermissionDenied`.
A method with no `CallFn` produces a `Notice` and is skipped; public methods are skipped.

## AssertCrossAccountIsolation (dynamic)

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
Creates a resource as `PrincipalA`, then verifies `PrincipalB` cannot see it. `ReadFn` must return
`codes.NotFound` and `ListFn` (if provided) must return count 0; anything else is an `Error`. The
function appends `account-id` metadata for each principal to the context it passes to your
callbacks.

## AssertErrorMessagesClean (dynamic)

```go
type ErrorTrigger struct {
    Method string
    Fn     func(ctx context.Context) error
}

func AssertErrorMessagesClean(ctx context.Context, triggers []ErrorTrigger) []Finding
```
Runs each trigger and inspects the gRPC status message for forbidden substrings — leaking any is
an `Error`; a nil error from a trigger is a `Warning`. The forbidden set:

```
"persistence:", "SELECT ", "INSERT ", "UPDATE ", "WHERE ", "ERROR:",
"/home/", "/Users/", "/app/", "goroutine ", ".go:"
```

## AssertNoSecretFieldsLeaked

```go
func AssertNoSecretFieldsLeaked(responses ...proto.Message) []Finding
```
Walks each response proto (recursing into nested messages) and returns an `Error` for any field
annotated `(infoblox.authz.v1.field).secret = true` that holds a non-empty string (other than the
literal `[REDACTED]`) or a non-zero value of another kind. nil messages are skipped.

## Summary

| Function | Kind | Passing condition |
|---|---|---|
| `AssertRulesComplete` | static | rules non-empty; every non-public rule has verb + resource |
| `AssertUnknownPrincipalDenied` | dynamic | every non-public method → `PermissionDenied` for an ungranted principal |
| `AssertCrossAccountIsolation` | dynamic | B's read → `NotFound`; B's list → 0 |
| `AssertErrorMessagesClean` | dynamic | no forbidden substring in any error message |
| `AssertNoSecretFieldsLeaked` | static (over responses) | no secret field carries a real value |
