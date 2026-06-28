---
title: Explanation
weight: 2
sidebar:
  open: true
---

Conceptual background for understanding devedge-sdk — why it exists, how it is shaped, and what its security model means.

{{< cards >}}
  {{< card link="why-devedge/" title="Why devedge-sdk?" icon="light-bulb" subtitle="The problem it solves: secure, AIP-correct services without boilerplate." >}}
  {{< card link="pluggability/" title="Pluggability Model" icon="puzzle" subtitle="Dep-light core + adapters: how the seam model keeps your code vendor-neutral." >}}
  {{< card link="security-posture/" title="Security Posture" icon="shield-check" subtitle="Fail-closed authz, multi-tenancy, and provable security invariants." >}}
  {{< card link="adding-an-isolated-module/" title="Adding an Isolated Module" icon="cube" subtitle="Maintainer checklist for carving a heavy component into its own nested Go module." >}}
{{< /cards >}}

For the runtime internals (the interceptor chain, codegen pipeline, layer model), see [Concepts](../concepts/).
