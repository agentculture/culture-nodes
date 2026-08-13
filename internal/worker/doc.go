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
// # Kinds this worker does not execute directly
//
// `code` nodes execute through the runner boundary (§13.7), which has two
// forms: an in-process Runner (Options.CodeRunner — the Lambda and
// headspace-bridge adapters) executed under a heartbeated lease, and the
// asynchronous runner-service path (runnerasync.go) taken when the registry
// resolves the node to a ServiceIdentity — dispatch, park as
// waiting_external, and commit on a later authenticated status sample.
// `wait` nodes execute through the timer-backed TimerWaitDispatcher
// (wait.go), wired in by New whenever Options.Waiter is nil: until.duration
// and until.timestamp park the work item leaselessly on a durable §12.7 wait
// timer the scheduler fires, and the resumed dispatch completes the node
// through the ordinary §12.5 path so loop bounds hold across the park;
// until.signal is an explicit, diagnosed refusal until the event surface
// lands (build plan t10). In every unconfigured case the attempt fails with
// a diagnostic that names the missing capability (seamRemedy) rather than
// silently succeeding or silently hanging. A node kind that cannot be
// executed is an honest failure, not an outcome.
//
// `approval` nodes are not in that category, even though a HumanDispatcher
// seam exists in seams.go too. The human-task surface (§9.9) is not
// unimplemented worker-side machinery waiting on a later task — task t6
// implemented it, entirely inside the engine (internal/engine/humantask.go):
// dispatching an approval node writes a human_tasks row and parks the node
// run leaselessly, in the same transaction that creates it, and never calls
// EnqueueWork. No work item is ever created for an approval node run, so
// this worker's claim loop never sees one to dispatch — not "fails to
// handle it", but structurally has nothing to claim. A human answers through
// the human-tasks decision API, against that row, never through this
// package.
//
// HumanDispatcher survives as a defensive guard, not a real seam: if the
// invariant above were ever violated, the worker's ordinary "no seam
// registered" handling refuses the item with a diagnosed technical failure
// naming the missing "human-task" capability — recorded on the attempt,
// never silently dropped, never treated as an implicit approval — exactly
// like an unimplemented code or wait seam. See seams.go's HumanDispatcher
// doc comment for why the interface is kept rather than removed, and
// internal/worker/approval_invariant_test.go for the proof of both halves:
// no work item is ever enqueued for approval, and one manufactured
// out-of-band is refused rather than processed.
package worker
