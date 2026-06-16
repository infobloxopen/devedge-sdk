---
name: build-run
description: Build devedge-sdk (SDK-internal — for maintainers of the SDK repo). It is a library (no app binary) — "running" means exercising it from the toy integration tests. Building/running a service that USES the SDK? follow the docs quickstart, not these make targets.
---

# Build

> **Audience: devedge-sdk maintainers.** These targets build the SDK repo itself. If you are
> building a *service that imports* devedge-sdk, follow the docs quickstart
> (`docs/content/docs/getting-started/quickstart.md`); your service has its own `main`, `buf`, and
> `go build`.

```
make build             # go build ./... (root module)
make generate          # rebuild generated files after any .proto change
```

devedge-sdk is a **library** — there is no long-running server to start. The toy
`WidgetService` is the closest thing to a runnable target; the integration tests
start and stop it inline:

```
cd testdata/toy && go test ./... -v -run TestIntegration
```

## After proto changes

Always regenerate and rebuild in order:

```
make generate          # runs buf + all protoc-gen-* plugins + go mod tidy
make build             # verify generated code compiles
cd testdata/toy && go test ./...   # verify integration tests still pass
```

## Module tidy

```
make tidy              # go mod tidy (root)
cd testdata/toy && go mod tidy
cd testdata/apikey && go mod tidy
```
