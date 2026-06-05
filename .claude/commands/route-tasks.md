---
description: Confirm [S]/[C] tags on the current feature's tasks and route them by model.
---

Ensure every task for the current feature is tagged `[S]` (simple/mechanical) or
`[C]` (complex). Untagged tasks block implementation. Dispatch `[S]` tasks to
Sonnet subagents (Agent tool, `model: sonnet`); keep `[C]` on Opus. An `[S]` task
that fails `/verify-change` is re-tagged `[C]`, redone on Opus, and the miss noted.
