---
title: Add a custom method or second resource
weight: 4
---

This page shows how to grow a scaffolded single-resource service into one that serves custom
(non-CRUD) RPCs and a second resource, wired through the `servicekit` host the scaffold already
generates. You start from a `devedge-sdk new service` project, add a custom RPC and its handler
alongside the generated CRUD, add a child resource and its repository, and register all of it as one
`servicekit.Module`.

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
`<Service>CRUDHandler` over your repository through `Register<Service>WithRepository`, and that is
all `Register` does. To add custom methods or a second resource, you replace that generated module
with a hand-written one — `servicekit.Module` is the seam for it.

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

## 5. Replace the generated module with a hand-written one

The generated `module/module.go` wraps `<Service>Module`, which serves only the single-resource CRUD
path. Replace it with a hand-written `servicekit.Module` whose `Register` builds the custom handler
over all three repositories and registers it with `Register<Service>` (not
`Register<Service>WithRepository`).

The cart host carries the module in `cmd/cartd/main.go`:

```go {filename="cmd/cartd/main.go"}
type cartdModule struct {
    client *entclient.Client
    db     sdkhealth.Pinger
}

func (m *cartdModule) Descriptor() servicekit.Descriptor {
    return servicekit.Descriptor{
        ID: "cartd",
        Methods: []string{
            cartdv1.CartService_CreateCart_FullMethodName,
            cartdv1.CartService_GetCart_FullMethodName,
            cartdv1.CartService_ListCarts_FullMethodName,
            cartdv1.CartService_UpdateCart_FullMethodName,
            cartdv1.CartService_DeleteCart_FullMethodName,
            cartdv1.CartService_AddItem_FullMethodName,
            cartdv1.CartService_RemoveItem_FullMethodName,
            cartdv1.CartService_Checkout_FullMethodName,
            cartdv1.CartService_GetCartSummary_FullMethodName,
        },
        AuthzRules: cartdv1.CartServiceAuthzRules,
        Resources: []servicekit.ResourceDescriptor{
            {Name: "cartd.cart"}, {Name: "cartd.cartitem"},
        },
    }
}

func (m *cartdModule) Register(_ context.Context, app *servicekit.App) error {
    h := &cartHandler{
        items:   cartdv1.NewCartItemEntRepository(m.client),
        summary: cartdv1.NewCartSummaryEntRepository(m.client),
    }
    h.CartServiceCRUDHandler.Repo = cartdv1.NewCartEntRepository(m.client)
    if err := cartdv1.RegisterCartService(app.Server, h); err != nil {
        return err
    }
    if m.db != nil {
        return app.Health.Register("cartd.db", sdkhealth.NewDBCheck("cartd.db", m.db))
    }
    return nil
}
```

Three things make this module serve the custom surface:

1. **`Descriptor.Methods` lists every served method** — the generated CRUD plus the custom RPCs —
   using the generated `<Service>_<Method>_FullMethodName` constants. The host uses this list to
   detect duplicate service names across a composition.
2. **`Descriptor.AuthzRules` is `<Service>AuthzRules`** — the full generated rule table, including the
   custom methods' rules. `RegisterCartService` contributes the same rules to the server, and the
   boot gate checks them.
3. **`Register` builds the custom handler and registers it with `RegisterCartService(app.Server, h)`**
   — the plain `Register<Service>` that takes a handler, not the repository. It constructs all three
   generated repositories from the ent client and assigns the owner repository to the embedded
   `CRUDHandler.Repo`.

{{< callout type="info" >}}
**Use `Register<Service>`, not `Register<Service>WithRepository`, for a custom handler.** The
`...WithRepository` helper builds the generated CRUD-only handler over one repository — it cannot
serve your custom methods. `Register<Service>(app.Server, h)` registers your handler, which has both
the embedded CRUD methods and the custom ones. See
[codegen → protoc-gen-svc](../../../reference/codegen/#protoc-gen-svc).
{{< /callout >}}

## 6. Point the host at the new module

In `runHost`, hand `servicekit.Run` the hand-written module instead of `svcmodule.Module(...)`:

```go {filename="cmd/cartd/main.go"}
return servicekit.Run(servicekit.HostConfig{
    Modules:       []servicekit.Module{&cartdModule{client: client, db: sqlDB}},
    GRPCAddr:      grpcAddr,
    HTTPAddr:      httpAddr,
    Authorizer:    authorizer,
    PrincipalFunc: grpcauthz.DevPrincipalFunc(),
    Logger:        slog.Default(),
    Context:       ctx,
})
```

The host still opens the database, runs the migration (now covering the child resource's model), and
owns the process — only the module changed. See the
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
