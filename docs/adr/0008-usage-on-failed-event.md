# ADR 0008: The `failed` callback event gains an optional §13.2 usage block

- Status: accepted
- Date: 2026-08-13
- Task: t3 (attempts-evidence-humans-loops plan,
  `docs/plans/2026-08-13-attempts-evidence-humans-loops.md`)
- Issues: #32 (usage on failed attempts)
- Frame: `docs/specs/2026-08-13-attempts-evidence-humans-loops.md`, claims
  c3/c30 (scope entries s2 and s17)

## Context

The PRD (`docs/initial-design/culture-nodes-prd-spec.md`) defines the actor
protocol's usage telemetry in exactly one place: §13.2, the synchronous
`200 OK` result body, whose example carries a `usage` block with
`input_tokens`, `output_tokens`, and nullable `cost`/`currency`. §13.4 lists
the callback event kinds (`accepted`, `heartbeat`, `progress`, `artifact`,
`completed`, `failed`, `blocked`) but defines **no per-kind payload schema**
— the concrete payload shapes are `internal/actors/protocol.go`'s
construction. That construction already extended §13.2's result shape to the
async `completed`/`blocked` payload (`CompletedPayload`, protocol.go's own
doc comment: "the same shape §13.2 defines for a synchronous result"),
carrying `usage` with it.

The `failed` payload (`FailedPayload`) carried only `class`, `message`, and
`detail`. That left a reporting hole the 2026-08-13 codex-orin proof run
demonstrated live (frame scope entry s14): a bridge whose session burned
real tokens and then failed — an error or incomplete terminal result, which
all three bridge CLIs already parse usage out of — had no field to report
that burn through. The attempt row persisted NULL usage and the rollups
(migration 0012, `usage_rollup.go`) counted the attempt as non-reporting,
understating real spend on exactly the attempts most worth accounting for
(failures get retried, so their burn compounds).

## Decision

The `failed` event payload gains an **optional** `usage` block with the same
shape and semantics as §13.2's:

- `FailedPayload` (internal/actors/protocol.go) adds an optional
  `Usage *Usage` field with the JSON tag `usage,omitempty`;
- `completionFor`'s `EventFailed` branch (internal/actors/callback.go)
  converts it via the existing `Usage.ToEngine()` seam into the
  `CompletionRequest`, from where the engine's generic completion path
  persists it on the attempt row exactly as it does for `completed`.

This is an **additive amendment to PRD §13.2**, not a contradiction of it:
the PRD defines the usage block's shape in the synchronous result and is
silent on per-kind callback payload schemas, so extending the `failed`
payload with the same optional block widens the construction the PRD
delegates to protocol.go. Recording it here, rather than letting the wire
shape drift from the PRD text silently, follows this repo's ground rule
(record deviations explicitly) and the frame's compatibility claim (c30):
wire-shape fixtures and the PRD pin the protocol this batch extends.

Semantics, matching the migration-0012 no-fabricated-zero stance:

- A bridge that still holds a terminal result object when the work fails
  reports that result's real token counts (and cost, when priced).
- A crash, timeout, or cancellation that left no result object **omits the
  block entirely**, and the attempt's usage stays NULL. Zeros are never
  fabricated; an unreported attempt renders as unreported.

Compatibility is two-way: an actor that never sends `usage` on `failed`
keeps working (the field is optional and its absence converts to a nil
engine usage), and an actor that sends it to an older control plane is
merely ignored (unknown JSON keys are dropped). The conformance fixtures
(`tests/conformance`, `tests/runnerconformance`) construct `FailedPayload`
with named fields and compile and pass unchanged against the widened struct.

## Consequences

- Failed async attempts whose bridge held a terminal result now persist real
  usage on the attempt row; usage rollups stop undercounting retried-failure
  burn as those bridges adopt the field (the bridge-side emission is task
  t4; the sync 500-body path is task t5).
- Rollup consumers must not assume NULL usage means "free": it continues to
  mean "unreported", and cancelled or result-less attempts remain honestly
  unreported (the h24 narrowing, recorded with task t5's docs).
- `CompletedPayload` and `FailedPayload` now agree that usage is a §13.2
  block wherever a terminal event can carry one; any future terminal payload
  should carry the same optional block rather than inventing a variant.
