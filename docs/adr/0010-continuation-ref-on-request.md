# ADR 0010: §13.1's invocation request carries a continuation ref, and §13.4's completed event returns one

- Status: accepted
- Date: 2026-08-13
- Task: t4 (economy-discord-graphs plan,
  `docs/plans/2026-08-13-economy-discord-graphs.md`)
- Issues: #47 (cold-session tax), #48 (economic contract)
- Frame: `docs/specs/2026-08-13-economy-discord-graphs.md`, claim c3 and
  honesty h2 (scope entry s3)

## Context

PRD §8 states the principle plainly: "Agent session, memory, workspace, and
continuation references are passed explicitly. There is no invisible shared
conversation" (§8, and §9.6's "a workflow passes `memoryRef`,
`workspaceRef`, or `continuationRef` explicitly").

The wire had only half of that sentence. §13.2's synchronous result body
declares `continuation_ref` — the handle an actor offers for continuing the
conversation it just had — and nothing anywhere consumed it:

- `internal/worker/dispatch.go`'s `completeFromResult` read every other field
  of the result and silently dropped this one (scope entry s3);
- `attempts` had no column for it, so even a captured ref died with the
  worker process that saw it;
- §13.1's request had no field to pass one back, so the engine had no way to
  say "continue that conversation" even if it had remembered which one;
- §13.4's `completed` event had no such field at all, so an actor that
  finished late could not report a ref even in principle — and asynchronous
  completion is the path long sessions take.

The consequence is the cold-session tax #47 measures: every node turn starts
a fresh provider session and re-pays for context the provider already had
cached.

## Decision

### 1. §13.1's request gains `continuation_ref`

`InvocationRequest.ContinuationRef` (`continuation_ref`, optional) carries
the ref a prior attempt returned. This is an **additive amendment to §13.1**
of the kind ADR 0008 and ADR 0009 made to §13.2: a new optional key, no
existing field changes meaning, an actor that ignores it keeps working, and
an older control plane that never sends it is merely a control plane that
never resumes.

It carries **the same name as §13.2's result field on purpose.** §8 names one
vocabulary word for this fact and there is one fact — the provider-side
conversation a turn belongs to. Direction is given by the message, not by the
field name: on a request it is the ref to continue *from*, on a result it is
the ref the actor offers to continue *with*. A second name (`resume_ref`,
`prior_continuation_ref`) would have invented a second vocabulary word for
one PRD noun, and the first bridge author to see both would have had to guess
whether they meant the same thing.

**Absent stays absent.** The field is a pointer with `omitempty`: a dispatch
with no prior ref omits the key entirely rather than sending `null` or `""`.
A bridge's "was I given one" check is therefore key presence, and no bridge
can mistake an empty string for a session handle.

### 2. §13.4's `completed` payload gains `continuation_ref`

`CompletedPayload` mirrors §13.2's result body — that is the field's whole
reason for existing ("an actor that finished late produced exactly the same
kind of answer as one that finished inline") — and it was missing exactly the
field the asynchronous path needs most. Adding it here is what makes
stickiness reachable for long sessions rather than only for turns short
enough to answer inline.

`FailedPayload` deliberately does **not** gain one this task. A ref is a
claim that a resumable conversation exists; a bridge reporting a failed turn
is the least reliable position from which to make that claim, and nothing
downstream would consume it yet.

### 3. The ref persists per attempt

`migrations/0018_attempt_continuation_ref.sql` adds a nullable
`attempts.continuation_ref TEXT`, expand-only with no default and no backfill
(`docs/adr/0002-migration-policy.md`). It rides `engine.CompletionRequest`
onto `engine.Attempt` at every completion seam that already carries result
fields — the synchronous result, the untrusted-post-run technical failure
(the invocation still happened and its session still exists), and the
terminal callback payload.

NULL means "no ref reported", never "the session ended". A resumable
conversation nobody told us about is indistinguishable from none, and the
honest reading of the column is the conservative one: dispatch cold.

### 4. The engine fills the request from the same run and the same actor

`dispatchActor` populates the outbound `continuation_ref` from the most
recent prior attempt **within this run, against this actor row**, that
returned one. That scope is narrower than c3's stated goal and is stated
narrowly on purpose:

- **Not `session_key`.** c3's key is actor + repo + workstream, and a
  workstream outlives a single run. Modelling it needs a declared transport
  key that all three bridges exclude from the Bound-inputs block (task t5)
  and a durable per-key serialization (task t6). Until those exist, a
  cross-run lookup would resume a conversation nothing declared it wanted
  resumed.
- **Not `usage.thread_id`.** ADR 0009 already fixed that boundary: the thread
  id is telemetry about where usage accrued, the continuation ref is the
  handle a bridge resumes with, and neither is derived from the other.
- **Not across actors.** A ref is meaningful only to the actor that issued
  it. An unattributed dispatch (no resolved actor row id — a registry without
  the `ActorRowID` capability) therefore looks up nothing and dispatches
  cold, rather than guessing which conversation it belongs to.

The lookup is best-effort: a failed query is reported and the dispatch
proceeds with no ref. A cold session is more expensive than a warm one and is
never wrong; failing a dispatch because an optimization could not be looked
up would be.

## Consequences

- The engine now carries a continuation ref end to end, but nothing resumes
  yet: all three bridges still hardcode `continuation_ref: None` on the way
  out and ignore it on the way in (task t5, scope entry s19). Until then this
  ADR's fields are plumbing with a null payload — and a `null` from a bridge
  that has no resume verb (colleague) stays a permanent, honest null rather
  than a fabricated handle.
- Stickiness itself remains gated on c42's A/B artifact (task t7). This ADR
  makes the ref *carriable*; it does not make resuming a default.
- `attempts.continuation_ref` is a provider-issued opaque handle. It is not
  evidence, nothing derives a ledger record from it, and no surface should
  present it as proof that a session was reused — that fact is
  `usage_thread_id`'s job.
- `internal/worker`'s `actorTelemetry` gains a third field, which is what ADR
  0009's consequence note anticipated when it made that a struct rather than
  a positional parameter.
