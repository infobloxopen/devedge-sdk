---
title: devedge-sdk
layout: hextra-home
---

{{< hextra/hero-badge >}}
  <div class="hx:w-2 hx:h-2 hx:rounded-full hx:bg-primary-400"></div>
  <span>Early — APIs will change</span>
  {{< icon name="arrow-circle-right" attributes="height=14" >}}
{{< /hextra/hero-badge >}}

<div class="hx:mt-6 hx:mb-6">
{{< hextra/hero-headline >}}
  Build Infoblox services&nbsp;<br class="hx:sm:block hx:hidden" />the contract-first way
{{< /hextra/hero-headline >}}
</div>

<div class="hx:mb-12">
{{< hextra/hero-subtitle >}}
  A modern Go service framework. Declare authorization and secrets&nbsp;<br class="hx:sm:block hx:hidden" />once in your proto — the framework enforces them everywhere.
{{< /hextra/hero-subtitle >}}
</div>

<div class="hx:mb-6">
{{< hextra/hero-button text="Get Started" link="docs/getting-started/" >}}
</div>

<div class="hx:mt-6"></div>

{{< hextra/feature-grid >}}
  {{< hextra/feature-card
    title="Declare authz once, enforced everywhere"
    subtitle="Annotate an RPC with (infoblox.authz.v1.rule). The framework builds the per-method rule table, enforces it fail-closed, and refuses to boot if any served method is undeclared."
    icon="lock-closed"
  >}}
  {{< hextra/feature-card
    title="Secret fields encrypted at rest"
    subtitle="Mark a field secret in proto. Generated storage code hashes it for lookup and encrypts the ciphertext — AES-256-GCM in dev, HashiCorp Vault Transit in prod. Plaintext is never persisted and never returned."
    icon="key"
  >}}
  {{< hextra/feature-card
    title="Cross-account tenant isolation"
    subtitle="Every query is scoped by account-id at the storage layer — in both GORM and ent. Principal B can never see Principal A's resources, and seccheck proves it in CI."
    icon="shield-check"
  >}}
  {{< hextra/feature-card
    title="Batteries-included gRPC server"
    subtitle="server.New assembles the interceptor chain — request-ID, error mapping, tenant-ID, fail-closed authz, field-mask validation, ETag/412 preconditions — plus an optional HTTP/JSON gateway."
    icon="server"
  >}}
  {{< hextra/feature-card
    title="Codegen from your proto"
    subtitle="protoc-gen-svc scaffolds the service, protoc-gen-storage emits a GORM repository, protoc-gen-ent emits an ent schema. The proto is the single source of truth."
    icon="cog"
  >}}
  {{< hextra/feature-card
    title="Pluggable, dependency-light core"
    subtitle="Core packages depend only on the standard library. No ORM, no policy-engine dependency. Every seam ships a dev-suitable default and swaps for a production backend without touching service code."
    icon="puzzle"
  >}}
{{< /hextra/feature-grid >}}
