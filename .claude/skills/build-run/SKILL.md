---
name: build-run
description: Build devedge-sdk. It is a library (no app binary) — "running" means exercising it from the toy integration tests.
---

# Build

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
