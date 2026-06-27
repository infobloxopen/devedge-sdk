---
title: Aggregates
weight: 5
---

An **aggregate** is a cluster of resources that must stay consistent together and are
written as a unit. One resource is the **root** (the only entry point for writes); the
others are **members**. The classic examples: an order and its line items ("an item cannot
be added once the order is SHIPPED"); a group and its memberships ("a group must keep ≥1
admin"). The invariant spans more than one resource, so it cannot live on any single
resource's CRUD handler.

> **Status.** The aggregate machinery ships today on top of the
> [transaction seam](../transactions/): the SDK-owned `infoblox.ddd.v1` annotations
> (`aggregate` / `member` / `references`), an `AggregateRepository[Root, ID]` with
> `Load`/`Save`, a fail-closed boundary gate at `Serve`, member write-redirection in the
> generated handlers, cascade-on-delete for owned members, and `etag`-as-aggregate-version.
> Backends: ent and in-memory. GORM aggregate support is a non-goal (a `references` field is
> emitted as a plain scalar FK so the GORM backend keeps building).

## The decision test: is this an aggregate?

Apply, in order:

1. **Is there an invariant that spans more than one resource?** If a rule must hold across
   a parent and its children (a state check, a count, a sum), you have a candidate. If each
   resource is independently valid, you do not need an aggregate — plain CRUD is correct.
2. **What is the *smallest* set of resources that must be consistent in one transaction?**
   That set is the aggregate. Resist the urge to pull in everything related — only what the
   invariant actually requires. A big aggregate (an unbounded `has_many` loaded on every
   write) is the main anti-pattern.
3. **Is a resource addressable on its own for *reads*, but only mutated *through* the
   root?** Then it is a **member**: "addressable for reads, aggregate-controlled for
   writes." If it has its own independent lifecycle and its own invariants, it is its own
   aggregate (link to it by ID, not by containment).
4. **Is the relationship containment or reference?** *Containment* (the member has no life
   without the root — delete the root, delete the members) stays inside the aggregate.
   A *reference* across aggregates must be **ID-only** (a scalar foreign key, no traversable
   edge) so code cannot walk and mutate across roots.

Rule of thumb: **partition, don't aggregate, for scale.** A tenant (`account_id`) is a
*partition*, not an aggregate root — it scopes queries, it does not enforce a cross-entity
write invariant. High-cardinality children should be their own aggregate, referenced by ID.

## Declaring an aggregate

The boundary is declared with the SDK-owned `infoblox.ddd.v1` annotations (generated
locally, in-repo — see [annotations](../annotations/)):

```protobuf
import "infoblox/ddd/v1/ddd.proto";

// Order is the aggregate ROOT.
message Order {
  option (infoblox.ddd.v1.aggregate) = {root: true};

  string id    = 1;
  string state = 2;
  // Containment: the owned line items. has_many keeps the traversable edge.
  repeated Item items = 3 [(infoblox.field.v1.opts) = {has_many: {foreign_key: "order_id"}}];
  string etag  = 4 [(google.api.field_behavior) = OUTPUT_ONLY]; // the aggregate version
}

// Item is a MEMBER owned by Order. Written THROUGH the root, addressable for reads.
message Item {
  option (infoblox.ddd.v1.member) = {root: "Order"};

  string id       = 1;
  string order_id = 2;
  Order  order    = 3 [(infoblox.field.v1.opts) = {belongs_to: {foreign_key: "order_id"}}];
}
```

What the generators do with this:

- **Containment → cascade.** The `Order`→`Item` foreign key is emitted with
  `OnDelete: Cascade` (the root owns its members; deleting the order deletes its items).
  A plain `has_many`/`belongs_to` with no `member` declaration keeps the default action,
  so this is opt-in.
- **`references` → ID-only link.** A cross-aggregate pointer uses
  `(infoblox.ddd.v1.references) = {aggregate: "User", foreign_key: "user_id"}` on a
  message-typed field. It emits a **scalar FK + ID and NO traversable edge** — code cannot
  walk or mutate across roots. (Contrast `belongs_to`/`has_many`, which are within-aggregate
  containment edges.) Its FK stays restrict/`SetNull`, never cascade — a reference is not
  ownership.
- **Member write-redirection.** A member service's write-capable standard methods
  (Create/Update/Delete/Undelete) are generated as gRPC `Unimplemented` ("route through the
  root"); `Get`/`List` keep delegating to the repository.
- **Fail-closed boundary gate.** At `Serve`, `AssertAggregateBoundaries` runs beside the
  authz completeness gate: a member resource that registers a write-capable method **fails
  closed** with a clear error (the same instinct as the authz gate — an undeclared method is
  denied). Removing the write RPC (keeping only `Get`/`List`) serves.

## Load and Save: the aggregate as one unit

An `AggregateRepository[Root, ID]` loads and saves the cluster as a consistency unit:

```go
root, err := orderAgg.Load(ctx, orderID)   // root + its items, eager-loaded in one read
// ... a domain method mutates the cluster (add/remove/change a member) ...
saved, err := orderAgg.Save(ctx, root)     // one tx; member mutations + a single etag bump
```

- **Load** uses a generated graph-load primitive (`Load<Root>Aggregate`) that eager-loads
  the declared containment edges — service code never touches the ent client.
- **Save** runs in one `Atomically` transaction (commit-or-rollback as a unit), tracks
  **member mutations** (added/removed/changed members), runs the root's optional
  `Validate(ctx) error` invariant hook before persisting, and bumps the **root etag exactly
  once** on any member change. The root etag is the **aggregate version**: a stale version
  (the caller holds an old etag) fails the `Save` with `ErrPreconditionFailed`.

### Domain invariants — the `Validate` hook

A root type that implements `Validate(ctx) error` (by convention) has it called by `Save`
before any persist. Put it in a regen-safe owned file beside the generated code:

```go
// order_behavior.go (owned, not generated)
func (o *Order) Validate(_ context.Context) error {
    if o.State == "SHIPPED" && len(o.Items) > 0 {
        return status.Error(codes.FailedPrecondition, "order is shipped; cannot change items")
    }
    return nil
}
```

A violated invariant rejects the `Save` (no partial write); the error maps to a gRPC code
via the error mapper.

## Multi-surface: a read-only projection of a member is NOT a member write

A member resource may have several read surfaces — for example a WS-005 read-only
projection (a `LookupBy<Hash>` or a summary view that shares the member's table). These are
**reads**: they carry no write authority, so the boundary gate does **not** treat them as a
member write. Only a registered write-capable standard method on the member trips the gate.
Auth lookups follow the same rule: resolve an API key by its hashed secret via a projection
(`LookupBy<Field>Hash`), **not** by loading the owning aggregate.

## Worked example: IAM

`testdata/iam` proves the shape end to end:

- **`account_id` is the tenant partition**, not an aggregate root — it scopes queries
  (`TenantMixin`) and does not enforce a cross-entity write invariant.
- **`Group` is an aggregate root that owns its `Membership` members** (the rule-holding
  aggregate: "≥1 admin" lives inside the group's boundary). Memberships are written through
  the group; the group→membership FK cascades on delete.
- **`ApiKey` is its own aggregate that references a `User`** via
  `ddd.v1.references` — a scalar `user_id` FK, no edge into the user aggregate.
- **Auth lookup is a projection** (`LookupByKeyValueHash` on the secret), never an aggregate
  load.

## Caveats

- The boundary gate guards the **registered transport surface**. A handler that reaches into
  the ent client directly bypasses it — the same caveat class as the authz gate. Keep `Save`
  the sole write path for the aggregate.
- **One root per `Save`.** Cross-aggregate consistency is eventual — via the
  [transactional outbox + domain events](../events/) seam (`events.Publisher` /
  `events.Dispatcher`), not a two-aggregate transaction. Link across aggregates by
  `references` (ID only); *react* across them with events.
- **Keep aggregates small.** A high-cardinality `has_many` eager-loaded on every `Load` is
  the main anti-pattern — make such children their own aggregate, referenced by ID.
