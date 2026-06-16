---
name: add-annotation
description: Extend the SDK annotation contract with a new proto field or method annotation. Requires apx + both canonical apis repos.
---

# Extend the annotation contract

## What the annotation contract is

Two canonical proto annotations ship with the SDK (`authz.v1.rule`, `field.v1.rule`). Adding a new
type is a cross-repo operation that changes what every downstream service imports.

## Steps

**1. Reserve an extension number** before any broad use.
Current placeholders: `50001` (authz rule), `50002` (field rule). Coordinate with the governance
owner for a permanent slot before publishing.

**2. Write the proto locally** in `proto/infoblox/<domain>/v1/<domain>.proto`. Keep the local copy
as a buf codegen input only — it must not become an importable Go package competing with the
canonical one (two protoregistry registrations panic at `init`).

**3. Run `make generate`** to verify the local bindings compile and the existing tests still pass.

**4. Submit to both canonical repos** via apx:
- `Infoblox-CTO/apis` (internal) and `infobloxopen/apis` (public)
- Pipeline: `apx prepare → PR → merge → CI finalize (auto-tags)`
- Watch for the `apx-tag-protection` ruleset: the CI GitHub App must be a bypass actor or the
  automated tag push will fail (GH013).

**5. Update the SDK** to consume the new tag:
```
go get github.com/infobloxopen/apis/proto/infoblox/<domain>@<new-tag>
go mod tidy
```
Delete the local generated `*.pb.go` that duplicates the canonical package.

**6. Write seccheck assertions** if the annotation has security implications (e.g. a `secret`-like
field that must not appear in responses).

**7. Update `AGENTS.md`** annotation-contract table.

## Known gotchas

- `apx breaking --against <tag>` misclassifies slash-containing release tags as filesystem paths →
  spurious BREAKING_CHANGE on every second release. Fixed in apx v0.12.1.
- An empty-diff PR (e.g. a second alpha with no content change) causes `apx submit` to return 422.
  Finalize locally: `apx finalize --ci --api ... --version ... --commit ...`.
