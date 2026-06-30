---
title: Concepts
weight: 6
sidebar:
  open: true
---

Core runtime and domain model concepts for devedge-sdk. Each page explains a key building block: what it is, how it works, and when to use it.

For higher-level design rationale, see [Explanation](../explanation/).

{{< cards >}}
  {{< card link="architecture/" title="Architecture" icon="template" subtitle="The three layers and where the SDK sits." >}}
  {{< card link="annotations/" title="Annotations" icon="tag" subtitle="The (infoblox.authz.v1.rule) and field.secret proto annotation contract." >}}
  {{< card link="tenant-isolation/" title="Tenant Isolation" icon="user-group" subtitle="Multi-tenancy enforcement at the storage layer." >}}
  {{< card link="transactions/" title="Transactions" icon="refresh" subtitle="The TxRunner seam and atomic multi-step writes." >}}
  {{< card link="aggregates/" title="Aggregates" icon="collection" subtitle="Domain-driven design aggregates: root, members, and consistency boundaries." >}}
  {{< card link="events/" title="Events" icon="bell" subtitle="Transactional outbox and domain events." >}}
{{< /cards >}}
