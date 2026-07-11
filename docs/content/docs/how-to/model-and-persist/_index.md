---
title: Model & Persist
weight: 2
sidebar:
  open: true
---

This section covers how to define your service contract, describe your data model, and choose a storage backend. Use these guides when you are setting up a new resource or deciding how to persist it.

{{< cards >}}
  {{< card link="define-a-service/" title="Define a service" icon="code" subtitle="Write a proto definition, run buf generate, and scaffold the service." >}}
  {{< card link="model-a-resource/" title="Model a resource" icon="cube" subtitle="Field types, framework fields, constraints, relationships, and secret fields." >}}
  {{< card link="storage-shapes/" title="Storage shapes" icon="database" subtitle="GORM vs ent — when to use each." >}}
  {{< card link="custom-methods/" title="Add a custom method or second resource" icon="puzzle" subtitle="Grow a scaffolded service: a custom RPC and a second resource, wired through the servicekit module and host." >}}
  {{< card link="change-the-database-schema/" title="Change the database schema" icon="adjustments" subtitle="Author a versioned, reversible migration with de migrate — safe on a large, live database." >}}
  {{< card link="add-full-text-search/" title="Add full-text search" icon="search" subtitle="Mark fields and calculated sources searchable, choose JIT or INDEXED, and query with q." >}}
{{< /cards >}}
