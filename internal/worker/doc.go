// Package worker claims ready work and dispatches it to whatever the node's
// kind says should execute it.
//
// # The loop
//
// One tick is: claim a bounded batch (PRD §12.4), and for each claimed item
// load the pinned definition, resolve the node's input bindings (§11.2),
// dispatch by kind, and report the result through engine.CompleteAttempt
// (§12.5). Nothing here decides what a run does next — the engine owns
// transitions — and nothing here interprets an actor's prose. A dispatch
// produces a technical status and, when it succeeded, a domain outcome and an
// output payload, and those are what get reported.
//
// # Why the worker resolves bindings and the engine does not
//
// The engine deliberately stops at "here is a node run that is ready"
// (internal/engine's doc comment says so). Resolving /run/input,
// /nodes/<id>/output, and /ledger/projections/<name> into a dispatch payload
// is a read of run state performed *outside* the completion transaction,
// because it happens before there is anything to commit — and because the
// payload is destined for a process the engine must never talk to. Putting
// the resolver here keeps the engine's transaction boundary exactly as tight
// as §12.5 draws it.
//
// # Synchronous and asynchronous dispatch
//
// A synchronous actor answers 200 and the worker completes the attempt while
// still holding the lease, heartbeating it (ExtendLease) for as long as the
// call runs.
//
// An asynchronous actor answers 202, and §12.6 then forbids what would
// otherwise be natural: "workers must not hold leases or goroutines for
// long-running agents." So the worker parks the work item in the `waiting`
// state, records the durable invocation and a deadline timer, and forgets
// about it entirely. No goroutine waits, no lease ticks, and the worker
// process may exit — the completion arrives later through
// internal/actors.HandleCallback, which re-leases the parked item under the
// fencing token this dispatch held.
//
// # Provider neutrality
//
// Nothing in this package names a model vendor or an agent product, and
// nothing branches on one (§9.5). An `agent` node and an `action.http` node
// take the same code path for the same reason: both are an endpoint speaking
// §13, and the difference between them is the contract they satisfy, not the
// transport. internal/actors/neutrality_test.go enforces this by grep.
//
// # Kinds that are not wired yet
//
// `code`, `approval`, and `wait` nodes need a runner boundary (§13.7), a
// human task surface (§9.9), and durable wait semantics (§12.7) that later
// tasks own. Each has a registered seam here — RunnerDispatcher,
// HumanDispatcher, WaitDispatcher — and, when no seam is registered, the
// attempt fails with a diagnostic that names the missing capability rather
// than silently succeeding or silently hanging. A node kind that cannot be
// executed is an honest failure, not an outcome.
package worker
