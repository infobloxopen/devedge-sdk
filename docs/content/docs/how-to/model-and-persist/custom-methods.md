---
title: Add a custom method or second resource
weight: 4
---

This page shows how to grow a scaffolded single-resource service into one that serves custom
(non-CRUD) RPCs and a second resource, wired through the `servicekit` host the scaffold already
generates. You start from a `devedge-sdk new service` project, add a custom RPC and its handler
alongside the generated CRUD, add a child resource and its repository, and register your handler
through the generated module's `Handler` seam — no forked `servicekit.Module`.

Use this page when the generated CRUD is not enough — your service has business operations
(`Checkout`, `AddItem`) or owns more than one resource. For the `servicekit` API these steps use, see
the [servicekit reference](../../../reference/servicekit/).

## What the scaffold generates

`devedge-sdk new service <name> --resource <Resource>` generates a service whose runtime is
`servicekit`:

- `cmd/<svc>/main.go` — the host. It opens the database, builds the generated repository, and hands
  the module to `servicekit.Run`.
- `module/module.go` — the module. Its `Module(repo, sqlDB)` constructor wraps the generated
  `<Service>Module`, which serves the single resource over the generated CRUD handler and adds a
  database readiness check.

A pure-CRUD service needs no further wiring. The generated `<Service>Module` registers the generated
`<Service>CRUDHandler` over your repository through `Register<Service>WithRepository`. To add custom
methods or a second resource, you grow that module IN PLACE: `<Service>ModuleOptions` carries a
`Handler` field, so you set an override handler instead of forking a hand-written `servicekit.Module`.
The module keeps its generated `Descriptor` — methods, authz rules, resource names — for free.

## Prerequisites

- A service scaffolded with `devedge-sdk new service`, building and passing `make test`.
- Familiarity with [Define a service](../define-a-service/) (the proto → generate loop) and the
  [servicekit reference](../../../reference/servicekit/).

The steps below follow the shopping-cart shape: a `Cart` owner resource, a `CartItem` child, custom
methods (`AddItem`, `RemoveItem`, `Checkout`), and a `CartSummary` read projection.

## 1. Declare the custom RPC in the proto

Add the custom method to the service, alongside the generated CRUD RPCs. A custom method is an
[AIP-136 custom method](https://google.aip.dev/136): it uses a colon suffix on the resource
collection in its HTTP mapping.

```proto {filename="proto/cartd/v1/cartd.proto"}
service CartService {
  // ... generated CRUD: CreateCart, GetCart, ListCarts, UpdateCart, DeleteCart ...

  rpc AddItem(AddItemRequest) returns (Cart) {
    option (google.api.http) = {post: "/v1/carts/{cart_id}:addItem", body: "*"};
    option (infoblox.authz.v1.rule) = {verb: "update", resource: "carts"};
  }
  rpc Checkout(CheckoutRequest) returns (Cart) {
    option (google.api.http) = {post: "/v1/carts/{cart_id}:checkout", body: "*"};
    option (infoblox.authz.v1.rule) = {verb: "update", resource: "carts"};
  }
}

message AddItemRequest {
  string cart_id          = 1;
  string sku              = 2;
  int32  quantity         = 3;
  int64  unit_price_cents = 4;
}
message CheckoutRequest { string cart_id = 1; }
```

{{< callout type="warning" >}}
**Every custom method declares its own `(infoblox.authz.v1.rule)`.** The boot-time authz
completeness gate fails closed: a registered RPC with neither a rule nor a `public: true` exemption
makes the server refuse to start. Pick the `verb` and `resource` that match the operation — a method
that mutates a cart declares `{verb: "update", resource: "carts"}`. See
[server → Server methods](../../../reference/server/#server-methods).
{{< /callout >}}

## 2. Declare the second resource

Add the child resource as its own message. The cart's `CartItem` belongs to a `Cart` through a
scalar `cart_id` foreign key:

```proto {filename="proto/cartd/v1/cartd.proto"}
message CartItem {
  string name        = 1 [(google.api.field_behavior) = OUTPUT_ONLY];
  string id          = 2;
  string account_id  = 3;                  // tenant scope
  string cart_id     = 4;                  // scalar FK, queryable per AIP
  string sku         = 5;
  int32  quantity    = 6;
  int64  unit_price_cents = 7;
  Cart   cart        = 9 [(infoblox.field.v1.opts) = {belongs_to: {foreign_key: "cart_id"}}];
}
```

A message with an `id` field is a resource, so the storage and ent plugins generate a repository for
it. For the relationship annotations (`has_many` on the parent, `belongs_to` on the child), see
[Model a resource → Relationships](../model-a-resource/#relationships) and
[codegen → Relationships in ent](../../../reference/codegen/#relationships-in-ent).

If your custom method returns a read projection, declare it as a multi-surface model over the owner:

```proto {filename="proto/cartd/v1/cartd.proto"}
message CartSummary {
  option (infoblox.storage.v1.model) = "Cart"; // a projection over Cart, no table of its own
  string id          = 1;
  string account_id  = 2;
  string status      = 3;
  string currency    = 4;
}
```

A surface's fields must be a subset of its owner's columns, so a derived rollup (a computed total)
cannot live on the surface — compute it in the handler and return it in the RPC response. See
[codegen → Multi-surface](../../../reference/codegen/#multi-surface).

## 3. Regenerate

```bash
make generate   # buf generate, then go generate ./gen/ent on the ent backend
```

You now have, in `gen/<svc>v1`:

- The custom RPCs on the `<Service>Server` interface. The generated `<Service>CRUDHandler` leaves
  each one `Unimplemented` — a custom method matches no AIP standard shape, so the generator never
  mis-handles it.
- `<Service>AuthzRules` updated with the custom methods' rules.
- A generated repository for the second resource and for any projection:
  `New<Child>EntRepository(client)` and `New<Surface>EntRepository(client)` (or the
  `New<Child>Repository(db)` GORM constructors).

## 4. Write the custom handler

Embed the generated `<Service>CRUDHandler` so the CRUD methods come from the generated defaults, then
implement the custom methods. The handler also holds the child and projection repositories:

```go {filename="cmd/cartd/handler.go"}
type cartHandler struct {
    cartdv1.CartServiceCRUDHandler // CreateCart/GetCart/ListCarts/UpdateCart/DeleteCart
    items   persistence.Repository[*cartdv1.CartItem, string]
    summary persistence.Repository[*cartdv1.CartSummary, string]
}

// AddItem adds an item to a cart, then returns the cart.
func (h *cartHandler) AddItem(ctx context.Context, req *cartdv1.AddItemRequest) (*cartdv1.Cart, error) {
    cart, err := h.Repo.Get(ctx, req.GetCartId())
    if err != nil {
        return nil, err // ErrNotFound → codes.NotFound via the interceptor chain
    }
    if cart.GetStatus() == statusCheckedOut {
        return nil, status.Error(codes.FailedPrecondition, "cart is checked out")
    }
    _, err = h.items.Create(ctx, &cartdv1.CartItem{
        Id:             uuid.NewString(),
        CartId:         req.GetCartId(),
        Sku:            req.GetSku(),
        Quantity:       req.GetQuantity(),
        UnitPriceCents: req.GetUnitPriceCents(),
        // account_id is stamped from tenant context by the repository.
    })
    if err != nil {
        return nil, err
    }
    return h.Repo.Get(ctx, req.GetCartId())
}
```

The embedded handler exposes the owner repository as `h.Repo`; the child repositories are the fields
you added. Two patterns carry through every custom method:

- **Return persistence sentinels, not gRPC statuses, from the repository path.** The
  `ErrorMapperUnary` interceptor maps `persistence.ErrNotFound` to `codes.NotFound` and the other
  sentinels for you. Use `status.Error` only for business-rule errors the persistence layer cannot
  express (here, "cart is checked out"). See
  [Define a service → Error handling](../define-a-service/#error-handling).
- **Pass `ctx` to every repository call.** The repository reads the tenant from context and applies
  the `account_id` scope, so the custom method never stamps the tenant itself.

When a custom method returns a projection, read it through the projection repository and compute the
derived fields in the handler:

```go {filename="cmd/cartd/handler.go"}
func (h *cartHandler) GetCartSummary(ctx context.Context, req *cartdv1.GetCartSummaryRequest) (*cartdv1.GetCartSummaryResponse, error) {
    sum, err := h.summary.Get(ctx, req.GetId()) // projection over Cart, no own table
    if err != nil {
        return nil, err
    }
    items, err := h.listItems(ctx, req.GetId())
    if err != nil {
        return nil, err
    }
    var total int64
    for _, it := range items {
        total += int64(it.GetQuantity()) * it.GetUnitPriceCents()
    }
    return &cartdv1.GetCartSummaryResponse{Summary: sum, TotalCents: total, ItemCount: int32(len(items))}, nil
}
```

## 5. Wire the custom handler through the module's `Handler` option

The generated `module/module.go` wraps `<Service>Module`, and its `<Service>ModuleOptions` carries a
`Handler` field for exactly this: set it and the module registers your handler instead of building
the default CRUD one. You grow the generated module IN PLACE — you do **not** fork a hand-written
`servicekit.Module`, so the module's `Descriptor` (methods, authz rules, resource names) still comes
from the generated proto facts for free.

Thread whatever the handler needs — here the ent client, so it can build the child and projection
repositories — through the hand-owned `Module` constructor, and set the handler as `Handler`:

```go {filename="module/module.go"}
// Module now takes the ent client so it can build the child + projection
// repositories the custom handler uses (was: a single Cart repository).
func Module(client *entclient.Client, sqlDB sdkhealth.Pinger) servicekit.Module {
    h := &cartHandler{
        items:   cartdv1.NewCartItemEntRepository(client),
        summary: cartdv1.NewCartSummaryEntRepository(client),
    }
    // The embedded CRUD handler serves the standard methods over the Cart repo.
    h.CartServiceCRUDHandler.Repo = cartdv1.NewCartEntRepository(client)

    return &cartdModule{
        inner: cartdv1.CartServiceModule(cartdv1.CartServiceModuleOptions{Handler: h}),
        db:    sqlDB,
    }
}
```

Setting `Handler` (instead of `Repo`) is the whole change. The module's `Register` then registers your
handler with `Register<Service>` — which serves both the embedded CRUD methods and your custom ones —
rather than building the CRUD-only handler over `Repo`. The generated `cartdModule` wrapper is
unchanged: it still forwards `Descriptor` to the generated inner module and registers the database
readiness check.

{{< callout type="info" >}}
**`Handler` and `Repo` are alternatives.** Set `Handler` for a service with custom or non-CRUD
methods; leave the scaffold's default `Repo` for a pure-CRUD service. `Register<Service>WithRepository`
(the `Repo` path) builds the generated CRUD-only handler and cannot serve custom methods; `Handler`
takes the plain `Register<Service>` path, which serves any `<Service>Server`. The module fails closed
at registration if neither is set. See [codegen → protoc-gen-svc](../../../reference/codegen/#protoc-gen-svc).
{{< /callout >}}

## 6. Point the host at the module

The host in `cmd/cartd/main.go` builds what `Module` now needs. Open the ent client once, keep a
`*sql.DB` on the same DSN for the readiness check, and hand both to the same `svcmodule.Module`:

```go {filename="cmd/cartd/main.go"}
return servicekit.Run(servicekit.HostConfig{
    Modules:       []servicekit.Module{svcmodule.Module(client, sqlDB)},
    GRPCAddr:      grpcAddr,
    HTTPAddr:      httpAddr,
    Authorizer:    authorizer,
    PrincipalFunc: grpcauthz.DevPrincipalFunc(),
    Logger:        slog.Default(),
    Context:       ctx,
})
```

The host still opens the database, runs the migration (now covering the child resource's model), and
owns the process — only what it hands `Module` changed. See the
[servicekit reference](../../../reference/servicekit/#hostconfig) for every `HostConfig` field.

## Verify

Build, then exercise a custom method over the HTTP gateway:

```bash
make test   # boots the host and round-trips CRUD + the custom methods
```

A custom method is authorized by the same dev grant as CRUD — present the granted identity headers:

```bash
# Create a cart, then add an item to it over the custom :addItem method.
curl -s -X POST localhost:8080/v1/carts \
  -H 'account-id: t1' -H 'groups: admin' -d '{"currency": "USD"}'

curl -s -X POST localhost:8080/v1/carts/<cart-id>:addItem \
  -H 'account-id: t1' -H 'groups: admin' \
  -d '{"sku": "sku-1", "quantity": 2, "unit_price_cents": 500}'
```

A request with no identity is denied with `403`, because the principal is empty and authz fails
closed. See [server → Make an authorized request](../../../reference/server/#make-an-authorized-request).

## Next steps

- [servicekit reference](../../../reference/servicekit/) — the `Module`, `App`, `HostConfig`, and
  `Run` API these steps use.
- [Model a resource](../model-a-resource/) — relationships, framework fields, and constraints for the
  second resource.
- [codegen → protoc-gen-svc](../../../reference/codegen/#protoc-gen-svc) — the generated handler,
  module, and `Register<Service>` helpers.
