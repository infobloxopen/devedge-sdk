---
title: Security Posture
weight: 3
---

The SDK builds security into the framework through three structural mechanisms: fail-closed
authorization, enforced multi-tenancy, and provable security invariants. These mechanisms
apply automatically to every service you build with the SDK — you do not configure them per
service or opt in per method.

Use this page to understand what the SDK guarantees, what it leaves to infrastructure, and
how to verify each property in your own CI.

## Fail-closed authorization

Authorization runs at two points: when the server starts and on every incoming call.

**Boot-time check.** `server.New` collects the `[]authz.MethodRule` slices contributed by
each registered service. When you call `srv.Serve`, the server checks that every gRPC method
it will serve has an authz rule. If any method is undeclared, the server refuses to start.
A new RPC method without an authz rule produces a boot failure, not a silent security gap.
Every served method must have either an explicit verb/resource pair or an explicit `Public`
declaration.

**Runtime check.** The `grpcauthz` interceptor, installed automatically, evaluates every
non-public call against the authorizer. An unknown principal, a missing grant, or any
authorizer error results in `codes.PermissionDenied`. The interceptor does not fall through
to the handler on uncertainty.

## Multi-tenancy enforcement

Every query issued through a generated repository is automatically scoped to the calling
tenant's `account_id`. The `TenantIDUnary` interceptor reads the `account-id` metadata
header from the incoming request and places the value on the context. The generated `Get`,
`List`, `Create`, `Update`, and `Delete` methods read `account_id` from the context and add
it as a WHERE clause or column value. The handler never needs to pass it explicitly.

A message with an `account_id` field also gets per-tenant unique constraints. A field marked
`unique` receives a composite index with `account_id` as the leading column, so uniqueness is
enforced within a tenant, not globally.

The isolation property is provable in CI, not assumed. `seccheck.AssertCrossAccountIsolation`
creates a resource as principal A, then asserts that principal B receives `codes.NotFound` on
`Get` and count 0 on `List`. The SDK runs this assertion on every PR; wire it into your own CI
test suite to confirm isolation in your service.

## Secret fields

A field annotated `(infoblox.field.v1.opts).secret = true` is never stored, logged, or
returned as plaintext. The SDK enforces this at three points:

1. **Storage.** `protoc-gen-storage` does not emit a plaintext column. It emits a
   deterministic `_hash` column (for indexed lookup) and a recoverable `_cipher` column. The
   `toModel`/`fromModel` helpers skip secret fields, so plaintext cannot round-trip through
   the model.
2. **Logging.** `middleware/redact` replaces the value with `[REDACTED]` before any log
   record is emitted.
3. **Responses.** `seccheck.AssertNoSecretFieldsLeaked` walks every response proto and
   returns an `Error` finding for any secret field that holds anything other than
   `[REDACTED]`. Wire it into your CI test to get a mechanical guarantee.

The encryptor is a pluggable seam: `secret.NewDev` (AES-256-GCM, in-process) for local
development and tests; `secret.NewVaultTransit` (no Vault SDK dependency) for production.
Swapping the encryptor requires no change to service code — only a constructor argument
changes.

## Error message safety

The `ErrorMapperUnary` interceptor converts persistence sentinels to safe gRPC status codes.
Internal details — SQL errors, file paths, Go internals — are stripped from error messages
before they cross the API boundary. `seccheck.AssertErrorMessagesClean` verifies this in CI
by running known-error triggers and checking that the resulting gRPC error messages contain no
forbidden substrings.

## CI assertions

The `seccheck` package surfaces all four invariants as ordinary Go test assertions. No special
harness is required — they run as part of `go test`.

| Assertion | What it proves |
|---|---|
| `AssertRulesComplete` | Every served method has an authz rule (static) |
| `AssertUnknownPrincipalDenied` | An unknown principal receives `PermissionDenied` on every non-public method (dynamic) |
| `AssertCrossAccountIsolation` | Principal B cannot read or list principal A's resources (dynamic) |
| `AssertErrorMessagesClean` | No internal detail leaks through error messages (dynamic) |
| `AssertNoSecretFieldsLeaked` | No secret field holds a plaintext value in any response (dynamic) |

See [Security Check](../../how-to/secure/security-check/) for the full wiring guide.

## SDK boundaries

The SDK handles authorization enforcement, tenant scoping, secret field treatment, and error
message safety. The following concerns are outside the SDK:

- **Vault policies and tokens.** `secret.NewVaultTransit` takes an address, token, and key
  name. Provisioning those is the platform team's responsibility.
- **Authorization policy logic.** The authz seam enforces that a decision was made and was
  "allow"; it does not contain the policy. The dev authorizer uses an in-memory grant list;
  production uses OPA or Cedar via an adapter.
- **Network-level security.** mTLS, certificate rotation, and network policies are
  infrastructure concerns.
