# Parallel tokens in full: split/join, per-token bounds, and event-driven continuation

- Status: proposed (design pass; implementation in tasks t20 and t21)
- Date: 2026-08-13
- Task: t19 of plan `economy-discord-graphs` (covers claim c49; honesty condition h42)
- Issue: [#43](https://github.com/agentculture/culture-nodes/issues/43) — parallel tokens, split/join, event-driven continuation (graph-model direction c35)
- PRD: §9.8 (parallelism), §9.7 (loop bounds), §12.5 (transaction boundary), §12.6 (async actors), §13.4 (callback events)
- Implemented by: t20 (split/join engine, per-token bounds), t21 (event pickup, mid-execution emission, replay)

Every statement about current behavior below is grounded in the code as it
exists on `main` at the time of writing, with file references. Judgment calls
are marked **Decision Dn** with alternatives considered; things that genuinely
cannot be settled without implementation feedback are marked as open items in
§12 — nothing there is quietly assumed.

## 1. What exists today (the substrate)

The engine is deliberately sequential. `internal/engine/doc.go:46-53` states
it: a run has exactly one active token, no path splits or joins one, and a
workflow declaring `limits.maxParallelTokens` above 1 still runs sequentially
— the limit is read and carried (`internal/engine/workflow.go:73-78`,
`Limits.MaxParallelTokens`), never honored. The groundwork this design builds
on:

- **Tokens are first-class rows with parent links.** `tokens` has
  `parent_token_id` from migration 0002, and `engine.Token` carries
  `ParentTokenID` (`internal/engine/types.go`). `doc.go` says why: "a token
  that only existed implicitly could not later be forked."
- **Transitions are singular by construction.** `transitionPlan`
  (`internal/engine/transition.go:36-49`) carries one `Edge` and one
  `NextNodeID`; `planTransition` is first-match-wins in normalized edge order
  (`transition.go:74-92`). `completion.advance` (`internal/engine/complete.go`,
  §12.5 step 9) creates exactly one token and one node run per completion,
  and `Engine.CreateRun` creates exactly one entry token
  (`internal/engine/engine.go:229-239`).
- **Node-run states have no barrier state.** The eight states
  (`internal/engine/types.go:113-134`): `ready`, `leased`, `running`,
  `waiting_external`, `waiting_human`, `completed`, `failed`, `cancelled`.
  Nothing represents "waiting on sibling tokens".
- **The event surface is half-built for this.** Migration
  `0016_signal_events.sql` was explicitly shaped for issue #43:
  `signal_events` rows are append-only facts owned by no subscriber;
  `signal_subscriptions` carries no uniqueness over `(run_id, event_name)` so
  any number of waiters may subscribe to one name; `fired_event_id` is N:1.
  The documented gap: subscription-then-event resumes,
  event-then-subscription stays parked — delivery
  (`internal/store/postgres/signal.go`, `DeliverSignalEvent`) fires pending
  subscriptions at append time and never scans history.
- **Delivery never completes anything.** `internal/api/signalevents.go:27-50`
  and `signal.go`'s doc comment fix the single-writer discipline: delivery
  appends the fact, marks subscriptions fired, and flips parked work items
  back to `ready` — the worker that re-claims the item completes the node
  run through the fenced §12.5 transaction (`internal/worker/wait.go`).
- **Completions serialize per run.** Both `CreateRun` and the completion
  guard take the `ledger.RunLockKey(runID)` advisory lock
  (`complete.go`, `guard`), and `cancelRun` (`internal/api/runs.go:340-383`)
  takes the same lock for its REAP step. One writer per run at a time is
  already the invariant — this design leans on it hard for barrier counting.
- **Node kinds are a closed enum with the future named.**
  `schemas/workflow/workflow.schema.json:230`: "`parallel`, `join`,
  `foreach`, `subworkflow`, and `event` are named in the PRD as later work
  and are deliberately absent from this enum."
- **Bounds are run-scoped and derived.** `TransitionCount` is
  `count(node_runs) - 1` (`internal/store/postgres/engine_store.go:706-716`);
  `NodeVisits` groups node runs by node key. `checkBounds`
  (`transition.go:116-144`) enforces `maxTransitions`, `maxVisitsPerNode`,
  and `maxDuration` before anything is created.
- **A mid-execution voice already exists for async actors.** The §13.4
  callback event stream (`internal/actors/protocol.go:193-212`) carries
  non-terminal kinds (`accepted`, `heartbeat`, `progress`, `artifact`) on
  POST `/v1/attempts/{id}/events`, authenticated by an attempt-scoped HMAC
  token (`internal/actors/token.go`, TTL 15 minutes at `token.go:42`), with
  event-id idempotency and sequence monotonicity.

## 2. Design overview and vocabulary

Two new node kinds — `parallel` and `join` — make splits and barriers
explicit graph structure. A completion at a `parallel` node selects a **set**
of edges instead of one; each selected edge fans out a token. Sibling tokens
share a **token group** recorded at split time; a `join` node is a barrier
that counts arrivals against the group's recorded cardinality under the run's
advisory lock. Bounds gain one new kind (`parallel_tokens`) and otherwise
stay run-scoped. Event-driven continuation adds **event routes** (edges whose
source is an event name rather than a node outcome), a **`signal` callback
event kind** for non-blocking mid-execution emission, and **replay** that
closes the 0016 event-then-subscription gap with a per-run, per-name cursor.

New vocabulary introduced by this design:

- **token group** — the durable record of one split: which node run fanned
  out, how many tokens it created, and which group encloses it.
- **join arrival** — the durable record of one branch reaching a join:
  which token, from which node, with what outcome and output.
- **event route** — a run-scoped, durable subscription derived from an
  event edge in the pinned IR: "when event E is delivered, create a token at
  node N."

Everything below keeps three existing invariants untouched:

1. **Single-transaction transitions.** Every split fan-out, every join
   arrival, and every event pickup commits in one §12.5-shaped transaction
   under the run's advisory lock, or not at all.
2. **Completion authority stays with fenced workers.** Neither event
   delivery nor barrier satisfaction ever completes a node run; they only
   make work claimable (the `signal.go` precedent).
3. **Bounds are engine-enforced.** No fan-out, arrival, or pickup relies on
   an actor deciding when to stop (PRD §9.7).

## 3. Scripted splits: multi-edge transition plans

### 3.1 The `parallel` node kind

#### D1 — splits are an explicit node kind, not implicit multi-match

A `parallel` node is a router: it does no domain work, declares the
kind-implied outcome `split` (one new entry in
`internal/compiler/vocabulary.go`'s `impliedOutcomes`, alongside
`KindWait: {"completed"}`), and its outgoing edges all originate from
`<node>.split`. When it completes, **every** edge from `split` whose guard
passes fires — set selection, not first-match-wins.

Alternative considered: keep no new kind and change edge semantics so that
any node outcome with multiple matching edges fans out. Rejected: today's
contract is first-match-wins in normalized order
(`transition.go:55-58` — "which edge wins is a property of the definition"),
and existing published workflows rely on it (guarded edge chains ending in an
unguarded fallback are the documented idiom). Silently turning multi-match
into fan-out would change the meaning of every already-pinned definition.
An explicit kind is opt-in, statically visible to the compiler, and matches
the schema comment that already names `parallel`/`join` as the anticipated
shape (`workflow.schema.json:230`).

Guard-filtered splits are the "scripted" selection the issue asks for: an
author writes N edges from `fan.split`, optionally guarded over `input` /
`output` / `outcome` exactly like today's CEL guards, and the eligible subset
at runtime is the fan-out set. Zero eligible edges is the existing
no-eligible-edge diagnostic and fails the run (a split that selects nothing
is a routing failure, same as today). One eligible edge is a degenerate but
legal split of cardinality 1 — it still records a group, so join semantics
stay uniform.

#### D2 — parallel and join nodes dispatch as internal work, not inline engine routing

A `parallel` node run is created, enqueued, claimed, and completed like any
other node: the worker gains a dispatch seam beside `kindWait`
(`internal/worker/dispatch.go`'s kind switch) that completes it immediately
with `StatusSucceeded` / outcome `split` and a trivial output. The fan-out
itself then happens inside that completion's §12.5 transaction. The same
holds for a satisfied join (§4): the barrier flips the join node run to
`ready`, and a worker completes it with outcome `joined`.

Alternative considered: route *through* parallel/join nodes inline, inside
the upstream completion's transaction, with no work item round-trip.
Cheaper by one queue hop, but it would create node runs that complete
without any attempt row (breaking the attempt-per-execution invariant that
`recordAttempt` and the attempts unique constraint encode), and it would
put multi-hop routing logic inside a single completion. The dispatch-as-work
shape reuses every existing safety property — fencing, leases,
restart-durability — for free. Revisit as an optimization only if the extra
hop measurably matters (open item O7).

### 3.2 `transitionPlan` grows a target set

```go
// transitionPlan is what the engine should do next. Exactly one of Targets,
// Complete, Bound, or Diagnostic is meaningful.
type transitionPlan struct {
    // Targets are the eligible edges in normalized order. len == 1 for every
    // node kind except parallel, where it is the full eligible set.
    Targets    []transitionTarget
    Complete   bool
    Bound      *BoundExceeded
    Diagnostic string
}

type transitionTarget struct {
    Edge       *Edge
    NextNodeID string
}
```

`planTransition` keeps its exact current behavior for every existing kind
(first match wins, loop breaks on match). For `node.Kind == kindParallel` it
collects instead of breaking. `transitionInput` gains two fields the new
bounds need:

```go
type transitionInput struct {
    // ... existing fields unchanged ...

    // ActiveTokens is how many tokens the run currently has active,
    // read inside the transaction (tokens_run_state_idx serves it).
    ActiveTokens int
}
```

Bounds are checked for the whole set before anything is created (§5): the
transition count is charged `+K`, each target node's visit count `+1`, and
the active-token count `+K-1` (the consumed source token minus the K
created). A bound exceeded refuses the **entire** split (§5.1).

### 3.3 Fan-out inside one transaction

`completion.advance` generalizes: for each target, insert one token and one
node run and dispatch it — the same three writes it does today, looped —
plus one `token_groups` row when the source is a parallel node:

```go
// TokenGroup records one split: the fan-out set of sibling tokens created
// by a single parallel-node completion. Cardinality is fixed at creation —
// the barrier counts against it (D4: discovered, not declared).
type TokenGroup struct {
    ID             string
    NamespaceID    string
    RunID          string
    SplitNodeRunID string    // the parallel node run whose completion fanned out
    ParentGroupID  string    // enclosing group; "" at top level (nested splits)
    Cardinality    int       // how many tokens the eligible edge set produced
    CreatedAt      time.Time
}
```

`engine.Token` gains `GroupID string` (nullable column, §7). Propagation
rules:

- The entry token has no group.
- An ordinary transition copies the consumed token's `GroupID` to the next
  token (a branch stays in its group).
- A split stamps each fanned token with the new group's ID; the new group's
  `ParentGroupID` is the consumed token's `GroupID` (nesting).
- The post-join token (§4.4) takes the fired group's `ParentGroupID` —
  the join closes the group and re-enters the enclosing one.

Parent links continue to work exactly as today: each fanned token's
`ParentTokenID` is the parallel node run's token, so token ancestry remains
a tree and the run detail surface can render the fork without new queries.

New audit events emitted in the fan-out transaction (§12.5 steps 7/10, via
`completion.emit`): one `TypeTokenSplit` carrying the group id, cardinality,
and the eligible edge list, plus the existing per-token
`TypeTokenTransitioned`/`TypeNodeRunReady` per branch.

Restart durability needs no new machinery: the fan-out commits atomically,
each branch is an ordinary `ready` work item, and a process crash after
commit leaves K claimable items exactly as a crash after a sequential
transition leaves one. (Test T10 pins this.)

## 4. Join barriers

### 4.1 The `join` node kind and the `waiting_join` state

#### D3 — the barrier is a node-run state, created by the first arrival

A `join` node declares its policy in its definition (§8):

```yaml
gather:
  kind: join
  ownerRef: team:platform
  join:
    policy: all        # all | any | quorum
    quorum: 2          # required iff policy == quorum
```

When a branch's completion routes an edge into a join node, the transition
takes an **arrival** path instead of the standard `advance`:

- **First arrival** (no open barrier for this group at this node): create
  one token at the join node (stamped with the arriving token's `GroupID`),
  create the join node run in the new state `NodeRunWaitingJoin`
  (`waiting_join`), record a `join_arrivals` row, and enqueue **no** work
  item. `waiting_join` parallels `waiting_human`'s shape exactly
  (`types.go:122-130`): parked, not terminal, nothing to lease — there is
  deliberately no work item because there is no claimable work until the
  barrier satisfies.
- **Subsequent arrival**: consume the arriving branch token (as every
  completion already does), record a `join_arrivals` row against the open
  join node run, emit `TypeJoinArrived`. No second token, no second node
  run — a join instance is one logical node execution fed by K branches.

The open barrier is located by `(run_id, node_key, group_id)`: the join
node run in state `waiting_join` whose token carries the arriving token's
group. No new `node_runs` column is needed — the lookup joins through
`tokens`. Loops that revisit the same join in later iterations get distinct
barriers automatically, because each iteration's split mints a fresh group.

```go
// JoinArrival is one branch reaching a barrier.
type JoinArrival struct {
    ID            string
    NamespaceID   string
    RunID         string
    JoinNodeRunID string
    GroupID       string
    TokenID       string          // the consumed branch token
    FromNodeID    string          // the branch node that completed into the join
    Outcome       string          // the domain outcome that routed here
    Output        json.RawMessage // that outcome's output payload
    ArrivedAt     time.Time
}
```

Alternative considered for the barrier's home: a new **token** state
(`waiting_at_join`) with no node run until satisfaction. Rejected: every
inspection surface (run detail, node-run listing, cancellation REAP,
`NodeVisits`) is keyed on node runs; a barrier that existed only as token
state would be invisible to all of them and would need a parallel set of
queries. A parked node run is the established idiom for "the graph is here,
waiting" (`waiting_human`, `waiting_external`).

#### D4 — the sibling set is discovered at split time, not declared at the join

The barrier counts arrivals against `token_groups.Cardinality` — the number
of tokens the split actually created — not against a count declared on the
join node. Guarded split edges make a declared count wrong by construction:
a 4-edge split whose guards pass 3 must join at 3. Discovery also survives
event-driven splits (§6), where cardinality is not knowable at authoring
time at all.

Alternative considered: the join declares the branch names it expects.
Rejected for the same reason (guards make membership dynamic), and because
it duplicates graph structure the edges already state — drift between the
two would be a new class of authoring bug the compiler would have to chase.

### 4.2 Counting and firing — why this is race-free

The barrier check runs inside each arrival's completion transaction, and
every completion for a run serializes on the `ledger.RunLockKey(runID)`
advisory lock (`complete.go` `guard`, taken before the fenced update). Two
branches completing "simultaneously" commit their arrivals strictly one
after the other; the arrival that brings the count to the threshold sees all
prior arrivals' committed rows. Counting is a plain
`SELECT count(*) FROM join_arrivals WHERE join_node_run_id = $1` under the
lock — no additional locking, no counter column to keep consistent.
(Test T13 pins the race.)

Satisfaction per policy:

- `all` — arrivals == cardinality.
- `any` — arrivals == 1 (the first arrival fires immediately).
- `quorum` — arrivals == `join.quorum` (the compiler can validate only
  `quorum >= 1` statically — cardinality is dynamic, so a quorum larger
  than the actual fan-out makes the barrier unsatisfiable; see §4.3
  failure semantics for how that resolves, and O2).

When the threshold is reached, the same transaction flips the join node run
`waiting_join → ready` and enqueues its work item. A worker claims it; the
join dispatch seam (D2) reads the arrivals and completes the node run with
`StatusSucceeded` / kind-implied outcome `joined` and the aggregated output:

```json
{
  "arrivals": [
    {"from_node": "lint", "token_id": "tok_1", "outcome": "clean", "output": {}, "arrived_at": "..."},
    {"from_node": "test", "token_id": "tok_2", "outcome": "passed", "output": {}, "arrived_at": "..."}
  ],
  "policy": "all",
  "cardinality": 2
}
```

#### D5 — join output is an ordered arrival array, not a keyed object

Keying branch outputs by node id collides when two branches route through
the same terminal node before the join; keying by a synthetic branch label
adds an authoring concept this pass does not need. An array ordered by
arrival, each element carrying `from_node`, `token_id`, `outcome`, and
`output`, is collision-free, CEL-indexable, and preserves arrival order as
information (which `any`/`quorum` consumers legitimately want). Downstream
edges from `join.joined` guard over it like any output.

### 4.3 Failure semantics

#### D6 — v1: any terminal branch failure fails the run, regardless of join policy

Today an unrouted technical failure fails the whole run
(`completion.failOrRetry` → `failRun`). With siblings, v1 keeps that rule:
a branch whose node run fails terminally (retries exhausted, no edge from
the technical status) fails the run — and the failing completion's
transaction now also **reaps** the run's other active state, mirroring
`cancelRun`'s REAP step (`internal/api/runs.go:340-383`) inside the engine
transaction: consume all active tokens, cancel all non-terminal node runs,
cancel all leasable work items, retire pending timers and signal
subscriptions, retire event routes (§6). A run with dangling live branches
after `RunFailed` would be exactly the re-dispatch zombie issue #19 fixed
for cancellation.

Why not let `any`/`quorum` joins tolerate branch failure (the join is still
satisfiable)? Because tolerance requires knowing, at the moment a branch
fails, **which** join its group will reconverge at and under what policy —
and the sibling set is discovered, not declared (D4): the engine cannot in
general know the group's join node before an arrival happens. The honest v1
rule is the conservative one that matches existing semantics. Two escapes
already exist and remain available to authors: route the technical status
itself (`failed` edges are legal today, `complete.go` `failOrRetry`), or
have the branch produce a domain outcome like `unavailable` and let the
join's consumers decide. Policy-aware failure tolerance — including partial
join resolution ("resolve with what arrived") — is deliberately deferred
(open item O2) until t20 has the barrier working and real workflows show
what they need. The likely shape, recorded here so t20 does not have to
rediscover it: a static compiler analysis mapping each parallel node to its
unique post-dominating join, which would let the engine consult the policy
at branch-failure time.

Unsatisfiable barriers (a `quorum` higher than realized cardinality, or an
`all` barrier one of whose branches completed the run early — §4.5): with D6
these cannot linger silently, because every way a branch stops without
arriving either fails the run (failure), cancels it (cancellation), or is
the early-run-completion case §4.5 refuses. Should a gap be found in
implementation, the barrier is still visible state (`waiting_join` node run
with its arrivals inspectable), never a hung transaction.

### 4.4 Cancellation of losing branches (`any` / `quorum`)

When a barrier fires before all branches arrive, the satisfying arrival's
transaction cancels the group's losers — PRD §9.8: "cancellation of losing
branches is explicit and best-effort":

- **Explicit, transactional**: consume the group's other active tokens
  (matching on `tokens.group_id`, including tokens of nested child groups by
  recursive group parentage), cancel their non-terminal node runs, cancel
  their leasable work items (`ready`/`leased`/`waiting` alike — the same
  three-state reap cancellation uses), retire their pending timers and
  signal subscriptions. Emit `TypeBranchCancelled` per branch.
- **Best-effort, post-commit**: propagation to async actors mid-invocation
  cannot run inside the transaction (no HTTP in the engine — the same
  reasoning as `internal/api/cancelpropagate.go`). `CompletionResult` gains
  the list of reaped in-flight invocation refs; the worker that drove the
  completion propagates cancellation best-effort after commit, mirroring
  `cancelpropagate.go`'s PROPAGATE step. A branch actor that completes
  anyway hits the existing fenced/terminal guards (`ErrStaleClaim`,
  `TerminalNodeRunError`) and leaves no trace — already tested behavior.

A late arrival from a branch that was cancelled cannot happen (its node run
is terminal; the completion guard refuses), so `join_arrivals` never grows
after firing.

Run-level cancellation (`cancelRun`) needs one addition: retire open
barriers — `waiting_join` node runs are non-terminal, so its existing
`status NOT IN ('completed','failed','cancelled')` UPDATE already catches
them; only the new `event_routes` table (§6) adds a REAP line.

### 4.5 Run completion with active siblings

#### D7 — an end node with active sibling tokens is refused at publish time

Today a token reaching an `end` node completes the run inside the same
transaction (`complete.go`, end-of-`advance`) — with siblings active that
would strand them (or force implicit cancellation semantics nobody wrote).
Rather than invent runtime semantics for it, the **compiler refuses** a
workflow in which a path from a `parallel` node can reach an `end` node
without passing through a `join` — every branch must reconverge before the
run can end. This is a static reachability walk (forward from each parallel
node's split edges, stopping at join nodes) over an already-normalized edge
list the compiler validates today; cycles are handled by visited-set
marking.

As defense in depth (pinned IRs from a future compiler bug, hand-built
IRs), the runtime keeps a guard: `completeRun` checks the active-token
count under the run lock and fails the run with a diagnostic naming the
stranded tokens instead of completing it — loud, not silent.

Alternative considered: "last token out completes the run" (an end-node
arrival with siblings active consumes its token, the run completes when the
final token ends). Rejected for v1: it makes the run's output depend on
branch timing (which end node ran last), which is exactly the kind of
nondeterminism a pinned definition is supposed to exclude. It can be
revisited as an explicit `join.policy` or end-node annotation if a real
workflow wants race-to-end semantics (O3).

## 5. Per-token bounds

The §9.7 limits with their v1 scoping, each a decision:

| Limit | Scope in v1 | Enforcement point |
| --- | --- | --- |
| `maxParallelTokens` | per run (cap on concurrently active tokens) | split fan-out; event pickup |
| `maxTransitions` | per run (unchanged) | every transition, charged `+K` at splits |
| `maxVisitsPerNode` | per run (unchanged) | every transition, per target |
| `maxDuration` | per run (unchanged) | every transition |

### 5.1 `maxParallelTokens` and split refusal

`checkBounds` gains the fourth check: at a split of eligible cardinality K,
`ActiveTokens - 1 + K > MaxParallelTokens` (the source token is consumed in
the same transaction) returns a new `BoundExceeded{Kind:
BoundParallelTokens}`. The compiler's expansion already guarantees the field
is present; a workflow that never declares it gets the compiler default of 1
(`internal/compiler/defaults.go:17-19`, `DefaultMaxParallelTokens`) —
meaning a workflow must opt into parallelism for a split to be legal at
runtime, which is exactly right.

#### D8 — a split that would exceed the cap is refused whole, as a bound failure

The entire split is refused — never a partial fan-out — and refusal follows
the existing bound path: `failBound`, `TypeRunBounded`, run failed. Partial
fan-out is rejected because *which* branches exist would depend on runtime
token counts, making the executed graph nondeterministic per run. Queueing
the excess (admission control: create tokens but hold K-minus-cap of them
back) is rejected for v1 because it introduces a token scheduler — a
substrate with its own fairness, ordering, and starvation questions — for a
cap whose v1 job is protection, not throughput shaping (O4 records the
queueing idea). Failing the run matches every other §9.7 bound: the limit
is the author's declared envelope, and crossing it is the workflow being
told its own declaration was violated, loudly.

Event-driven pickup at the cap behaves differently — §6.4: an external event
must not fail a healthy run; pickup is refused and recorded, the run
continues.

### 5.2 Why the other three limits stay run-scoped

- **`maxTransitions`** bounds total work/cost of the run; splitting the
  budget per token would multiply the envelope by fan-out. The derived
  count (`node_runs - 1`, `engine_store.go:706-716`) remains correct under
  this design's bookkeeping: every transition creates exactly one node run
  — a K-way split is K transitions creating K node runs — with one
  documented exception: join arrivals after the first create no node run,
  so a K-way join contributes K transitions' worth of routing but only 1 to
  the count. That undercount is acceptable and *intentional*: an arrival
  does no dispatchable work, and the limit exists to bound work. The
  definition becomes "node runs created after entry", which the code
  comment on `TransitionCount` should restate (t20).
- **`maxVisitsPerNode`** stays per run. Honest consequence, stated rather
  than hidden: three parallel branches each legitimately traversing a
  shared node once will count 3 visits, so authors of fan-in-heavy graphs
  must budget `maxVisitsPerNode ≥` expected fan-out. The per-token
  alternative (count visits along a token's ancestry chain) was considered
  and rejected for v1: lineage-scoped counting requires walking
  `parent_token_id` chains (or materializing per-token counters) inside
  every transition, and joins make lineage a DAG — whose visit count merges
  how? — a semantics question with no obviously right answer. Revisit with
  implementation feedback (O5).
- **`maxDuration`** is wall clock; per-token wall clock is the same clock.

## 6. Event-driven continuation (t21)

Three pieces: any-node pickup via event routes, non-blocking mid-execution
emission via a new callback event kind, and replay closing the 0016 gap.

### 6.1 Event routes: any-node pickup, including splits

#### D9 — event edges in the definition, materialized as durable per-run routes

The workflow schema's edge grows an alternative source (§8): instead of
`from: "node.outcome"`, an edge may declare `onEvent: "<name>"`. Its guard,
when present, evaluates over a new CEL variable `event`
(`{name, payload, emitter}`) plus the existing `input`. Multiple event edges
naming the same event are the split form: one delivery creates one token
per matching edge — "any node can pick one up and continue from it,
**including splits**" (issue #43) falls out of the same set semantics as D1
with no extra machinery.

At `CreateRun`, the engine materializes the pinned IR's event edges as rows:

```sql
-- event_routes: run-scoped durable pickup routes derived from the pinned
-- IR's onEvent edges at run creation. status: active -> retired.
CREATE TABLE event_routes (
    id            TEXT PRIMARY KEY,
    namespace_id  TEXT NOT NULL REFERENCES namespaces (id),
    run_id        TEXT NOT NULL REFERENCES runs (id),
    event_name    TEXT NOT NULL,
    target_node   TEXT NOT NULL,
    guard         TEXT,           -- CEL source, compiled at publish, rebuilt at eval
    status        TEXT NOT NULL DEFAULT 'active',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX event_routes_pending_idx
    ON event_routes (namespace_id, event_name) WHERE status = 'active';
```

Why a table instead of evaluating the IR at delivery time: delivery
(`DeliverSignalEvent`) currently matches subscriptions with one indexed SQL
scan; asking it to load and parse every running run's pinned IR per event
would turn an O(matching rows) operation into O(running runs) IR decodes.
The routes table keeps delivery a pure SQL match — the same shape
`signal_subscriptions_pending_idx` already serves — at the cost of one
insert per event edge per run creation. Routes are retired alongside timers
and subscriptions on run terminal states (cancelRun REAP; run
failure/completion in the engine transaction).

Delivery semantics: `DeliverSignalEvent` already takes the affected runs'
advisory locks in sorted order (`matchSubscriptionsUnderRunLocks`,
`signal.go`). It gains a second phase: after firing subscriptions, match
active routes for `(namespace, name)` (respecting the event's optional
`run_id` scope), evaluate guards, and for each match create one token, one
`ready` node run, and one work item at the target node — the entry-token
shape (`engine.go:229-239`), executed under the run's lock. Delivery still
completes nothing (invariant 2, §2): the tokens it creates are new claimable
work, exactly like `CreateRun`'s. Pickup tokens have no group and no parent
(`ParentTokenID` empty — they are new roots; the *event* is their
provenance, carried in the audit event and the node's input).

#### D10 — event routes are multi-fire, bounded by §9.7

Each matching delivery creates fresh tokens; a route does not retire after
one fire. A cron-like emitter can therefore drive a run's node repeatedly —
which is the loops-and-flows-are-one-thing model c35 confirmed — and the
protection against runaway is exactly §9.7: `maxVisitsPerNode`,
`maxTransitions`, and `maxParallelTokens` all apply to pickup-created
tokens (§6.4). Single-fire routes were considered (retire on first match)
and rejected: they re-create the wait-node subscription semantics that
already exist; the new capability is precisely standing pickup.

The node-run/visit accounting treats a pickup like any dispatch: the target
node's `VisitCount` increments, `TransitionCount`'s derived formula counts
the new node run.

Input for the picked-up node: the resuming event is folded into the node
run's dispatch context the way a fired signal wait folds it into output
(`wait.go` `signalWaitCompleted`) — the target node's input bindings can
reference the event payload. The exact binding surface
(`/event/payload` pointer namespace) is t21's to pin down (O6).

### 6.2 Mid-execution emission: the `signal` callback event kind

#### D11 — emission rides the §13.4 callback stream with attempt-token auth

An actor node that wants to emit an event and keep working posts to the
callback route it already has — POST `/v1/attempts/{id}/events` — with a
new non-terminal event kind:

```go
// EventSignal asks the control plane to append a signal event on the
// emitting attempt's behalf and deliver it. Non-terminal: the attempt
// continues; the event carries no committed workflow state (§10.4 —
// actor-reported information, never observed evidence).
EventSignal EventKind = "signal"
```

Payload: `{"name": "...", "payload": {...}, "scope": "run" | "namespace"}`
(default `run`). Callback ingest, on a `signal` event, appends a
`signal_events` row — `emitter` set from verified context, e.g.
`node:<id>/actor:<id>`, never caller-supplied — and performs the same
delivery `POST /v1alpha1/events` performs (fire subscriptions, fire routes),
in one transaction. The existing callback idempotency (event-id dedup,
sequence monotonicity — `protocol.go:237-247`) makes a redelivered emission
a no-op instead of a double-append.

Auth decision, and why not the existing external route: giving bridges the
`NODES_EVENT_TOKEN_SECRET` bearer (the external delivery route's auth,
`signalevents.go` `requireEventAuth`) would hand every actor a
namespace-wide credential that can wake any parked run. The attempt-scoped
HMAC token (`internal/actors/token.go`) is already minted per attempt,
names its attempt inside the signature, and expires in 15 minutes — the
emission inherits per-attempt attribution and blast-radius for free. The
15-minute TTL is a real constraint for long sessions; `token.go:38-41`
already states the policy ("an actor that runs for an hour is expected to
be re-issued a token"), so emission shares whatever re-issue mechanism
async completion uses — no new gap, but t21 must verify sync-dispatched
bridges are minted a callback token at all (O1).

Non-blocking is structural, not aspirational: the callback is a plain HTTP
POST from the bridge while the session keeps running; the attempt's
lease/fencing state is untouched (only terminal kinds commit workflow
state — `EventKind.Terminal()`), and delivery under the run lock is safe
because the emitting attempt's own completion, whenever it comes, is a
separate transaction that serializes behind it. The agent-calls-a-human
example from the issue becomes: the agent node emits
`{"name": "review-requested"}` mid-turn; a human node's route or a parked
wait picks it up; the agent keeps working; the human's answer comes back as
another event the agent's workflow routes.

All-backends note (repo rule): the engine-side surface is one callback kind;
each bridge (claude, codex, colleague, acp) must expose an emit affordance
to its session for the feature to be real per backend. That is adapter
work outside this design's engine scope, but t21's acceptance should name
it rather than let one backend quietly become the only emitter.

### 6.3 Replay and catch-up: closing the 0016 gap

#### D12 — replay is a per-run, per-name cursor over the append-only fact table

The gap (documented in `0016_signal_events.sql:41-48` and
`signal.go:45-52`): a subscription created after its event stays parked.
The fix, at subscription-park time (worker dispatch,
`dispatchSignalWait` / `StartDurableSignalWait`), inside the park
transaction:

Look for the **oldest** matching `signal_events` row satisfying all of:

1. `namespace_id` matches, `name` matches, and the event's `run_id` scope
   admits this run (NULL or equal);
2. `created_at > run.created_at` — a run only catches up on facts from its
   own lifetime, never on history from before it existed;
3. `created_at >` the newest event previously fired to **this run** for
   **this name** — derived by joining this run's `fired`
   `signal_subscriptions` rows to their `fired_event_id` events; no new
   cursor table, the fired rows *are* the cursor.

If found, the subscription is inserted already `fired` with
`fired_event_id` set (one transaction — the same
`StartDurableSignalWait` call, answering "replayed" instead of "parked"),
the work item is **not** parked, and the dispatch completes immediately
through the existing fired-subscription path
(`signalWaitCompleted`, `wait.go`) — outcome `completed`, event folded into
output, §9.7 bounds enforced by the completion as always. A new audit event
`TypeSignalReplayed` distinguishes catch-up from live delivery. If not
found, park exactly as today.

Supporting index (expand-only): `signal_events (namespace_id, name,
created_at)`.

Why the floor and cursor, stated honestly:

- Without the run-creation floor, any run subscribing to a busy name would
  instantly consume months-old facts.
- Without the per-(run, name) cursor, a loop that re-parks on the same name
  each iteration would re-fire on the same old event every time — a hot
  loop terminated only by `maxVisitsPerNode`. The cursor makes catch-up
  *monotonic*: each replay consumes the next unseen fact, which is queue
  semantics per run.
- The asymmetry this creates is real and deliberate, not accidental: **live
  delivery is broadcast** (one event fires every pending subscription,
  N:1), while **replay is per-subscriber catch-up** advancing the run's
  cursor. Two waiters in the same run subscribing late to the same name
  will consume two *different* backlogged events, where subscribing early
  would have had one event fire both. The alternative — replay the latest
  matching fact to every late subscriber without a cursor — restores
  symmetry but reintroduces the hot-loop problem in any cyclic graph and
  makes "how many times will this event fire?" unanswerable. t21 should
  validate the cursor choice against the first real multi-waiter workflow
  and revisit if the asymmetry bites (O6).

Event **routes** (§6.1) deliberately do not replay: a route matches only
events delivered after the run (and the route row) exists — condition 2 by
construction, since routes are created with the run. Replaying standing
multi-fire routes against history would mean a new run re-executes every
historical event, which is never what "start observing" means.

### 6.4 Pickup at the bounds

Event pickup enforces the same limits as a split, with one difference in
refusal semantics:

#### D13 — pickup past a bound is refused and recorded, never a run failure

An external event arriving while a run is at its `maxParallelTokens` cap
(or the target node at `maxVisitsPerNode`, or the run at `maxTransitions`)
must not fail a healthy run — the run did nothing; the world spoke at a
busy moment. Delivery skips creating the token, emits
`TypeEventPickupRefused` (route id, event id, which bound), includes the
refusal in the delivery response's `resumed`-style listing, and moves on.
The event fact remains appended. There is **no** deferred retry of refused
pickups in v1 — retrying would be admission-control scheduling (the same
substrate D8 declined to build); the append-only fact table means a future
catch-up mechanism remains buildable if wanted (O4). An operator watching
the run sees the refusal in the audit stream, which satisfies the
"observable and reversible" bar the spec sets for engine decisions.

## 7. Migration plan (all expand-only per ADR-0002)

Two migrations, one per implementing task. Neither drops, renames, or
tightens anything; an N-1 binary never reads the new tables/columns and
keeps working (`docs/adr/0002-migration-policy.md`).

**0017 (t20) — split/join:**

```sql
-- token_groups: one row per split fan-out (D4: discovered cardinality).
CREATE TABLE token_groups (
    id                 TEXT PRIMARY KEY,
    namespace_id       TEXT NOT NULL REFERENCES namespaces (id),
    run_id             TEXT NOT NULL REFERENCES runs (id),
    split_node_run_id  TEXT NOT NULL REFERENCES node_runs (id),
    parent_group_id    TEXT REFERENCES token_groups (id),
    cardinality        INTEGER NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX token_groups_run_id_idx ON token_groups (run_id);

-- tokens.group_id: nullable — NULL for every token an N-1 binary wrote
-- and for tokens outside any split. FK, no backfill needed.
ALTER TABLE tokens ADD COLUMN group_id TEXT REFERENCES token_groups (id);
CREATE INDEX tokens_group_id_idx ON tokens (group_id);

-- join_arrivals: one row per branch reaching a barrier (§4.1).
CREATE TABLE join_arrivals (
    id                TEXT PRIMARY KEY,
    namespace_id      TEXT NOT NULL REFERENCES namespaces (id),
    run_id            TEXT NOT NULL REFERENCES runs (id),
    join_node_run_id  TEXT NOT NULL REFERENCES node_runs (id),
    group_id          TEXT NOT NULL REFERENCES token_groups (id),
    token_id          TEXT NOT NULL REFERENCES tokens (id),
    from_node         TEXT NOT NULL,
    outcome           TEXT NOT NULL,
    output            JSONB,
    arrived_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (join_node_run_id, token_id)
);
CREATE INDEX join_arrivals_join_node_run_id_idx ON join_arrivals (join_node_run_id);
```

The new node-run status value `waiting_join` needs no migration:
`node_runs.status` is unconstrained TEXT (migration 0002 — no CHECK), so a
new value is purely additive. Per ADR-0002's consequences section, the
*application-level* question is value tolerance: the API/web layers must
render an unknown status string gracefully rather than switch-default into
an error (t20 touches the status vocabulary end to end).

**0018 (t21) — events:**

```sql
-- event_routes: §6.1 (full DDL there).
-- plus the replay scan index:
CREATE INDEX signal_events_replay_idx
    ON signal_events (namespace_id, name, created_at);
```

**Rollout hazard worth naming (not a schema issue):** during an upgrade
window, an N-1 **worker** can claim work for a run pinned to a workflow
using the new node kinds; its `LoadWorkflow` fails on the unknown kind
(engine `WorkflowError`) and would fail the run — a healthy workflow killed
by rollout timing. Recommendation for t20 (O8): the worker treats
unknown-kind load failure as release-and-retry with backoff (the work item
returns to claimable, a new-binary worker picks it up), not as attempt
failure. Alternatively, operators simply do not publish parallel workflows
until a rollout completes — but a code guard is cheap and the failure mode
is silent otherwise.

## 8. Workflow-schema additions

All additive to `schemas/workflow/workflow.schema.json`; existing documents
remain valid, and the compiler's normalized IR gains fields only new
definitions carry.

1. **Node kinds**: enum gains `parallel` and `join` (amending the :230
   comment that names them as deliberately absent). `foreach`,
   `subworkflow`, `event`, `action.container`, `transform.wasm` stay
   absent (§10).
2. **Join config** on `node`:

   ```json
   "join": {
     "type": "object",
     "required": ["policy"],
     "additionalProperties": false,
     "properties": {
       "policy": {"enum": ["all", "any", "quorum"]},
       "quorum": {"type": "integer", "minimum": 1}
     }
   }
   ```

   Compiler rules: `join` block required iff `kind == "join"`; `quorum`
   required iff `policy == "quorum"`. `first_success` is deliberately not
   in the v1 enum — see §10.
3. **Implied outcomes** (`internal/compiler/vocabulary.go`):
   `parallel → ["split"]`, `join → ["joined"]`.
4. **Event edges**: the edge object gains `onEvent` as an alternative to
   `from` (exactly one of the two; `additionalProperties: false` keeps the
   xor checkable with `oneOf`):

   ```json
   {"onEvent": "review-requested", "to": "notify-human", "when": "event.payload.severity == 'high'"}
   ```

   Compiler: event-edge targets count as reachable; `when` compiles in a
   CEL environment adding the `event` variable; an event edge from
   `onEvent` to an `end` node is refused (a run must not complete as a
   side effect of pickup — it would race D7's guarantees).
5. **Compiler structural checks** (t20): a `parallel` node requires ≥ 1
   edge from `split`; a `join` node requires ≥ 1 incoming edge and exactly
   the normal outgoing-edge rules from `joined`; D7's
   no-end-inside-split reachability walk; `parallel`/`join` nodes carry no
   contract block (they produce engine-shaped outputs, not domain
   contracts).

Deviation note (CLAUDE.md ground rules): PRD §9.8 lists `first_success`
among join policies; v1 shipping without it is a recorded deviation with
its reason in §10, not silent drift.

## 9. Engine test matrix for t20/t21

Named so the implementing tasks can adopt them one-to-one. All engine tests
drive the public engine/store surface the way existing completion tests do;
concurrency tests use real Postgres (the advisory-lock serialization is the
thing under test).

**t20 — split/join and bounds:**

| # | Test | Pins |
| --- | --- | --- |
| T1 | 3-edge unguarded split fans out 3 tokens, 3 node runs, 3 work items, 1 `token_groups` row (cardinality 3), all in one transaction; `group_id` stamped; parents point at the split token | §3.3 atomic fan-out |
| T2 | Guarded split: guards pass on 2 of 4 edges → cardinality 2 | D1 set selection |
| T3 | Split with zero eligible edges fails the run with the no-eligible-edge diagnostic | §3.1 |
| T4 | `all` join over 3 branches: first arrival creates `waiting_join` + no work item; third arrival enqueues; join completes with `joined` and an arrival array of 3 in arrival order | §4.1, D5 |
| T5 | `maxParallelTokens = 2`, 3-way split → `BoundExceeded{parallel_tokens}`, run failed, zero tokens created (nothing partial) | D8 |
| T6 | `maxTransitions` charged `+K` at a split; a split crossing it is refused whole | §5.2 |
| T7 | `any` join: first arrival fires; sibling branches' tokens consumed, node runs cancelled, work items (`ready`/`leased`/`waiting`) cancelled, timers + signal subscriptions retired; late completion of a cancelled branch returns the existing stale/terminal refusal and leaves no trace | §4.4 |
| T8 | `quorum: 2` of 3: second arrival fires; third branch reaped as in T7 | §4.2 |
| T9 | `all` join, one branch fails terminally (retries exhausted, no `failed` edge) → run failed, all sibling state reaped in the same transaction | D6 |
| T10 | Restart mid-split: crash after fan-out commit; restarted workers claim both branches and the run completes (durability needs no recovery step) | §3.3 |
| T11 | `cancelRun` mid-split retires every group token, node run, work item — and the `waiting_join` barrier | §4.4 |
| T12 | End node reachable inside a split without a join → compiler refusal; hand-built IR reaching `completeRun` with active siblings → run failed loudly, not completed | D7 |
| T13 | Concurrency: two branch completions racing into an `all` barrier (parallel goroutines, real Postgres) → exactly one satisfying arrival enqueues the join, arrival count exact | §4.2 |
| T14 | Nested split/join: inner group joins first; post-join token carries the outer group; outer join then satisfies | §3.3 propagation |
| T15 | Two branches traverse a shared node; `maxVisitsPerNode` counts both (documents run-scoped semantics as intended behavior) | §5.2 |

**t21 — events, emission, replay:**

| # | Test | Pins |
| --- | --- | --- |
| T16 | Event delivered, then a wait node subscribes → dispatch completes immediately with the event folded into output, subscription row `fired` with `fired_event_id`, `TypeSignalReplayed` emitted (h42's replay half) | D12 |
| T17 | Event appended before `CreateRun` is never replayed to that run | D12 floor |
| T18 | Loop re-parking on the same name consumes backlogged events one per iteration, oldest first, never the same event twice | D12 cursor |
| T19 | Two pending same-name subscriptions, one live delivery fires both (broadcast unchanged by the replay work) | §6.3 |
| T20 | Mid-execution emission: an async attempt posts a `signal` callback event; a parked same-name wait in another run resumes; the emitting attempt is still in flight and later completes normally (h42's emission half) | D11 |
| T21 | Emission auth: expired attempt token refused; a token for attempt A cannot emit as attempt B; redelivered `signal` event (same event id) appends exactly one fact | D11 |
| T22 | Event edge pickup: delivery creates token + `ready` node run + work item at the target node of a running run; node's input can read the event payload | D9 |
| T23 | Two event edges on one name = pickup split: one delivery, two tokens | D9 |
| T24 | Pickup at `maxParallelTokens` cap: refused, `TypeEventPickupRefused` emitted, run continues unharmed, event fact still appended | D13 |
| T25 | Multi-fire route: three deliveries create three node runs; the fourth trips `maxVisitsPerNode` per T24's refusal semantics | D10 |
| T26 | Event-edge guard over `event.payload` filters pickup | D9 |
| T27 | Run cancellation/failure/completion retires event routes; a post-terminal delivery matches nothing | §6.1 |

## 10. Explicitly out of scope

- **`foreach`** — dynamic fan-out over a runtime collection needs per-item
  token payloads, item indexing, and a collection-sized cardinality story;
  this pass's splits are over declared edges only.
- **`subworkflow`** — child-run creation, digest pinning across runs, and
  result mapping are a separate PRD later-work item with their own
  authority questions.
- **`first_success` join policy** — the PRD names it, but this engine
  deliberately has no first-class success/failure typing on domain
  outcomes (§3.4 separates domain outcomes from technical status), so
  "first *success*" has no honest meaning yet; shipping `any` + guards
  covers the practical cases until outcomes grow a success annotation, and
  the omission is recorded as a PRD deviation (§8).
- **Token admission queueing** — refused splits/pickups are terminal
  refusals, not queued work (D8/D13); a scheduler is its own design.
- **Cross-namespace events** — `signal_events` stays namespace-scoped.

## 11. Decision index

| ID | Decision |
| --- | --- |
| D1 | Splits are an explicit `parallel` node kind; ordinary edges stay first-match-wins |
| D2 | `parallel`/`join` dispatch as internal work items; completion authority stays with fenced workers |
| D3 | The barrier is a `waiting_join` node run created by the first arrival |
| D4 | The sibling set is discovered at split time (`token_groups.cardinality`), never declared at the join |
| D5 | Join output is an ordered arrival array |
| D6 | v1: any terminal branch failure fails the run and reaps siblings, regardless of policy |
| D7 | End-inside-split is refused at publish; runtime guard fails loudly as defense in depth |
| D8 | A split exceeding `maxParallelTokens` is refused whole as a bound failure |
| D9 | Event pickup = `onEvent` edges materialized as durable per-run `event_routes` |
| D10 | Event routes are multi-fire, bounded by §9.7 |
| D11 | Mid-execution emission is a `signal` callback event kind under attempt-token auth |
| D12 | Replay is a per-run, per-name cursor: oldest unseen fact since run creation |
| D13 | Pickup past a bound is refused-and-recorded, never a run failure |

## 12. Open items for implementation

- **O1 (t21)** — verify sync-dispatched bridges are minted a callback token
  today; if not, extend dispatch payloads so every attempt can emit. Also
  confirm the token re-issue path long async sessions use, which emission
  inherits (15-minute TTL, `token.go:42`).
- **O2 (t20, likely post-t20)** — policy-aware branch-failure tolerance and
  partial join resolution for `any`/`quorum` require statically mapping
  each parallel node to its post-dominating join. Deferred until the
  conservative D6 rule meets real workflows; also covers refusing a
  `quorum` that can exceed realized cardinality.
- **O3** — race-to-end semantics (an end node annotation or join policy
  that lets the first finishing branch complete the run, cancelling the
  rest) if a real workflow wants it; D7 forbids it structurally today.
- **O4** — admission queueing for refused splits/pickups if refusal (D8) or
  drop (D13) proves too blunt in practice; the append-only event table and
  `TypeEventPickupRefused` records keep both retrofittable.
- **O5** — per-token-lineage `maxVisitsPerNode` if run-scoped counting
  (§5.2) trips legitimate fan-in graphs; requires answering how lineage
  visit counts merge at a join.
- **O6 (t21)** — the exact input-binding surface for pickup nodes
  (`/event/...` pointer namespace) and validation of the replay-cursor
  asymmetry (§6.3) against the first real multi-waiter workflow.
- **O7 (t20)** — measure the extra queue hop D2 costs per split/join; only
  if it matters, design the inline-routing optimization without breaking
  the attempt-per-execution invariant.
- **O8 (t20)** — the N-1-worker rollout guard: unknown node kind at
  dispatch should release-and-retry with backoff rather than fail the run
  (§7); confirm the release path composes with lease reclaim.
