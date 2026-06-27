---
title: Secure
weight: 3
sidebar:
  open: true
---

The SDK's security surface: how to prove invariants in CI and how secret fields are handled end-to-end.

For the conceptual overview of the security model see [Security Posture](../../explanation/security-posture/).

{{< cards >}}
  {{< card link="security-check/" title="Security Check" icon="shield-check" subtitle="Prove authz completeness, unknown-principal denial, tenant isolation, and no-secret-leak in CI." >}}
  {{< card link="secret-fields/" title="Secret Fields" icon="lock-closed" subtitle="How secret fields are stored (hash + cipher), redacted, and checked. Vault Transit for production." >}}
{{< /cards >}}
