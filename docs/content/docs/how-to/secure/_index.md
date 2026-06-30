---
title: Secure
weight: 3
sidebar:
  open: true
---

This section covers the two parts of the SDK's security surface: verifying your authorization coverage in CI, and controlling how secret field values are stored, redacted, and transported. Use these guides when you need to confirm that your service enforces access control correctly or when you need to protect sensitive data in your domain model.

For a conceptual overview of the security model, see [Security posture](../../explanation/security-posture/).

{{< cards >}}
  {{< card link="security-check/" title="Security Check" icon="shield-check" subtitle="Prove authz completeness, unknown-principal denial, tenant isolation, and no-secret-leak in CI." >}}
  {{< card link="secret-fields/" title="Secret Fields" icon="lock-closed" subtitle="How secret fields are stored (hash + cipher), redacted, and checked. Vault Transit for production." >}}
{{< /cards >}}
