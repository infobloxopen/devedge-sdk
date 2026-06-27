---
title: Guides
weight: 3
sidebar:
  open: true
---

Task-focused how-tos for common service-building jobs.

{{< cards >}}
  {{< card link="define-a-service/" title="Define a Service" icon="code" subtitle="proto → buf generate → scaffold." >}}
  {{< card link="model-a-resource/" title="Model a Resource" icon="cube" subtitle="Resource shape: field types, framework fields, constraints, relationships, secret fields." >}}
  {{< card link="storage-shapes/" title="Storage Shapes" icon="database" subtitle="GORM vs ent — when to use each." >}}
  {{< card link="security-check/" title="Security Check" icon="shield-check" subtitle="Prove authz, isolation, and no-secret-leak in CI." >}}
{{< /cards >}}

### Production secrets

Secret-at-rest works out of the box (AES-256-GCM in dev); for production, swap in HashiCorp Vault.

{{< cards >}}
  {{< card link="vault-transit/" title="Vault Transit" icon="lock-closed" subtitle="Optional: production secret handling with Vault." >}}
{{< /cards >}}
