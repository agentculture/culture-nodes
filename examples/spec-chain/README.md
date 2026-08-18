# spec-chain — the devague spec chain as a graph, not as one long session

`/scope` → `/think` → `/challenge` is how a vague idea becomes a buildable spec
in this repository. It works. The way it *runs* does not scale, and the
objection is this repo's own (issue #89, umbrella #87):

- **The operator is the transport.** Every `devague` move is typed in one
  session. Nothing about the sequence is durable except its side effects on
  `.devague/`.
- **The operator is the memory.** Which lenses were swept, over which surfaces,
  which claim a finding seeded — all of it lives in one context window.
- **The evidence is self-reported.** Every "I read file X and found Y" is a
  completion claim by an agent. The probes were real; nothing *measured* that
  they happened.
- **It costs one long session.** Scope, think and three challenge passes in one
  context is the most expensive shape there is under #48's session economics.

The chain is already move-driven and deterministic — devague makes no LLM calls
at all (devague#20) — which is exactly what makes it expressible as a graph:

> the **moves** are code nodes, the **judgement** is agent nodes, the **gates**
> are human decisions, and the frame is a **carried artifact**.

## The shape

```text
scope-sweep ─► record-scope ─► think-frame ─► interrogate ─► record-frame
                                                                  │
                              ┌───────────────────────────────────┘
                              ▼
                  [HUMAN GATE] confirm-proposed-set
                              │ approved
                              ▼
                  converge-export ─► converge-verdict ──not_converged──► interrogate
                              │ converged
                              ▼
                       challenge-fan (parallel)
                     ┌────────┬────────┬────────┐
                 adjacent  feedback  lifecycle  security     (four agent lenses)
                     └────────┴────────┴────────┘
                              ▼
                     challenge-join (all)
                              ▼
                       record-findings
                              ▼
                  [HUMAN GATE] adjudicate-findings
                              │ approved
                              ▼
                      reconverge ─► reconverge-verdict ─► spec-exported
```

## Four decisions inside it

**No new node kind.** `internal/compiler/vocabulary.go` closes the enum at nine
and `TestNodeKindEnumStaysClosedAtNine` holds it there. There is no "devague
node" and no "lens node": a deterministic move is what `code` already means,
and a lens sweep is what `agent` already means.

**The moves are `code` nodes, not `agent` nodes.** This is the same argument
[`examples/merge-gate`](../merge-gate/) makes about the gate. An agent may only
produce `proposed` records (PRD §10.4), so an agent reporting *"I ran `devague
converge`"* is a completion claim about a deterministic program — the actor
grading its own homework. A code node runs the program through the runner
boundary, routes on what it exited with, and is the only kind that may declare
`ledger.observe`.

**The frame travels as an artifact** (decision c25, assumption c13). A lens
agent on another host cannot read `.devague/` in somebody's working tree, and a
filesystem path between nodes is issue #74 again. Every move node's `passed`
contract therefore *requires* a non-empty `artifacts` map: a move that exported
nothing cannot report success, and the engine's own output validation is what
enforces it.

**Agent-origin content lands `proposed`; a confirmation is a separate human
act** (claim c11 / honesty h11). Every agent node declares `ledger.propose` and
nothing stronger — the compiler refuses an agent that tries. The two approval
nodes are where a person decides, and `internal/devague` already models exactly
this shape: a devague-`confirmed` claim maps to a `proposed` record **plus** a
separate `review` record naming it, "because confirmation is a human act
regardless of who authored the claim".

## The gates are unbypassable by construction

`converge-export` is reachable from `confirm-proposed-set.approved` and nowhere
else; `reconverge` from `adjudicate-findings.approved` and nowhere else. That
is a property of the **edges**, checked by the compiler and pinned by
`tests/lint/specchain_test.go`, not a habit somebody has to keep.

`expired` is the third implied port on both approval nodes and is deliberately
left unrouted, as PRD §11.1's own listing leaves it: a decision nobody made has
no edge to follow, and a run that reaches it stops with a diagnostic rather
than picking a branch on the approver's behalf.

## The three-answer convergence

A code node gets exactly one success name and one failure name mapped onto its
exit status (`internal/worker/code.go`), so a move with three real answers puts
the third in a `decision` node. The published exit-status contract a move
program must honour:

```text
exit  0   converged     the moves applied and the frame converged and exported
exit 10   not_converged the moves applied, open vagueness remains — loops back
                        to interrogation, bounded by maxVisitsPerNode
exit 11   move_unavailable  a move could not be applied at all — a question for
                        a person, not a defect in the frame
else      move_broken   the move program itself broke
```

Keeping `not_converged` distinct from `move_unavailable` is the same discipline
`measurement_incomplete` encodes one example over: "the frame is still vague"
and "nothing measured the frame" are different facts with different right
answers, and folding them is how a spec nobody converged comes to look
exported.

## WHAT DOES NOT WORK YET

This file compiles. Compiling is not the claim. Each gap was measured at HEAD
while authoring the example, is named again at the node it affects, and is why
"the spec chain runs as a graph" is not yet true of a *run*.

1. **Nothing carries data into a code operation.** A code node's `input`
   bindings *are* resolved — and an unresolvable one refuses the dispatch as
   `contract_rejected` — and are then discarded: `buildCodeOperation`
   (`internal/worker/code.go`) lowers only image, argv, working directory,
   `environmentRefs`, network posture and `workspaceRef`, and
   `DispatchContext.Input` appears nowhere on the code path. The bindings are
   declared anyway; deleting them would hide the gap rather than close it.
2. **The artifact carrier is one-way.** `POST /v1alpha1/attempts/{attemptID}/artifacts`
   is the only artifact route on the HTTP surface — there is no GET, by test
   (`internal/api/artifacts_test.go`). Publishing also needs the per-attempt
   callback token, and `internal/runners.ContextEnvironment` forwards only the
   run/node-run/attempt ids into the executed process, never a credential.
   Claim c13 says the carrier is "possible rather than aspirational"; the
   honest measure is **half** possible — ingest exists, retrieval does not.
3. **The shipped runner reports paths, not `artifact://` handles.** The
   headspace bridge holds no artifacts Store and records exported outputs "as
   local filesystem paths … not as `artifact://` refs"
   (`internal/runners/headspace/doc.go`), keyed by the declared path. That is
   why the `artifacts` schemas require the map to be non-empty but do not
   constrain the ref value: constraining it would reject every completion the
   shipped runner can produce, and accepting a path as a portable handle is
   issue #74.
4. **No route lands a devague frame in the work ledger**, so honesty condition
   h5's second half is declared and unmet. `internal/devague.MapFrameClaims` —
   the authority-honest mapping — has zero callers outside its own package, and
   `POST /v1alpha1/plan-imports` imports a *plan* into tables that are
   "deliberately NOT the append-only work ledger". The moves can leave
   `observed` runner evidence; they cannot yet leave the `derived` per-claim
   records h5 asks for.
5. **No image here carries devague, and no move program is committed.**
   merge-gate can point at `scripts/merge-gate.py`; there is no
   `scripts/spec-chain-move.py`, so the granted origin is a contract nothing in
   this repository satisfies. This is the sharpest gap in the example.
6. **The lens set is static, which the method is not.** A parallel node fans
   one token per eligible `split` edge, so the four lenses are fixed at publish
   time and addressed by the workflow digest. `/challenge` selects lenses
   *risk-scaled*, per frame. Widening the sweep is a graph edit — at least a
   versioned, reviewable one.
7. **An approval node is not served by `adapters/human-inbox`.** The engine
   parks an approval node's token by writing a `human_tasks` row, and a person
   answers through `GET /v1alpha1/pending-decisions` +
   `POST /v1alpha1/human-tasks/{id}/decision`. The human-inbox bridge is a §13
   **actor** bridge serving **agent** nodes on a `kind=human` actor; no adapter
   reads `human_tasks` or `pending-decisions`. "human-inbox approval nodes"
   names a composition that does not exist. Both surfaces put a person in the
   path; this example takes the engine's own, matching merge-gate and
   pr-upkeep.

## What *is* real today

- The compiler accepts every kind, contract, port and edge, offline, with no
  control plane (`scripts/validate-examples.sh`).
- The split/join is structurally checked: a parallel node with no split edge, a
  join nothing reaches, an end reachable from inside a split, and a join
  reachable outside one are all refused at publish time
  (`internal/compiler/parallel.go`). The four lens branches provably reconvene
  before any ending.
- The authority split is compiler-enforced: `ledger.observe` on an agent node
  is an error, so the deterministic legs and the judgement legs cannot quietly
  swap authorities.
- The gates are unbypassable, by edges.

## Deployment configuration

Loading this example into another deployment means supplying these; it never
means editing the graph.

| What | Value | Notes |
| --- | --- | --- |
| Run input | `idea`, `slug`, `repository` | plain identifiers, never host paths |
| Actor | `actor://company/spec-scout` | the scope sweep |
| Actor | `actor://company/spec-author` | frame author; two nodes share it so the second continues a warm session |
| Actor | `actor://company/spec-challenger` | the four lens sweeps |
| Runner | `runner://headspace/devague-moves` | a separate identity so the moves can be placed on the host that has devague |
| Approvers | `group/spec-approvers` | both gates |
| Granted env | `SPEC_CHAIN_MOVE_URL` | where the move program is fetched from |
| Granted env | `SPEC_CHAIN_MOVE_SHA256` | the digest those bytes must have |

The move plan each node applies travels in **argv**, so it is pinned by this
workflow's content digest — which moves a run applies must not be selectable
after seeing the frame, the same argument merge-gate makes for keeping its gate
matrix out of the environment.

## Related

- `docs/specs/2026-08-18-jira-driven-idea-to-shipped-loop.md` — claims c6, c11,
  c13, c25; honesty conditions h5, h11
- issue #89 — the shape sketch this follows
- `internal/devague/doc.go` — the authority-honest devague → ledger mapping
- [`examples/merge-gate`](../merge-gate/) — the gate as a code node, and the
  "why not a tenth kind" argument in full
- [`examples/development-loop`](../development-loop/) — the carrier idiom and
  the same discipline about naming what does not work yet
