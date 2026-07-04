---
title: Secure
weight: 3
sidebar:
  open: true
---

This section covers the SDK's security surface: authenticating callers from verified tokens, verifying your authorization coverage in CI, and controlling how secret field values are stored, redacted, and transported. Use these guides when you need a service to know who is calling, to confirm it enforces access control correctly, or to protect sensitive data in your domain model.

For a conceptual overview of the security model, see [Security posture](../../explanation/security-posture/).

{{< cards >}}
  {{< card link="security-check/" title="Security Check" icon="shield-check" subtitle="Prove authz completeness, unknown-principal denial, tenant isolation, and no-secret-leak in CI." >}}
  {{< card link="secret-fields/" title="Secret Fields" icon="lock-closed" subtitle="How secret fields are stored (hash + cipher), redacted, and checked. Vault Transit for production." >}}
  {{< card link="add-authentication/" title="Add Authentication" icon="key" subtitle="Verify caller tokens with the authn seam so the authorizer sees a token-verified principal, and run it locally against the dev IdP." >}}
  {{< card link="call-another-service/" title="Call Another Service" icon="switch-horizontal" subtitle="Attach an outbound token: pass the bearer through within a trust domain, or exchange it for a scoped token across audiences (RFC 8693), cached." >}}
{{< /cards >}}
