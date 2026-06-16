---
name: run-tests
description: Run devedge-sdk's tests (SDK-internal — for maintainers of the SDK repo). Use when verifying a change or before committing. Building a service that IMPORTS the SDK? run that module's own `go test`, not these make targets.
---

# Run tests

> **Audience: devedge-sdk maintainers.** These `make` targets run inside the SDK repo. If you are
> building a *service that imports* devedge-sdk, follow the docs quickstart
> (`docs/content/docs/getting-started/quickstart.md`) and run your own module's tests instead.

## Root module (fast, no external services)

```
make test              # go test ./...
make vet               # go vet ./...
```

Single package or test:

```
go test ./lro/... -run TestManager_Cancel -v
go test ./middleware/... -v
```

## Integration tests (separate Go module)

```
cd testdata/toy && go test ./...
cd testdata/apikey && go test ./...
```

Always run these after any proto, middleware, or server change — they are the
integration gate, not covered by `make test`.

## Security gate

```
make security-check    # go test ./testdata/toy -run Security -v
```

## Docker-gated tests

`authz/opaauthz` (OPA round-trip) and `secret/` (Vault Transit) require
running daemons. They self-skip when `OPA_URL` / `VAULT_ADDR` are unset.
Run them in the Docker-enabled CI environment; skip locally and say so.
