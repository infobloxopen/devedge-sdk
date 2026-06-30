---
title: Tenant Isolation
weight: 3
---

Multi-tenant services let many accounts share a single deployment. Tenant isolation is the guarantee that a resource created by one account is never visible to another account. devedge-sdk enforces this guarantee at the storage layer — in generated repository code — rather than in handler code, so it cannot be omitted by accident.

Use this page when you need to understand how the SDK enforces data separation, what each storage backend does to scope queries, and how to verify isolation in a test.

## Tenant propagation

An incoming gRPC request carries the tenant identifier in the metadata key `account-id`. The `TenantIDUnary` interceptor reads that value and stores it on the request context. Generated repositories then read it from the context before executing any query.

The relevant context functions are:

```go
func TenantIDFromContext(ctx context.Context) string // returns "" if absent
func WithTenantID(ctx context.Context, tenantID string) context.Context // tests / non-gRPC paths
```

The tenant identifier is never passed as a handler argument. It travels on the context from the interceptor to the repository.

## GORM scoping

When a protobuf message has an `account_id` field, `protoc-gen-storage` adds an `account_id = ?` clause to every read, update, and delete query. The generated `Get` method illustrates the pattern:

```go
func (r *APIKeyRepository) Get(ctx context.Context, key string) (*APIKey, error) {
    var m APIKeyModel
    tenantID := middleware.TenantIDFromContext(ctx)
    q := r.db.WithContext(ctx).Where("id = ?", key)
    if tenantID != "" {
        q = q.Where("account_id = ?", tenantID)
    }
    if err := q.First(&m).Error; err != nil {
        if err == gorm.ErrRecordNotFound {
            return nil, persistence.ErrNotFound
        }
        return nil, fmt.Errorf("get APIKey: %w", err)
    }
    return fromModel_APIKey(&m), nil
}
```

The same scoping applies in `List`, `Update`, `Delete`, and the generated `LookupBy<Field>Hash` methods. A cross-tenant read returns `ErrNotFound` rather than a permission error. This hides the existence of the resource from the requesting tenant.

## Authorization and scoping layers

Two layers act in sequence on every request. The caller's final status depends on which layer rejects the request.

1. **Authorization (per-tenant grant).** `grpcauthz` checks the request's principal — derived from the same `account-id` metadata — against the authorizer's grants. A tenant with no grant for the resource type receives `PermissionDenied` (HTTP 403) before the handler or repository runs. With a single-tenant dev grant such as `{Tenant: "alice", …}`, a request carrying `account-id: bob` matches no grant and is rejected with 403, regardless of who owns the resource.
2. **Repository (tenant scoping).** Once a request is authorized for the resource type, it reaches the tenant-scoped query. Reading another tenant's specific resource returns `NotFound` (HTTP 404), hiding that the resource exists.

The 404 response therefore applies only when a caller is authorized for the resource type but does not own the specific resource — for example, when two tenants are both granted the `read api_keys` verb. A caller with no grant at all receives 403.

{{< callout type="info" >}}
**`seccheck.AssertCrossAccountIsolation` exercises the repository layer directly.** It asserts `NotFound` because it calls the repository with a valid tenant context, bypassing the authorization layer. To observe the 404 path end to end, grant both test tenants the verb and resource, then let the repository hide the row.
{{< /callout >}}

## Ent scoping

The ent backend enforces the same guarantee through a query interceptor installed by the generated `entrepo.TenantMixin`. This mixin is embedded automatically whenever the protobuf message has an `account_id` field. The interceptor runs on every `Get`, `List`, and edge-traversal query and scopes it to the tenant returned by `middleware.TenantIDFromContext(ctx)`.

`protoc-gen-ent` emits a `WhereAccountID` method per tenant resource in `ent/<resource>_filter.ent.go`. The interceptor calls this method to apply the scope, so isolation is wired without any code you need to write. Soft-delete works the same way: `SoftDeleteMixin` installs a `WhereDeleteTimeIsNil` scope that the interceptor also applies.

Because the interceptor runs on graph traversals as well as CRUD methods, the isolation bound holds even for ad-hoc ent queries.

## Verifying isolation

`seccheck` provides a check you wire into a normal Go test. The check creates a resource as one principal, then attempts to read and list it as a second principal.

```go
cfg := seccheck.IsolationConfig{
    PrincipalA: "alice",
    PrincipalB: "bob",
    CreateFn: func(ctx context.Context) (string, error) {
        aliceCtx := middleware.WithTenantID(ctx, "alice")
        created, err := repo.Create(aliceCtx, &APIKey{
            Id: "k1", Name: "alice key", AccountId: "alice", KeyValue: "sk_alice",
        })
        if err != nil {
            return "", err
        }
        return created.Id, nil
    },
    ReadFn: func(ctx context.Context, id string) error {
        bobCtx := middleware.WithTenantID(ctx, "bob")
        _, err := repo.Get(bobCtx, id)
        return mapToNotFound(err) // persistence.ErrNotFound → codes.NotFound
    },
    ListFn: func(ctx context.Context) (int, error) {
        bobCtx := middleware.WithTenantID(ctx, "bob")
        items, _, err := repo.List(bobCtx, persistence.ListOptions{})
        return len(items), err
    },
}

findings := seccheck.AssertCrossAccountIsolation(context.Background(), cfg)
seccheck.RunT(t, findings) // ZERO findings expected
```

For isolation to hold, `ReadFn` must return `codes.NotFound` and `ListFn` must return count 0. Any other result produces an `Error` finding that fails the test. The SDK's own apikey fixture runs this check against both the GORM and ent repositories and expects zero findings.

{{< callout type="info" >}}
**The `account-id` metadata key is shared by both tenant scoping and authorization.** The identifier that scopes a request's repository queries is the same one used for the unknown-principal authz check — the tenant and the authz principal share an origin.
{{< /callout >}}

See [Security check](../../how-to/secure/security-check/) for the full set of assertions.
