---
title: Security Posture
weight: 3
---

devedge-sdk's security model has three pillars: **fail-closed authorization**, **enforced
multi-tenancy**, and **provable security invariants**. Each is structural — baked into the
framework, not left to per-service convention.

## Fail-closed authorization

Authorization in the SDK is fail-closed at two levels:

**Boot-time gate.** `server.New` collects the `[]authz.MethodRule` slices contributed by each
registered service. At `srv.Serve`, it checks that every served gRPC method has an authz rule. If
any method is undeclared, the server refuses to start. There is no "silent allow" — the only way a
method serves traffic is if it has an explicit rule, either a declared verb/resource pair or an
explicit `Public` declaration.

**Runtime gate.** The `grpcauthz` interceptor (installed automatically) evaluates every non-public
call against the authorizer. An unknown principal, a missing grant, or any authorizer error all
result in `codes.PermissionDenied`. The interceptor does not fall through to the handler on doubt.

This means: adding a new RPC method to a service without declaring its authz rule is a boot
failure, not a silent security gap.

## Multi-tenancy enforcement

Every query issued through a generated repository is automatically scoped by `account_id`. The
`TenantIDUnary` interceptor reads the `account-id` metadata header from the incoming request and
places the value on the context. The generated `Get`, `List`, `Create`, `Update`, and `Delete`
methods read `account_id` from the context and add it as a WHERE clause or column value — the
handler never needs to pass it explicitly.

A message with an `account_id` field also gets **per-tenant unique constraints**: a field marked
`unique` gets a composite index with `account_id` as the leading column, so uniqueness is enforced
within a tenant, not globally.

The isolation property is provable: `seccheck.AssertCrossAccountIsolation` creates a resource as
principal A, then asserts that principal B receives `codes.NotFound` on `Get` and count 0 on
`List`. This runs in CI on every PR — it is not an assumption.

## Secret fields

A field annotated `(infoblox.field.v1.opts).secret = true` is never stored, logged, or returned
as plaintext. The framework enforces this at three points:

1. **Storage.** `protoc-gen-storage` does not emit a plaintext column. It emits a deterministic
   `_hash` column (for indexed lookup) and a recoverable `_cipher` column. The `toModel`/`fromModel`
   helpers skip secret fields — plaintext cannot round-trip through the model.
2. **Logging.** `middleware/redact` replaces the value with `[REDACTED]` before any log record is
   emitted.
3. **Responses.** `seccheck.AssertNoSecretFieldsLeaked` walks every response proto and returns an
   `Error` finding for any secret field that holds anything other than `[REDACTED]`. Wire it into
   your CI test to get a mechanical guarantee.

The encryptor is a pluggable seam: `secret.NewDev` (AES-256-GCM, in-process) for local dev and
tests; `secret.NewVaultTransit` (no Vault SDK dependency) for production. Swapping them requires
no change to service code — only a constructor argument changes.

## Clean error messages

The `ErrorMapperUnary` interceptor converts persistence sentinels to safe gRPC status codes. Internal
details (SQL, file paths, Go internals) are stripped from error messages before they cross the API
boundary. `seccheck.AssertErrorMessagesClean` verifies this in CI by running known-error triggers
and checking that the resulting gRPC error messages contain no forbidden substrings.

## What is provable in CI

The `seccheck` package surfaces all four invariants as ordinary Go test assertions:

| Assertion | What it proves |
|---|---|
| `AssertRulesComplete` | Every served method has an authz rule (static) |
| `AssertUnknownPrincipalDenied` | An unknown principal receives `PermissionDenied` on every non-public method (dynamic) |
| `AssertCrossAccountIsolation` | Principal B cannot read or list principal A's resources (dynamic) |
| `AssertErrorMessagesClean` | No internal detail leaks through error messages (dynamic) |
| `AssertNoSecretFieldsLeaked` | No secret field holds a plaintext value in any response (dynamic) |

These run as part of `go test` — no special harness. See [Security Check](../../how-to/secure/security-check/) for the full wiring guide.

## What is not in scope

The SDK does not:

- **Manage Vault policies or tokens.** The `secret.NewVaultTransit` encryptor takes an address,
  token, and key name. Provisioning those is the platform team's responsibility.
- **Implement authorization policy logic.** The authz seam enforces that a decision was made and
  was "allow"; it does not contain the policy. The dev authorizer uses an in-memory grant list;
  production uses OPA or Cedar via an adapter.
- **Enforce network-level security.** mTLS, certificate rotation, and network policies are
  infrastructure concerns outside the SDK.
