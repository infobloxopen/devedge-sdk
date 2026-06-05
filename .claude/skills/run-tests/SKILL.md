---
name: run-tests
description: Run devedge-sdk's tests. Use when verifying a change or before committing.
---

# Run tests

Unit tests — all packages, fast, no external services:

    make test          # go test ./...

Single package / single test:

    go test ./authz/...
    go test ./authz/grpcauthz -run TestUndeclaredMethodDeniedByDefault -v

Vet (part of the gate):

    make vet           # go vet ./...

## Layers

- **Unit**: every current test is pure Go — no Docker, no network. Fast.
- **Integration / e2e**: none yet. The authz DX POC will add an ephemeral-OPA
  integration test (testcontainers + Docker); until that lands there is no e2e
  layer to run.
