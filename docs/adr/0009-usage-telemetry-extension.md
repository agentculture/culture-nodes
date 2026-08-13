# ADR 0009: §13.2 usage gains four optional fields, and a termination reason beside it

- Status: accepted
- Date: 2026-08-13
- Task: t1 (economy-discord-graphs plan,
  `docs/plans/2026-08-13-economy-discord-graphs.md`)
- Issues: #47 (cold-session tax and dropped cache telemetry), #48 (economic
  contract)
- Frame: `docs/specs/2026-08-13-economy-discord-graphs.md`, claim c2 and
  honesty h1 (scope entries s2 and s21)

## Context

The PRD defines actor usage telemetry in one place: §13.2's synchronous
result body, whose `usage` block carries `input_tokens`, `output_tokens`, and
nullable `cost`/`currency`. `migrations/0012_attempt_usage.sql` gave those
four fields four nullable columns on `attempts`, and ADR 0008 extended the
same block additively to the `failed` callback event.

Those four fields cannot answer the question the economy work exists to
answer. The live fan-out that motivated this plan exhausted a five-hour
provider session window mid-wave, and the diagnosis (#47, #48) turned on
cache economics: what fraction of the input tokens a turn billed were cache
reads. That fraction is reported by the providers and dropped by us —
scope entry s21 pins a bridge fixture reporting 9984 cached of 13880 input
tokens with nowhere for the 9984 to go. Three further facts are missing for
the same reason: reasoning tokens (billed, uncounted), the model that
produced the counts (tokens are neither comparable nor priceable across
models, so a rollup summing them without it sums different units), and the
provider-side thread the usage accrued on (without it, "this workstream
reused one warm session" is an assumption, not a measurement).

A fifth fact is missing and is not a usage field at all: how the turn ended
as the provider reported it — an output-token cap, a context-window stop, a
cancellation.

## Decision

### 1. §13.2's usage block gains four optional fields

`cached_input_tokens`, `reasoning_tokens`, `model`, and `thread_id` join the
block, in `internal/actors.Usage`, `engine.Usage`, and four new nullable
columns (`migrations/0017_attempt_usage_extended.sql`). This is an
**additive amendment to §13.2** of exactly ADR 0008's kind: the PRD's example
body is widened, no existing field changes meaning, an actor that sends none
of them keeps working, and an actor that sends them to an older control
plane is merely ignored (unknown JSON keys are dropped).

Every one of the four is a pointer, and each is independently absent-able
*within* a reported block, for the reason §13.2 already makes `cost` and
`currency` nullable: an actor whose contract exposes no cache telemetry
reports null, which means "unmeasurable", where a 0 would claim a measured
0% cache ratio. So:

- `usage_input_tokens IS NOT NULL` remains the one "this attempt reported
  usage" sentinel (migration 0012). None of the four new columns may stand
  in for it, and a consumer must not infer the block's presence from any of
  them.
- `thread_id` is telemetry, not a resume handle. The handle a later dispatch
  passes back to a bridge is `continuation_ref` (task t4), a separate field
  with a separate life cycle; neither is derived from the other.

### 2. The termination reason rides beside the usage block, not inside it

`termination_reason` is an optional field on `InvocationResult`,
`CompletedPayload`, `FailedPayload`, and the sync error body, carried through
`engine.CompletionRequest.TerminationReason` onto `engine.Attempt`, and
persisted in a column named `termination_reason` — deliberately without the
`usage_` prefix its four siblings carry.

The alternative — a fifth field inside the usage block — was rejected on the
no-fabricated-zeros rule that migration 0012 and ADR 0008 both exist to
enforce. `Usage.InputTokens`/`OutputTokens` are plain `int64`, so a usage
block cannot be constructed without them; an actor that knew only *why* its
turn ended (a cancellation, an output cap that produced no final result
object) would have had to send `input_tokens: 0, output_tokens: 0` to report
it. That fabricates a zero-burn attempt and, worse, trips the
`usage_input_tokens IS NOT NULL` sentinel — recording a reason would have
silently converted an honestly-unreported attempt into a falsely-reported
one. Keeping the reason a sibling makes the two facts independently present:
usage without a reason, a reason without usage, both, or neither.

`termination_reason` is also not a second copy of §13.5's error class. The
class is the control plane's classification of a failure and decides retry
and routing; the reason is the provider's statement about the turn, and on a
failed attempt the two differ most.

### 3. Carriage covers every seam that already carries usage

The four usage fields travel through the existing `Usage.ToEngine()` seam, so
every completion path that carried usage carries them with no further change.
The termination reason needed its own carriage, and got it at every one of
those same seams: the synchronous result, both terminal callback payloads,
and the non-2xx error body (`actors.TerminationReasonOf`, the sibling of
`actors.UsageOf`). A reason reported at a seam that drops it would be the
same silent drop this ADR exists to close.

## Consequences

- Cache economics become measurable per attempt as bridges adopt the fields
  (bridge emission is task t3; rollups, API, and the `cache_ratio` the web
  Statistics view renders are task t2). Until a bridge emits them, the
  columns are NULL, which reads as unreported — never as 0% cached.
- Rollup and analytics consumers must treat each new column as independently
  optional. Summing `usage_cached_input_tokens` over attempts that reported
  none is only honest if the "not reported" count travels with the sum, the
  way `attempts_not_reported` already accompanies the 0012 rollups.
- Token sums are only comparable within one `usage_model`. A cross-model sum
  is a different unit, and any surface presenting one owes the reader that
  caveat.
- `internal/worker`'s `completeTechnicalFailure` now takes an
  `actorTelemetry` value rather than a bare usage pointer, so a future third
  actor-reported fact on the failure path (task t8's persisted capacity class
  and Retry-After are the next candidates) extends a struct instead of adding
  a positional parameter.
- Provider names stay out of the §9.5-scanned trees. The vendor-specific
  evidence that motivated the change lives in this ADR and in the frame, not
  in `internal/actors` or `internal/engine`.
