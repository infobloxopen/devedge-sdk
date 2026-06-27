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

> **Status.** This page is a decision guide. The transaction seam this builds on ships
> today — see [Transactions](../transactions/). The full aggregate machinery (the
> `infoblox.ddd.v1` annotations, `AggregateRepository`, the fail-closed boundary gate, and
> member write-redirection) is a sequenced follow-up; the forward pointer at the bottom
> tracks it.

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

## Atomic check-then-write today

Until the aggregate machinery lands, the invariant is enforced with the transaction seam
directly: load the root, check the invariant, write the member — atomically. See the
[atomic check-then-write recipe](../transactions/#atomic-check-then-write-recipe).

```go
err := txRunner.Atomically(ctx, func(ctx context.Context) error {
    order, err := orders.Get(ctx, orderID)
    if err != nil {
        return err
    }
    if order.State == "SHIPPED" {
        return status.Error(codes.FailedPrecondition, "order is shipped; cannot add items")
    }
    _, err = items.Create(ctx, item)
    return err
})
```

## Coming next

A future release adds the SDK-owned aggregate machinery on top of this seam:

- `infoblox.ddd.v1` annotations (`aggregate` / `member`) and an ID-only `references`
  annotation, generated locally in-repo;
- an `AggregateRepository[Root, ID]` with `Load`/`Save`;
- a **fail-closed boundary gate** at `Serve` (a member with no declared root is a boot
  error — the same instinct as the authz completeness gate);
- **member write-redirection** in the generated handlers (a member's direct
  Create/Update/Delete is suppressed — "route through the root");
- cascade-on-delete for owned members, and `etag`-as-aggregate-version.

Until then, model aggregates with the transaction seam and the decision test above.
