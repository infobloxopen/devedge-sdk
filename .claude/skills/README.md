# devedge-sdk skills

Project-specific mechanics for the agentic delivery lifecycle. The *policy* lives
in the development-hub's `CLAUDE.md`; these skills are the per-repo *how*.

Each skill is YAML frontmatter (`name`, `description`) + a lean, command-first body.

- `run-tests` — how to test (layers + exact commands).
- `build-run` — how to build (this is a library; there is no app to run).
- `verify-change` — the QA gate (functional + scope).
- `change-database-schema` — evolve a service's DB safely: sequential migrations, zero-downtime
  recipes (Postgres + MySQL), and how they run unattended.
- `define-slo` — author a GOOD SLI/SLO: the three layers, good/valid with a client-fault-excluded
  denominator, a measured target, the error budget, the mandatory error-budget policy, and the
  `de slo`/`slogen` verbs that lint + render it.

Convention: when a mechanical step repeats, promote it to a skill here. Keep them
lean — commands over prose.
