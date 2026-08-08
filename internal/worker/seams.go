package worker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
)

// The dispatch seams for node kinds this slice does not execute yet.
//
// Three of PRD §9.2's MVP kinds need machinery that belongs to other tasks: a
// `code` node needs the headspace-cli runner boundary (§13.7), an `approval`
// node needs the human-task surface (§9.9), and a `wait` node needs durable
// wait semantics on top of §12.7's timers. Each is declared here as an
// interface the worker will use if one is registered, and each is a hard,
// diagnosed failure when none is.
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
// earned.

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

// HumanDispatcher creates the human task an `approval` node needs (§9.9).
//
// An approval is almost always asynchronous — a human is not going to answer
// inside a lease — so an implementation is expected to return Async with a
// task reference and a deadline.
type HumanDispatcher interface {
	// DispatchApproval creates a human task for the node.
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
