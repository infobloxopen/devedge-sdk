---
name: verify-change
description: The QA gate for devedge-sdk — run before marking any change done. Functional (build + vet + lint + tests) and scope (diff vs acceptance criteria; keep the core clean).
---

# Verify a change (QA gate)

Both checks must pass before a change is "done".

## 1. Functional

    make build         # compiles
    make vet           # go vet clean
    make lint          # golangci-lint if installed, else go vet
    make test          # all unit tests green

- **e2e**: no e2e/integration layer exists yet (the SDK is a pure-Go library; its
  tests cross no real I/O boundary). When a change adds one that does (e.g. the
  planned ephemeral-OPA integration test), run it and report the result. If it
  can't run locally (no Docker), say it was skipped and why — never claim it passed.

## 2. Scope

Diff the change against its acceptance criteria (spec / task / PR intent). Reject
anything that doesn't trace to a criterion — speculative abstraction, gold-plating,
or — specific to this repo — **an internal policy-model type, a policy-engine
dependency (e.g. OPA), or an ORM leaking into the clean core** (`authz`,
`authz/grpcauthz`, `persistence`). The core stays engine-neutral; adapters live
outside the module.
