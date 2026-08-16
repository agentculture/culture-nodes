# Owner decision: disposition the remaining Web/UX scope (#12)

Status: proposed decision brief, 2026-08-15.

## Decision requested

Should the graphical, on-canvas workflow editor become a new scoped project,
or should #12 close with its useful first authoring slice complete and the
graphical editor declined?

The checklist in `.owner-issues/issue-12.md` is stale relative to this
checkout. Its four formerly open deliverables now have concrete
implementations: attempt-to-run usage rollup is in
`internal/store/postgres/usage_rollup.go`; run name and description schema are
in `migrations/0013_run_metadata.sql`; workflow browsing is implemented by
`web/src/routes/NodeGraphs.tsx`; and paste/upload, validate, preview, and
publish are implemented by `web/src/routes/AuthorWorkflow.tsx` and exercised
end-to-end by `web/e2e/authoring.spec.ts`. The remaining product choice is the
issue body's explicitly later “editing steps on canvas” scope.

## Options and cost

### A — Open a successor for graphical editing

Close #12 as complete and open a narrowly named successor, “Graphical workflow
editor: mutate steps and edges on canvas.” Before implementation, define
round-trip semantics for YAML comments and ordering, immutable published
versions, validation feedback, undo/redo, accessibility, and the relationship
between source text and graph state.

Engineering and design cost are **unknown**. The cheapest estimating
experiment is a disposable prototype that loads one validated workflow IR,
adds one node and edge, serializes it, and proves that the serialized document
validates through `POST /v1alpha1/workflows/validate` as implemented in
`internal/api/workflows.go`. Record elapsed design/engineering time and the
losses in round-tripping before estimating the full editor. No external
service cost is identified by the checked-in implementation.

### B — Stop at source authoring (recommended)

Close #12 as complete and record on-canvas mutation as won't-do. Cost: no new
implementation spend; authors continue to paste or upload source and use the
existing validation, preview, and publish flow in
`web/src/routes/AuthorWorkflow.tsx`.

## Dependencies

Option A depends on an owner-approved interaction design and an ADR covering
the Phase-3 boundary called out by `.owner-issues/issue-12.md`. It also depends
on preserving the compiler as the authority (`internal/api/workflows.go`) and
the immutable publish semantics documented in
`internal/store/postgres/store.go`. Option B has no new code dependency.

## Consequence of “no”

**No means close #12 as complete and mark graphical on-canvas workflow
mutation won't-do; no generic “remaining UX” issue survives. Any later editor
proposal must be a new issue with a named user task and acceptance test.**
