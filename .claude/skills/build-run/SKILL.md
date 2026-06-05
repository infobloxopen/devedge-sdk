---
name: build-run
description: Build the devedge-sdk module. It is a library (no app binary to run) — "running" means exercising it from tests or a consumer.
---

# Build

    make build         # go build ./...

devedge-sdk is a **library** imported by services, not an executable — there is
no server or CLI to start. To smoke-test a seam (the gRPC authz interceptor, the
persistence repository), write or extend a test rather than launching a process:

    make test

Tidy modules after changing imports:

    make tidy          # go mod tidy
