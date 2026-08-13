# ADR 0011: a workflow declares its economic budget, and being refused by it routes an edge

- Status: accepted
- Date: 2026-08-13
- Task: t11 (economy-discord-graphs plan,
  `docs/plans/2026-08-13-economy-discord-graphs.md`)
- Issues: #48 item 5 (economic contract), #47 (cold-session tax)
- Frame: `docs/specs/2026-08-13-economy-discord-graphs.md`, claim c6 /
  honesty h5, and claim c46 (session accounting) from scope entry s26

## Context

PRD §9.7 lists five workflow-policy bounds and the fifth was never built:
"maximum total transitions; maximum visits per node; maximum wall-clock
duration; maximum concurrent tokens; **optional agent token or cost
budget**."

Until this task the only budget in the system was
`worker.MaxDispatchAttempts = 3` — a hardcoded cap on how many times ONE work
item may be re-dispatched (`internal/worker/budget.go`). It bounds a re-lease
loop. It says nothing about what a run may spend in total, and a workflow
author had no way to say it either.

What that cost is on the record: the 2026-08 live fan-out exhausted the
operator's five-hour session window mid-wave, and discovered it as a provider
error after the sessions had already been billed (issue #47/#48, deviation
d1). Nothing had declared how many sessions the work was allowed to open, so
nothing could refuse the one that broke the bank.

## Decision

### 1. `spec.budget` is a sibling of `spec.limits`, and nothing about it is defaulted

`budget.maxSessions` and `budget.maxUncachedInput`, both optional integers,
both `minimum: 1`.

It is a sibling rather than a member of `limits` because the two fail
differently. A limit bounds the SHAPE of an execution and exceeding one is a
bound the engine raises against the run. A budget bounds what an execution may
SPEND, and exceeding one is a refusal the author routes (§3 below).

`limits` expands defaults so the normalized IR always carries bounds — an
unbounded loop is a bug. `budget` expands NOTHING. A workflow that declares no
budget is unbudgeted, which is a statement about that workflow, and inventing
an economic ceiling for it would refuse work nobody restricted. The IR omits
the block entirely, so "unbudgeted" is part of what the run's digest pins.

**Zero is refused, twice.** The schema's `minimum: 1` and a policy-level
diagnostic (`budget.not_positive`) that says why: `0` reads as "no bound" —
which is the runtime's own convention for the §9.7 limits, where every check
is `> 0` — and equally as "nothing may be spent". A budget that could mean
either is not a contract. `budget: {}` is refused for the same reason
(`budget.empty`). An author who wants no bound omits the key.

### 2. The check lives where `budget.go` already argued it must

On CLAIMED work, holding the fencing tuple ClaimWork handed out, one step
before the actor is invoked — beside the dispatch-count cap and the capacity
breaker, and for the identical reasons that file's header gives at length: a
claim-time skip strands the item silently ("it stays 'ready' forever, its node
run stays running, its run never ends, and nothing anywhere records why"), and
committing an attempt requires the claim.

It guards the kinds `dispatchActor` handles (`agent`, `action.http`) —
the dispatches that open a billable provider session. The runner path stays
uncovered for the same reason the dispatch cap leaves it uncovered.

A budget that cannot be READ resolves neither way: the error returns to the
dispatch loop and the lease recovers the claim. Proceeding would spend money
the author forbade on the strength of a database hiccup; refusing would route
a workflow permanently down its exhaustion branch on the strength of the same
hiccup. Nothing was invoked, so a retry is free.

### 3. A refusal is a ROUTED outcome, not an engine failure

PRD §3.4 is explicit that an expected answer follows a graph edge instead of
wearing technical failure as a costume, and the `changes_required` precedent
is exactly this shape. Running out of a budget the author WROTE DOWN is the
most expected answer there is.

So a refused dispatch completes with the reserved name
`budget_exhausted`, and the workflow's own edge decides what happens next: a
cheaper actor, a human, a summarise-and-stop node. Only a workflow that
declared a budget and no route out of it ends failed — with the bound named
in the attempt's diagnostic, never silently.

Three mechanics make that honest:

- **`budget_exhausted` is not a technical status.** §3.4's list is closed and
  it is not on it. It is not a contract outcome either: no ACTOR can produce
  it, because the whole point is that no actor ran. It sits in the same
  routable-name family as the technical statuses — routable from any node
  whose dispatch the budget guards, declared nowhere — and the compiler
  refuses an edge from it on any other kind, because such an edge could never
  fire.
- **The attempt's technical status is `policy_denied`.** That is the honest
  §3.4 word for "a declared policy denied this dispatch", and it is the
  status the engine never retries — which is what a refusal should be, since
  the budget would refuse the retry for the same reason.
- **`CompletionRequest.RefusalOutcome` is a new field, not a reuse of
  `Outcome`.** `Outcome` is the actor's answer and requires a succeeded
  status; giving it a second meaning on failure paths (where it is currently
  ignored) would have made every existing caller's stray value load-bearing.
  The engine rejects a refusal outcome on a succeeded attempt outright.

The refused attempt is recorded **unattributed** (`actor_id` NULL), on
`parkExhausted`'s precedent: no actor did anything here, and which actor the
dispatch was addressed to belongs in the detail as a fact about the refusal
rather than as a mark against that actor's record.

### 4. `maxSessions` counts COLD STARTS — the c46 rule

A dispatch carrying a prior `continuation_ref` (ADR 0010) resumes a
conversation this run has already paid to open, and charges **nothing**. Only
a dispatch that carries none — a cold start — charges one session.

This is not a discount; it is the only coherent reading. Under the opposite
rule a warm workstream of N node turns on one session would count as N and
always exhaust the budget it was designed to conserve, so session stickiness
(claim c3) and the economic contract (claim c6) would spend the whole time
fighting each other (claim c46, scope entry s26). The acceptance test is
exactly that: three warm turns under `maxSessions: 1` all run, and the run is
charged one session.

The cold/warm decision and the outbound request's `continuation_ref` come from
ONE lookup (`sessionPlan`), so the thing charged for and the thing sent on the
wire cannot disagree. An unattributed dispatch (no resolved actor row id) is
always cold: a handle belongs to an identity, and there is no identity here
whose conversation it would be.

**Why a new table (`run_sessions`, migration 0023) and not a column on
`attempts`.** The cold/warm fact is known by the WORKER at dispatch time,
while an attempts row is written by the ENGINE at completion time — and for an
asynchronous invocation, in a different process that resolved nothing (the
shape migration 0015 already had to take for `actor_id`). `attempts` also
holds rows for kinds that never touch a provider at all, so any
NULL-means-cold column over that population would count sessions that never
existed. One row per session, keyed by the §13.1 protocol attempt id so a
re-entered dispatch charges once.

Rows are written **only** for runs whose workflow declares `maxSessions`: the
ledger exists to be spent against, and a write on the hot path of every
dispatch in every unbudgeted run would be a write nothing reads. It is written
immediately BEFORE the invocation, which deliberately over-counts a dispatch
that dies in transport — the conservative direction, since a budget that
under-counts spends money the author forbade.

### 5. Absent cache telemetry is charged in full, and the shortfall is named

`maxUncachedInput` sums `usage_input_tokens - COALESCE(usage_cached_input_tokens, 0)`
over every attempt of the run that reported usage (including failed and
retried ones — retry burn is real spend, `UsageRollup`'s rule).

An attempt that reported input tokens but **no cached figure** is charged IN
FULL. ADR 0009 keeps that column NULL precisely so nobody zero-fills it into a
fabricated measurement; the same honesty applied to a DECISION means the
budget must not hand out a discount nobody demonstrated. "No cache telemetry"
is not the fact "0% cached" — it is charged as if it were, as a policy, and
the refusal detail says how many attempts were charged that way so the
assumption is auditable rather than invisible.

In the other direction the measure is a floor, and says so: an attempt that
reported no usage at all (a cancelled session, a crash with no terminal
result — `AttemptsNotReported` is a permanent category) burned tokens no
ceiling here can charge for. The refusal detail carries that count too.

The test is against spend ALREADY accrued, because what the next dispatch will
itself consume is unknowable before it runs. The ceiling therefore stops the
run at its last funded dispatch rather than mid-turn.

## Consequences

- A workflow can now be refused work it declared it could not afford, before
  the money is spent, and route that refusal — which is what "the economic
  contract" in issue #48 item 5 asked for.
- `budget_exhausted` joins the routable-name vocabulary. Anything that later
  enumerates routable names (an authoring UI, a lint, a docs generator) must
  read it from `compiler.OutcomeBudgetExhausted` /
  `engine.OutcomeBudgetExhausted` rather than from the technical-status list.
- A currency-denominated budget is still not implemented; attempts persist
  `usage_cost`/`usage_currency` and nothing bounds a run by them
  (`docs/acceptance.md` says so under §9.7).
- The uncached-input ceiling only bites on backends that report usage at all.
  Until every bridge reports it (task t3 taught them cached input; a backend
  with no cache verb still reports none), a run's measured spend is a floor
  and the budget is correspondingly loose. That is stated in the refusal
  detail rather than hidden.
- `run_sessions` is control-plane bookkeeping, not evidence. Nothing on this
  path writes an `observed`-authority ledger record from it, and no surface
  may present it as proof that a session was or was not reused — that is what
  `usage_thread_id` measures (ADR 0009 §1).
