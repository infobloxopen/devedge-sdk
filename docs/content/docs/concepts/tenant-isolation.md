---
title: Tenant Isolation
weight: 3
---

Infoblox services are multi-tenant: many accounts share one deployment, and a resource created
by one account must be **invisible** to every other. devedge-sdk enforces this at the storage
layer, not in handler code — so it cannot be forgotten.

## The tenant flows from metadata to the query

1. An incoming request carries the tenant in the gRPC metadata key **`account-id`**.
2. The **`TenantIDUnary`** interceptor reads it onto `ctx`:

   ```go
   func TenantIDFromContext(ctx context.Context) string // returns "" if absent
   func WithTenantID(ctx context.Context, tenantID string) context.Context // tests / non-gRPC paths
   ```

3. Generated repositories read `middleware.TenantIDFromContext(ctx)` and **scope every query**
   by it. The tenant is never passed as a handler argument; it travels on the context.

## GORM shape — scoped WHERE clauses

When a message has an `account_id` field, `protoc-gen-storage` adds an `account_id = ?` clause to
every read, update, and delete. The generated `Get` looks like:

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

The same scoping is applied in `List`, `Update`, `Delete`, and the generated
`LookupBy<Field>Hash` methods. A cross-tenant read therefore returns `ErrNotFound` — the
resource simply does not exist *for that tenant* — rather than a permission error that would leak
its existence.

## ent shape — the privacy layer

The ent shape enforces the same invariant through ent's **privacy** rules and **hooks**, applied
by the generated mixin. Tenant scoping is a query-level privacy rule, so it holds even for ad-hoc
graph traversals — not just the CRUD methods. The mixin reads the tenant from the same
`middleware.TenantIDFromContext(ctx)`.

## Proving it: `seccheck.AssertCrossAccountIsolation`

Isolation is verified, not assumed. `seccheck` ships a check you wire into a normal Go test:
create a resource as Principal A, then attempt to read and list it as Principal B.

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

For isolation to hold, `ReadFn` must return **`codes.NotFound`** and `ListFn` must return
**count 0** — anything else is an `Error` finding that fails the test. The SDK's own apikey
fixture runs this against **both** the GORM and ent repositories and expects zero findings.

{{< callout type="info" >}}
The `account-id` metadata key is the same one used for the unknown-principal authz check — the
tenant and the authz principal share an origin, so a request that is authorized is also scoped.
{{< /callout >}}

See [Security check](../../guides/security-check/) for the full set of assertions.
