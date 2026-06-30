---
title: Explanation
weight: 2
sidebar:
  open: true
---

This section explains how devedge-sdk works and why it is shaped the way it is. Use these pages when you want to understand the reasoning behind a design decision, the security model, or the pluggability boundary — not when you need step-by-step instructions (see [How-to](../how-to/)) or API details (see [Reference](../reference/)).

{{< cards >}}
  {{< card link="why-devedge/" title="Why devedge-sdk?" icon="light-bulb" subtitle="The service-authoring problems it addresses: secure, AIP-correct services without boilerplate." >}}
  {{< card link="pluggability/" title="Pluggability model" icon="puzzle" subtitle="How the dep-light core and adapter seam keep your service code vendor-neutral." >}}
  {{< card link="security-posture/" title="Security posture" icon="shield-check" subtitle="Fail-closed authorization, multi-tenancy enforcement, and the provable security invariants the SDK upholds." >}}
  {{< card link="adding-an-isolated-module/" title="Adding an isolated module" icon="cube" subtitle="Maintainer checklist for carving a heavy component into its own nested Go module without breaking the dep graph." >}}
{{< /cards >}}

For runtime internals — the interceptor chain, codegen pipeline, and layer model — see [Concepts](../concepts/).
