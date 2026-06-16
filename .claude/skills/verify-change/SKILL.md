---
name: verify-change
description: QA gate for devedge-sdk — run before marking any change done. Functional (build + vet + lint + tests) and scope (diff vs acceptance criteria; keep the core clean).
---

# Verify a change (QA gate)

Both checks must pass before a change is "done".

## 1. Functional

```
make build             # all root packages compile
make vet               # go vet clean
make lint              # golangci-lint if installed, else go vet
make test              # root-module unit tests
cd testdata/toy && go test ./...    # integration tests (separate module)
make security-check    # §3.5 security assertions against toy fixture
```

Docker-gated tests (`authz/opaauthz`, Vault in `secret/`) require a running daemon.
They are skipped if `VAULT_ADDR` / `OPA_URL` are unset — state this explicitly rather
than claiming the full suite passed.

## 2. Scope

Diff the change against its acceptance criteria (spec / task / PR intent). Reject anything
that does not trace to a criterion:
- Speculative abstraction or unused extension points
- An OPA, GORM, or ORM import in the engine-neutral core (`authz/`, `authz/grpcauthz/`, `persistence/`)
- Hand-editing a generated file (`*.pb.go`, `*.svc.go`, `*.storage.go`, `*.pb.gw.go`, `*.authz.go`)
- A new RPC without an `(infoblox.authz.v1.rule)` annotation
