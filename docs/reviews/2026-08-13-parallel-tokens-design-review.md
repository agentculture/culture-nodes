# Review: parallel-tokens-full design (t19)

Independent colleague review of docs/design/2026-08-13-parallel-tokens-full.md,
run 2026-08-13 (colleague backend, work item 55801826b6b7, status: ok).
Collected by the operator; findings route to t20/t21 implementation.

## Design Review: docs/design/2026-08-13-parallel-tokens-full.md

### 1. Correctness Risks / Likely Bugs

**R1 — §4.1 "First arrival creates one token at the join node" conflicts with §4.1 "Subsequent arrival: consume the arriving branch token" (line ~268-279).**
The first arrival creates a token at the join node (stamped with the arriving token's GroupID) and a `waiting_join` node run. But the subsequent arrival path says "consume the arriving branch token" — meaning the branch token is consumed by the completion transaction that records the arrival. The first arrival's token at the join node is *newly created* and never consumed until the join fires. This is internally consistent, but the design doesn't explicitly state what happens to the first-arrival's join-node token if the join is never satisfied (e.g., all other branches fail). The token sits `active` forever. The D6 reaping should cover this, but the reaping description (§4.3) says "consume all active tokens" — does that include the join node's token? It should, but it's not explicitly called out.

**R2 — §4.1 barrier lookup by `(run_id, node_key, group_id)` is underspecified (line ~281).**
The design says "the join node run in state `waiting_join` whose token carries the arriving token's group." But the first arrival creates a *new* token at the join node with the arriving token's GroupID. The lookup needs to find the `waiting_join` node run by joining `node_runs → tokens` on `node_runs.token_id = tokens.id` where `tokens.group_id = arriving_token.group_id`. This is doable but the design doesn't spell out the SQL or the exact join path. For t20, this is a concrete implementation gap.

**R3 — §4.4 "consume the group's other active tokens (matching on `tokens.group_id`, including tokens of nested child groups by recursive group parentage)" (line ~419).**
This is the most complex operation in the design. The recursive group parentage traversal is not specified in terms of SQL or algorithm. A naive recursive CTE on `token_groups` would work, but the design doesn't address the performance implications of a deep nesting. More critically: if a nested split's inner join hasn't fired yet, the inner-group tokens are still active. The outer-group cancellation needs to reach through to them. This is correct in principle but the design doesn't specify how to handle the case where the inner join is itself `waiting_join` — does the outer cancellation cancel the inner join's `waiting_join` node run? It should, but it's not explicit.

**R4 — §5.1 `ActiveTokens - 1 + K > MaxParallelTokens` (line ~505).**
The formula is correct: the source token is consumed (-1) and K new tokens are created (+K), so the net change is K-1. But the design says "ActiveTokens is how many tokens the run currently has active, read inside the transaction (tokens_run_state_idx serves it)." The existing `tokens_run_state_idx` is `(run_id, state)` — it filters by state. The design needs a new index or query for `WHERE state = 'active'` under the transaction. This is a minor implementation detail but worth flagging.

**R5 — §6.3 replay cursor: "the newest event previously fired to this run for this name" (line ~730).**
The design says to derive the cursor by "joining this run's `fired` `signal_subscriptions` rows to their `fired_event_id` events." But a run can have multiple subscriptions to the same name (the design explicitly allows this: "signal_subscriptions carries no uniqueness over (run_id, event_name)"). The cursor should be the MAX of all fired events across all subscriptions for that name, not just one. The design says "the newest event" which implies MAX, but it's not explicit about the aggregation.

**R6 — §6.3 replay: `created_at > run.created_at` floor (line ~722).**
This floor is correct but creates a subtle issue: if a signal event is delivered *at* `run.created_at` (same microsecond), it's excluded. This is a race condition that could cause a replay to miss an event that was delivered in the same transaction as the run creation. The design should use `>=` or explicitly document the microsecond exclusion.

### 2. Design, Clarity, and Maintainability Concerns

**D1 — §3.2 `transitionPlan` grows a `Targets []transitionTarget` field (line ~167-174).**
The design proposes changing `transitionPlan` from carrying one `Edge`/`NextNodeID` to carrying a slice of `Targets`. This is a significant structural change. The current code (transition.go:36-49) has `Edge *Edge` and `NextNodeID string` as separate fields. The design's proposed struct has `Targets []transitionTarget` where each target has `Edge` and `NextNodeID`. This is a breaking change to the API surface. The design should note that `transitionPlan` is used in `CompletionResult` (types.go:379-432) which has `NextNodeID string` and `NextNodeRunID string` — these would need to change to support multi-target fan-out.

**D2 — §4.1 "enqueue no work item" for first arrival (line ~271).**
The design deliberately doesn't enqueue a work item for the `waiting_join` state. This is correct (no claimable work until satisfaction), but it means the `waiting_join` node run is invisible to the worker's claim loop. The design says "parked, not terminal, nothing to lease" — but the existing `waiting_external` and `waiting_human` states also have no claimable work items, and they're handled by the scheduler/delivery paths. The `waiting_join` state is woken by the arrival path, not by a scheduler. This is fine, but the design should explicitly note that `waiting_join` is the first state that is woken by a *completion* path rather than a scheduler or delivery path.

**D3 — §4.3 D6 "any terminal branch failure fails the run, regardless of join policy" (line ~374).**
This is a conservative v1 decision, but it has a significant operational impact: a single branch failure in a 10-branch `all` join kills the entire run, even if 9 branches have already arrived. The design acknowledges this (O2) but doesn't quantify the blast radius. For implementers, this means the reaping logic (§4.3) is the most complex code path in the design — it needs to handle `waiting_join` barriers, nested groups, async actor cancellation, etc.

**D4 — §6.1 event routes: "Pickup tokens have no group and no parent (ParentTokenID empty)" (line ~650).**
This is a significant departure from the token tree model. The design says "they are new roots; the event is their provenance." But the existing run detail surface renders token ancestry as a tree. Event-pickup tokens break this tree — they're roots with no parent. The design should note that the run detail surface needs to handle this case, or that event-pickup tokens should carry a synthetic parent (e.g., the event's emitter node run).

**D5 — §7 migration: `join_arrivals` has `UNIQUE (join_node_run_id, token_id)` (line ~830).**
This uniqueness constraint prevents duplicate arrivals for the same token, which is correct. But it also means that if a branch's completion is retried (e.g., due to a transaction failure), the second attempt to record the same arrival would fail. The design should note that the arrival recording must be idempotent — either through the uniqueness constraint (which it has) or through the transaction's all-or-nothing semantics (which it also has). The constraint is the right choice.

**D6 — §8 "Compiler structural checks: a `parallel` node requires ≥ 1 edge from `split`" (line ~870).**
The design says a split with zero eligible edges fails the run (D1). But a parallel node with zero *declared* edges from `split` is a compiler error. The design should clarify: zero declared edges = compiler error; zero eligible edges (all guards fail) = runtime run failure. These are different failure modes with different audit events.

### 3. Concrete, Actionable Suggestions (ranked)

**S1 — [Critical] §4.1: Explicitly specify the SQL/join path for barrier lookup.**
The design says "the lookup joins through `tokens`" but doesn't spell out the exact query. For t20, this is a concrete implementation gap. Add: `SELECT nr.* FROM node_runs nr JOIN tokens t ON nr.token_id = t.id WHERE nr.run_id = $1 AND nr.node_id = $2 AND t.group_id = $3 AND nr.state = 'waiting_join'`.

**S2 — [Critical] §4.4: Explicitly handle nested `waiting_join` barriers in the cancellation path.**
When an outer group is cancelled, the design says to cancel "non-terminal node runs" — but a nested `waiting_join` is non-terminal and should be cancelled. Add explicit language: "cancel all `waiting_join` node runs whose tokens belong to the cancelled group or any descendant group."

**S3 — [High] §3.2: Note the `CompletionResult` API change.**
The current `CompletionResult` has `NextNodeID string` and `NextNodeRunID string`. For multi-target fan-out, these need to become slices or the design needs to explain how the result surface changes. This is a breaking API change that t20 must address.

**S4 — [High] §6.3: Clarify the replay cursor aggregation for multi-subscription same-name case.**
The design says "the newest event previously fired to this run for this name" — make explicit that this is `MAX(fired_event_id.created_at)` across all fired subscriptions for that name in that run.

**S5 — [Medium] §4.1: Document what happens to the join node's token if the join is never satisfied.**
The first arrival creates a token at the join node. If the join is never satisfied (e.g., all other branches fail and the run is reaped), this token must be consumed by the reaping logic. Add explicit language to §4.3: "the reaping step also consumes the join node's token (if any) and cancels the `waiting_join` node run."

**S6 — [Medium] §6.1: Address the token tree breakage for event-pickup tokens.**
Event-pickup tokens have no parent, which breaks the token tree model. Either: (a) give them a synthetic parent (e.g., the event's emitter node run), or (b) document that the run detail surface must handle orphaned roots.

**S7 — [Low] §5.1: Add a note about the `ActiveTokens` query performance.**
The design says "tokens_run_state_idx serves it" but the existing index is `(run_id, state)` which is fine for the query `SELECT count(*) FROM tokens WHERE run_id = $1 AND state = 'active'`. No change needed, but worth confirming the index covers this.

**S8 — [Low] §7: Add a note about the `waiting_join` status value in API/web layers.**
The design says "the application-level question is value tolerance" but doesn't specify what the API/web layers currently do with unknown status values. Add a concrete check: grep for `NodeRunState` string parsing in the API layer and confirm it handles unknown values gracefully.

### Summary

The design is thorough and well-reasoned. The token/group/barrier state machine is internally consistent, the failure semantics (D6) are conservative but correct for v1, and the replay cursor (D12) is sound for loops and multi-waiter runs. The main risks are: (1) the nested group cancellation (§4.4) is the most complex code path and needs more explicit specification; (2) the `transitionPlan` API change (§3.2) is a breaking change that needs to be reflected in `CompletionResult`; and (3) the event-pickup token tree breakage (§6.1) needs a decision on how to handle orphaned roots. The design doc's claims about the existing engine code are accurate — the references to transition.go, complete.go, types.go, wait.go, and runs.go all check out.
