# devedge-sdk — Claude Code instructions

`devedge-sdk` is a clean, pluggable runtime SDK for Infoblox services (companion to
[devedge](https://github.com/infobloxopen/devedge)). It follows the **agentic
delivery lifecycle** defined in the development-hub's `CLAUDE.md` (Propose →
Analyze → Plan → Implement → QA → Document, with model routing and a verification
gate). The full policy lives in the hub; this file records the per-repo mechanics.

## Commit messages

NEVER add AI/LLM attribution to commit messages — no `Co-Authored-By`, no
"Generated with". Describe the change and its intent only.

## Core principles (do not regress)

- **Clean core.** No policy-engine dependency (e.g. OPA), no ORM, and no internal
  policy-model types in the core packages (`authz`, `authz/grpcauthz`,
  `persistence`). Engine / ORM / policy adapters live *outside* this module.
- **Pluggable with dev-suitable defaults.** Every seam ships a development
  implementation and a swappable interface. The API/app shape drives design;
  persistence and authz engines *serve* it (see `persistence/SHAPES.md`, `COMPAT.md`).
- **Fail closed** in authz: an undeclared method is denied; default-deny everywhere.

## Build / test / lint

- build: `make build`  (`go build ./...`)
- test:  `make test`   (`go test ./...`)
- vet:   `make vet`    (`go vet ./...`)
- lint:  `make lint`   (golangci-lint if installed, else `go vet`)

## Skills

`run-tests`, `build-run`, `verify-change` in `.claude/skills/`. Gate command:
`/verify-change`.

## Lifecycle

Spec Kit is **not** installed here yet (no `.specify/`), so `/speckit.*` phase
commands aren't available — run the phases manually and use `/verify-change` as the
QA gate. Tracked as workstream **WS-002** (authz developer experience) in the hub.
