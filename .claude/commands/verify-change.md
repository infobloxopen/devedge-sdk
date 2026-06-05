---
description: Run the devedge-sdk QA gate (build + vet + lint + tests, then a scope check).
---

Invoke the `verify-change` skill. Functional: `make build`, `make vet`, `make lint`,
`make test` must pass; note that no e2e layer exists yet (or, if a change adds one,
run it / say why it was skipped). Scope: diff the change against its acceptance
criteria and reject anything out of scope — especially an internal policy-model
type, a policy-engine dependency (e.g. OPA), or an ORM creeping into the clean
core (`authz`, `authz/grpcauthz`, `persistence`).
