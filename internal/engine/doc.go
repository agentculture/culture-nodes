// Package engine orchestrates workflow graph execution and run state
// transitions: it creates runs, moves tokens along eligible edges, records
// attempts, enforces loop bounds, and commits the PRD §12.5 transaction.
//
// # What it is authoritative about
//
// The engine owns orchestration state — runs, tokens, node runs, attempts —
// and nothing else. It does not invoke actors, does not run code, and never
// interprets an actor's prose. What it accepts is a completion report:
// a technical status, a domain outcome, an output payload, and a proposed
// ledger delta. What it does with that report is validate it against the
// pinned definition's contracts and commit the state change it earns.
//
// # Domain outcome versus technical status
//
// PRD §3.4's distinction is load-bearing throughout. `changes_required` is a
// domain outcome: the verification actor ran fine and the answer is "not
// yet", so the engine follows the `changes_required` edge back to the build
// node, which is a normal transition and not an error anywhere in this
// package. A technical status — failed, timed_out, contract_rejected — is
// about the dispatch, and it consults the node's retry policy, then any edge
// the workflow declares from that status, and only then fails the run.
//
// contract_rejected is where the two most often get confused, so it is worth
// naming: output that violates its outcome's schema is recorded as the
// *technical* status contract_rejected, never routed as if the actor had
// produced a business answer.
//
// # Bounded loops
//
// Loops are first-class and bounded by the engine, not by the actor. Before
// any transition creates the next node run, the run's transition count, the
// target node's visit count, and the run's wall clock are checked against the
// workflow's §9.7 limits; crossing one fails the run with a bounded event. No
// loop in this runtime relies solely on an agent deciding when to stop.
//
// # One transaction
//
// Every committed change — the fenced completion, the attempt row, the
// accepted ledger records, the node run's completion, the next token and node
// run, the enqueued work, the audit events, and their outbox rows — commits
// in one database transaction or not at all. See CompleteAttempt for the
// step-by-step mapping to §12.5's numbered list, and internal/engine.Tx for
// the surface that transaction spans.
//
// # What this slice does not do yet
//
// Sequential execution only. A run has exactly one active token, and no path
// here splits or joins one, so a workflow that declares
// limits.maxParallelTokens above 1 still runs sequentially — the limit is
// read and carried, not yet honored. Tokens are nonetheless first-class rows
// with parent links, because §9.8's split and join model needs them to be and
// a token that only existed implicitly could not later be forked.
//
// The engine does not build actor invocation payloads: it enqueues work
// referencing a node run, and resolving a node's input bindings into a
// dispatch is the actor-protocol task's job. It does not create human tasks
// or timers, so approval and wait nodes are dispatched as ordinary work in
// this slice. Asynchronous actors (§12.6's waiting_external) have a node-run
// state reserved and no transition into it yet.
package engine
