# development-loop — our own development loop, as a graph

This example is the dogfooding one. Every other workflow under `examples/`
automates somebody else's work; this one expresses the order **this project's
own work** moves through:

```text
provision-workspace ─▶ work ─▶ handover-gate ─▶ gate-verdict
                        ▲                          │
                        │  gates_passed ───────────┤─▶ merge-review ─▶ cleanup-workspace ─▶ finish
                        │                          │        │
                        ├── changes_required ──────┘        └── changes_required ──▶ work
                        │
                        │  measurement_incomplete ──▶ needs-human
                        │  gate_broken ────────────▶ gate-backoff ─▶ handover-gate
                        │
   work.needs_human (continuation exhausted) ──────▶ needs-human
   work.timed_out   (deadline + workspace fence) ──▶ needs-human
```

The point of committing it is **not** that the loop runs. It is that the order
is now a **compiled artifact** rather than prose a reader has to obey by hand
(spec claim c21). `nodes validate` refuses an unreachable node, an
`onExhausted` outcome nothing routes, a loop with no declared bound, an outcome
no contract declares, and a binding addressing a surface no resolver answers.
The graph survives all five. That is a real property and a narrow one.

## Validate it

Offline, no control plane, no database, no network:

```bash
go run ./cmd/nodes validate examples/development-loop/workflow.yaml
# valid: development-loop 1.0.0 (0 errors, 0 warnings)

scripts/validate-examples.sh     # the same gate CI runs, over every example
```

`examples/development-loop/input.json` is a complete run input for the
contract the workflow declares.

## What is real today

- **The continuation is engine-evaluated.** `work.continue.while` compiles to
  a CEL program at publish time and is evaluated by
  `Node.DecideContinuation` (`internal/engine/workflow.go`). No model decides
  whether to keep going.
- **A fired deadline pauses instead of cancelling, while it may.** `work`
  declares `policy.timeout: 45m` and `bounds.maxWallClock: 4h`, so the
  deadline handler consults the continuation, re-arms the timer, and keeps the
  session warm (`internal/scheduler/scheduler.go`'s
  `deadlineContinuationHolds`). Once the wall clock is spent the same handler
  cancels and emits `dev.culture.nodes.deadline.cancel-requested`.
- **The workspace fence then refuses the leftover retry.** `maxAttempts: 2`
  leaves budget on the table deliberately; the engine declines to spend it
  against a session it has asked to stop but has not observed stop, emitting
  `dev.culture.nodes.attempt.retry-refused` (`internal/engine/retry.go`). A
  refusal is not an ending, so `work.timed_out` has its own edge to the human
  node.

Both of those two events have one precondition worth stating: the deadline
timer is written only when the actor accepts the invocation **asynchronously**
(`internal/store/postgres/async.go`'s `StartAsyncWait`). A bridge answering
`work` synchronously creates no timer, and neither event is reachable for it.

## What does **not** work yet

Stated here as well as in the graph's own comments, because a graph that looks
complete and is not is worse than one that is explicit about its gaps.

| # | Gap | Where it bites |
|---|-----|----------------|
| 1 | **No worktree events exist** — there is no `worktree.provisioned` and no `worktree.reaped` type in `internal/events/envelope.go` or `internal/engine/events.go`. A consumer filtering the run's event stream for either finds nothing; what lands is `attempt.completed` plus a `proposed` claim. | `provision-workspace`, `cleanup-workspace` |
| 2 | **Nothing calls the provisioner and nothing reaps.** `provision()` exists identically in all three bridges (`adapters/*/src/*/workspace.py`) and its only callers are those bridges' own tests. No dispatch path reaches it, no bridge config carries the roots its signature needs, and there is no removal counterpart anywhere. | `provision-workspace`, `cleanup-workspace` |
| 3 | **Neither handoff carrier can carry anything.** `internal/artifacts` has zero production callers (no ingest/fetch endpoint, no bridge upload, `artifact_refs` dropped on the wire); and task t6 has not landed, so `tests/lint/crosshosthandoff_test.go` still pins a handle to `^artifact://` alone and nothing yet knows what a `git_ref` handle is. No bridge host holds a push credential either. | `work.completed` → routed honestly to `work.handoff_unavailable` |
| 4 | **The gate cannot reach the tree it measures.** `operation.workspaceRef` is handed to the runner verbatim (`internal/worker/code.go`), and the headspace bridge treats it as a local filesystem path, holding no artifact store (`internal/runners/headspace/doc.go`). So the node declares no `workspaceRef` at all rather than making a runner `stat` a JSON pointer. | `handover-gate` |
| 5 | **The continuation condition cannot read the gate's numbers.** The t18 design writes the condition as `failed_gate_count > 0`; `DecideContinuation` populates only `node.state` and `budget.remaining_sessions` and leaves `input`/`output`/`outcome`/`event` empty, so an expression reaching into `output` still cannot be written. It no longer stops the loop *silently*, though: since #105 an erroring condition returns `ErrContinuationUndecidable` and the scheduler records a `dev.culture.nodes.continuation.undecidable` event rather than a stop indistinguishable from a legitimate one. The gate still steers the loop through an **edge**. | `work.continue.while` |
| 6 | **Two of four gates measure nothing on most of this repo.** Coverage reaches only `culture_nodes`, and `sonar.sources=culture_nodes` bounds cognitive complexity the same way. A change under `internal/`, `adapters/` or `web/` is tests-only plus file-length, and the other two are `not_applicable/instrument_not_reaching_tree` — never green (issue #88). | `gate-verdict.measurement_incomplete` → a person |
| 7 | **The validator is unwritten.** t18 produced a design, not a program. The operation fetches and digest-verifies a deployment-supplied script; nothing in this repository implements what that script must compute. | `handover-gate` |

Two consequences worth naming rather than leaving to be discovered:

- **Every path that does not reach `cleanup-workspace` leaks a worktree.**
  `provision-blocked`, `handoff-blocked` and `abandoned` all end with the tree
  on disk, and per gap 2 nothing will ever remove it.
- **`cleanup-workspace` fails closed.** Reaping destroys work no handoff
  carried, and nothing in this graph can undo that, so `reap_refused` is a
  first-class domain answer with its own terminal node and a closed
  `retained_reason` enum.

## What the guards check

`tests/lint/developmentloop_test.go` asserts the four structural properties the
compiler cannot: the three new nodes exist with the kinds their design chose;
both handoff carriers are required with constrained refs; the gate-failure edge
reaches the node that declares the continuation, that declaration carries all
three bounds, and exhaustion reaches a **human** node; and `work.timed_out` is
routed. Four of those five checks fail on documents that compile with zero
errors — which is the reason they exist.

## Authoring decisions

- `provision-workspace`, `work` and `cleanup-workspace` are all
  `actor://company/developer`: a worktree is minted by the bridge that owns
  the checkout and can only be removed by that same host (the t16 decision).
  `merge-review` is deliberately a different actor — which increasingly means
  a different machine, which is exactly why the work must reach it as a handle
  rather than as a path (issue #74).
- Provisioning and reaping are `agent` nodes, which spends a provider session
  on deterministic plumbing. That is a known cost, not an oversight: `code`
  reaches a container that is not the writer's host, and `agent` is the only
  kind that reaches the host holding the checkout. The shape this wants is a
  non-model bridge verb, and it does not exist.
- The gate is two nodes because a code node's routable answer is its exit
  status (`internal/worker/code.go` recognises one success and one failure
  name). `handover-gate` measures and encodes its domain fact as an exit code;
  `gate-verdict` turns that into the three answers t18 specifies. This is
  `pr-upkeep`'s sweep/triage pattern, reused rather than reinvented.
