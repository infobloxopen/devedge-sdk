---
title: Aggregates
weight: 5
---

An **aggregate** is a cluster of resources that must stay consistent together and are
written as a unit. One resource is the **root** — the only entry point for writes. The
others are **members**. Use aggregates when a business invariant spans more than one
resource and that invariant must hold within a single transaction.

Classic examples: an order and its line items ("an item cannot be added once the order is
SHIPPED"); a group and its memberships ("a group must keep ≥1 admin"). The invariant spans
more than one resource, so it cannot live on any single resource's CRUD handler.

## Deciding whether to use an aggregate

Work through these questions in order:

1. **Is there an invariant that spans more than one resource?** If a rule must hold across
   a parent and its children — a state check, a count, a sum — you have a candidate. If each
   resource is independently valid, plain CRUD is correct.
2. **What is the smallest set of resources that must be consistent in one transaction?**
   That set is the aggregate. Include only what the invariant actually requires. An aggregate
   that loads an unbounded `has_many` on every write is the main anti-pattern.
3. **Is a resource addressable on its own for reads, but only mutated through the root?**
   If yes, it is a **member**: readable independently, but written only through the root. If
   it has its own independent lifecycle and its own invariants, it is its own aggregate — link
   to it by ID, not by containment.
4. **Is the relationship containment or reference?** *Containment* means the member has no
   life without the root (delete the root, delete the members). A *reference* across
   aggregates uses an ID-only scalar foreign key with no traversable edge, so code cannot
   walk or mutate across roots.

Partition for scale; do not use an aggregate for it. A tenant (`account_id`) is a
*partition*, not an aggregate root. It scopes queries but does not enforce a cross-entity
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

The generators produce the following from these annotations:

- **Containment → cascade.** The `Order`→`Item` foreign key is emitted with
  `OnDelete: Cascade` (the root owns its members; deleting the order deletes its items).
  A plain `has_many`/`belongs_to` with no `member` declaration keeps the default action,
  so this is opt-in.
- **`references` → ID-only link.** A cross-aggregate pointer uses
  `(infoblox.ddd.v1.references) = {aggregate: "User", foreign_key: "user_id"}` on a
  message-typed field. It emits a scalar FK and ID field with no traversable edge, so code
  cannot walk or mutate across roots. Contrast `belongs_to`/`has_many`, which are
  within-aggregate containment edges. The FK action stays restrict or `SetNull`, never
  cascade, because a reference is not ownership.
- **Member write-redirection.** A member service's write-capable standard methods
  (Create/Update/Delete/Undelete) are generated as gRPC `Unimplemented`, signaling that
  clients should route through the root. `Get` and `List` keep delegating to the repository.
- **Fail-closed boundary gate.** At `Serve`, `AssertAggregateBoundaries` runs beside the
  authz completeness gate: a member resource that registers a write-capable method fails
  closed with a clear error. Removing the write RPC (keeping only `Get`/`List`) resolves
  the error.

## Loading and saving

An `AggregateRepository[Root, ID]` loads and saves the cluster as a consistency unit:

```go
root, err := orderAgg.Load(ctx, orderID)   // root + its items, eager-loaded in one read
// ... a domain method mutates the cluster (add/remove/change a member) ...
saved, err := orderAgg.Save(ctx, root)     // one tx; member mutations + a single etag bump
```

- **Load** uses a generated graph-load primitive (`Load<Root>Aggregate`) that eager-loads
  the declared containment edges — service code never touches the ent client directly.
- **Save** runs in one `Atomically` transaction (commit-or-rollback as a unit), tracks
  member mutations (added, removed, or changed members), runs the root's optional
  `Validate(ctx) error` invariant hook before persisting, and bumps the root etag exactly
  once on any member change. The root etag is the **aggregate version**: a caller holding an
  outdated etag receives `ErrPreconditionFailed`.

The aggregate machinery is built on the [transaction seam](../transactions/). It runs on
three backends: ent, GORM, and in-memory. Both ent and GORM support the same surface — a
tx-aware generated repository (`conn(ctx)`), the `Load<Root>Aggregate` eager-load, and
cascade-on-delete. On GORM, cascade is expressed with cascade-on-delete tags through the
`persistence/gormtx` adapter (`GormTxRunner`, `GormOutboxStore`, `GormIdempotencyStore`),
wired through the same backend-neutral seams. The IAM fixture exercises the transactional-
outbox worked example on both GORM and ent.

### Domain invariants

A root type that implements `Validate(ctx) error` has it called by `Save` before any
persist. The SDK detects this method by convention at runtime — it is duck-typed, not a
compiled interface the root must declare. Place it in a regen-safe owned file beside the
generated code:

```go
// order_behavior.go (owned, not generated)
func (o *Order) Validate(_ context.Context) error {
    if o.State == "SHIPPED" && len(o.Items) > 0 {
        return status.Error(codes.FailedPrecondition, "order is shipped; cannot change items")
    }
    return nil
}
```

A violated invariant rejects the `Save` with no partial write. The error maps to a gRPC
code via the error mapper.

## Read-only projections on members

A member resource may have several read surfaces — for example a read-only projection
(a `LookupBy<Hash>` or a summary view that shares the member's table). These are reads:
they carry no write authority, so the boundary gate does not treat them as member writes.
Only a registered write-capable standard method on the member trips the gate.

Auth lookups follow the same rule: resolve an API key by its hashed secret via a projection
(`LookupBy<Field>Hash`), not by loading the owning aggregate.

## Worked example: IAM

`testdata/iam` demonstrates the pattern end to end:

- **`account_id` is the tenant partition**, not an aggregate root — it scopes queries
  (`TenantMixin`) and does not enforce a cross-entity write invariant.
- **`Group` is an aggregate root that owns its `Membership` members** (the rule-holding
  aggregate: "≥1 admin" lives inside the group's boundary). Memberships are written through
  the group; the group→membership FK cascades on delete.
- **`ApiKey` is its own aggregate that references a `User`** via
  `ddd.v1.references` — a scalar `user_id` FK with no edge into the user aggregate.
- **Auth lookup is a projection** (`LookupByKeyValueHash` on the secret), never an aggregate
  load.

## Failure modes and constraints

{{< callout type="warning" >}}
**Keep `Save` the sole write path for the aggregate.** The boundary gate guards the
registered transport surface. A handler that reaches into the ent client directly bypasses
it — any direct ent-client use, not only writes — the same risk class as bypassing the authz
gate.
{{< /callout >}}

{{< callout type="warning" >}}
**Each `Save` operates on one root.** Cross-aggregate consistency is eventual: use the
[transactional outbox and domain events](../events/) seam (`events.Publisher` /
`events.Dispatcher`), not a two-aggregate transaction. Link across aggregates with
`references` (ID only) and react across them with events.
{{< /callout >}}

{{< callout type="info" >}}
**Keep aggregates small.** A high-cardinality `has_many` eager-loaded on every `Load`
degrades write performance. Make such children their own aggregate, referenced by ID.
{{< /callout >}}
