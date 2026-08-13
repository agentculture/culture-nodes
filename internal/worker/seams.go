package worker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// The dispatch seams for node kinds this worker does not execute directly.
//
// `code` and `wait` are genuinely pending: a `code` node needs the
// headspace-cli runner boundary (§13.7) and a `wait` node needs durable wait
// semantics on top of §12.7's timers, and RunnerDispatcher / WaitDispatcher
// are the extension points those later tasks fill in. Each is declared here
// as an interface the worker will use if one is registered, and each is a
// hard, diagnosed failure when none is.
//
// `approval` is different, and HumanDispatcher's own doc comment below says
// why: task t6 gave approval nodes a different mechanism entirely (an
// engine-side park, PRD §9.9), one that never produces a work item for this
// package to dispatch in the first place. HumanDispatcher is not a pending
// extension point — it is a defensive guard against that invariant ever
// breaking. See internal/worker/doc.go's "Kinds this worker does not execute
// directly" section for the full picture, and
// .devague/deliveries/culture-nodes-app-design.json's deviation d1 for how
// this task's scope changed once t6 landed.
//
// Why interfaces now rather than when the implementations land: the branch in
// the dispatcher has to exist either way, and a branch that says "not yet"
// with no seam behind it invites the eventual implementation to be wired in
// somewhere else entirely. The shapes below are also the honest statement of
// what each kind needs from the worker — a resolved input, the node's own
// declaration, and the identity of the attempt — which is worth writing down
// while the reasoning is fresh.
//
// A missing seam fails the attempt with technical status `failed` and a
// diagnostic naming the capability. It deliberately does not succeed with
// some placeholder outcome: a workflow whose approval node auto-approved
// because nobody had implemented approval yet would be the worst possible
// failure mode for a system whose whole premise is that claims must be
// earned. For `approval` specifically this is not a temporary state to
// outgrow — it is the permanent, correct behaviour for a work item that
// should never exist (see internal/worker/approval_invariant_test.go).

// DispatchContext is what every seam receives: the identity of the attempt
// being dispatched and the payload its bindings resolved to.
type DispatchContext struct {
	// RunID, NodeRunID, TokenID, NodeID identify the work.
	RunID     string
	NodeRunID string
	TokenID   string
	NodeID    string
	// AttemptID is the protocol attempt id — the Idempotency-Key any external
	// call must carry so a redispatch is not a second execution (§20.3).
	AttemptID string
	// ActorRowID is the actors-table row id the node's actor reference
	// resolved to, for durable attribution on the attempt row
	// (attempts.actor_id). It is populated only AFTER Registry.Resolve
	// succeeds (dispatchActor), so every completion committed from a path
	// that fired before resolution carries "" — persisted as NULL, never a
	// guessed attribution. Best-effort besides: a registry without the
	// ActorRowID capability leaves it empty on the success paths too.
	ActorRowID string
	// Attempt is the attempt number this claim represents.
	Attempt int
	// Input is the node's resolved input payload (§11.2).
	Input json.RawMessage
	// Deadline is when this attempt must be finished by, zero when the node
	// declares no timeout and the worker has no default.
	Deadline time.Time
}

// SeamResult is what a seam reports back. It is deliberately the same shape
// the actor path produces, because the engine does not care which kind of
// executor answered — it records a technical status and, on success, a domain
// outcome and an output.
type SeamResult struct {
	// TechStatus is how the dispatch went (§3.4). Required.
	TechStatus engine.TechStatus
	// Outcome is the domain outcome, required when TechStatus is succeeded.
	Outcome string
	// Output is the payload for that outcome.
	Output json.RawMessage
	// LedgerDelta is what the executor proposes to write. A runner may
	// propose `observed` evidence here; the engine re-checks the §10.4
	// authority matrix regardless of what these records claim.
	LedgerDelta json.RawMessage
	// Async reports that the seam took ownership and will report the result
	// later through its own path. The worker then parks the work item exactly
	// as it does for a §13.3 acceptance and reports nothing now.
	Async bool
	// AsyncRef is the seam's own handle for an async dispatch, recorded as
	// the invocation id.
	AsyncRef string
	// AsyncDeadline is when the async dispatch must have reported by.
	AsyncDeadline time.Time
}

// RunnerDispatcher executes a `code` node through the §13.7 runner boundary.
//
// The contract that boundary imposes is not this interface's to enforce, but
// it is this interface's to make possible: the operation is a typed document,
// never a command string, and the implementation must enforce policy inside
// dispatch rather than handing an unrestricted shell to another process.
type RunnerDispatcher interface {
	// DispatchCode executes the node's typed operation. `operation` is the
	// IR's operation block verbatim.
	DispatchCode(ctx context.Context, dc DispatchContext, runnerRef string, operation json.RawMessage) (SeamResult, error)
}

// HumanDispatcher is a vestigial seam kept only as a defensive guard, not an
// extension point anyone should implement.
//
// This interface was written for a design the codebase no longer has: an
// `approval` node's human task created from inside a worker dispatch, the
// way a `code` or `wait` node's eventual seam still will be. Task t6 shipped
// something categorically different (PRD §9.9, internal/engine/humantask.go)
// — dispatching an approval node inserts a human_tasks row directly, in the
// same transaction that creates the node run, and never calls EnqueueWork.
// No work item is ever created for an approval node run, in any code path,
// so the worker's claim loop can never observe one to hand to this
// interface. A human answers through the human-tasks decision API against
// that row, not through anything in this package.
//
// HumanDispatcher therefore has no legitimate implementation to register.
// Options.Human is expected to stay nil in every real deployment, and the
// worker's existing "no seam registered" path (dispatchSeam's errNoSeam
// branch, in dispatch.go) is what makes that safe: a `kind: approval` work
// item that reaches the worker despite the invariant above — a bug, a bad
// migration, a future regression — is refused with a diagnosed technical
// failure naming the missing "human-task" capability, exactly like an
// unimplemented `code` or `wait` seam, rather than silently dropped or
// treated as an implicit approval. internal/worker/approval_invariant_test.go
// proves both halves of this: that dispatching an approval node never
// produces a work item, and that one manufactured out-of-band is refused,
// not processed. See seams.go's package doc and doc.go for how this fits
// with RunnerDispatcher and WaitDispatcher, which remain real, pending
// extension points.
//
// The interface and its plumbing in dispatch.go/worker.go are left in place
// rather than deleted: removing them would touch dispatch.go and worker.go's
// Options/dispatch switch, which this task (t8, deviation d1) scopes away
// from a sibling task's concurrent changes to those same files. Kept, the
// seam is inert and honestly documented; a caller who registers one anyway
// is opting out of the engine-side design on purpose, and DispatchApproval
// would need to know that to behave correctly — which is precisely why
// nothing in this codebase does.
type HumanDispatcher interface {
	// DispatchApproval would create a human task for the node. No production
	// path in this codebase ever calls it, because no approval work item
	// ever exists to trigger the call — see the interface doc above. The one
	// place it IS exercised, worker_test.go's TestRegisteredSeamIsUsed, does
	// so against a work item it manufactures for the purpose, to prove the
	// mechanism still functions as a mechanism — not to exercise anything a
	// real deployment does.
	DispatchApproval(ctx context.Context, dc DispatchContext, approverRef string, deadline time.Duration) (SeamResult, error)
}

// WaitDispatcher implements a `wait` node's resume condition (§9.2, §12.7).
//
// `until` is the IR's wait block verbatim: a duration, a timestamp, or a
// named signal. A duration or timestamp is a durable timer; a signal is an
// external event. Both are asynchronous by nature.
type WaitDispatcher interface {
	// DispatchWait arms the node's resume condition.
	DispatchWait(ctx context.Context, dc DispatchContext, until json.RawMessage) (SeamResult, error)
}
