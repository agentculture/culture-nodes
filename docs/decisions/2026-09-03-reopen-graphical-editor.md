# Decision: reopen the graphical workflow editor (supersedes the 2026-08-15 issue-12 brief)

Status: decided by the operator on 2026-09-03 (frame `web-ui-lift`, question q1;
spike verdict go in `docs/decisions/2026-09-03-design-canvas-spike.md`).

## What this supersedes

`docs/decisions/issue-12-remaining-web-ux-scope.md` (2026-08-15) recommended
option B and ruled, verbatim:

> No means close #12 as complete and mark graphical on-canvas workflow
> mutation won't-do; no generic "remaining UX" issue survives. Any later editor
> proposal must be a new issue with a named user task and acceptance test.

and, for option A:

> The cheapest estimating experiment is a disposable prototype that loads one
> validated workflow IR, adds one node and edge, serializes it, and proves that
> the serialized document validates through `POST /v1alpha1/workflows/validate`
> as implemented in `internal/api/workflows.go`. Record elapsed design/engineering
> time and the losses in round-tripping before estimating the full editor.

Both conditions are now met, so the won't-do is reopened rather than drifted
past:

- **The named user task and acceptance test** live in a Feature issue opened
  alongside this record: *compose a workflow on canvas and publish it*, accepted
  when a canvas-authored workflow's published digest equals the CLI publish path
  on the same emitted source and every diagnostic shown on canvas is
  byte-identical to the validate response (spec success signal c33, honesty h23).
- **The estimating experiment** ran as plan task t12 on 2026-09-03 (elapsed
  1 minute 59 seconds of machine time in the sandbox, plus the operator's offline
  compile at the gate): three fixtures round-trip byte-identically with
  `flowCollectionPadding: false`, one added node and one added edge change only
  the inserted lines, and the mutated document compiles with 0 errors.

## What changed since August that makes the editor buildable

- The spec (`docs/specs/2026-09-03-web-ui-lift.md`) fixes the semantics the brief
  said were undefined: the canvas edits the author's YAML as a syntax tree, so
  comments, blank lines, anchors and key order survive (c28, h10); published
  versions stay immutable and digest-keyed, and a comment-only edit publishes
  nothing while telling the author why (decision c42, question q4).
- The compiler stays the authority: the canvas ships the document string
  verbatim to the existing `POST /v1alpha1/workflows` and adds no write route
  (boundary c39, h31).
- A shared Culture node (t3) and the Design gallery that opens a version's
  stored source (t8) already ship on the same branch, so the editor is a lens
  over text the app already holds.

## What stays out

- No YAML re-serialization from the IR in the editor path.
- No new write route or role; the viewer refusal is the same as on the text page.
- The Phase-3 boundary the August brief named (source text versus graph state)
  is answered by "text is the truth, the canvas is a lens", not widened.

## Issues

- Record: this file, pointed at by the Record issue opened with
  `scripts/open-issue.sh --type Record`.
- Feature: the user task above, opened with `scripts/open-issue.sh --type
  Feature`; the editor PR links both.
- Issue #12 is re-scoped by this record: its "editing steps on canvas" item is
  now the Feature issue; its other items closed in v0.11.x.
