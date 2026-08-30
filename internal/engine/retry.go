package engine

import (
	"context"
	"fmt"
	"time"
)

// The retry ladder for a technical failure, and the one place a re-dispatch
// is ever decided.
//
// It lives in its own file rather than in complete.go for two reasons. The
// arithmetic one is spec finding s46: complete.go sits within 80 lines of the
// repo's 1000-line hard limit and is on this batch's critical path, so growth
// here would have forced a split at a handover gate instead of at a
// deliberate seam. The better reason is that "may this attempt be tried
// again" stopped being a one-line question. It is now two questions with
// different owners — the workflow author's retry budget, and the control
// plane's own knowledge of whether the previous session is still out there —
// and giving them a file makes it hard to answer the second by accident while
// editing the first.

// failOrRetry handles a technical failure: consult the node's retry policy
// first, then look for an edge the workflow declares from this technical
// status, and only then fail the node run and the run.
func (c *completion) failOrRetry(ctx context.Context) error {
	// A refusal the control plane itself produced (task t11: the declared
	// economic budget cannot fund the next dispatch) routes under its own
	// name rather than under the technical status, so an author can send
	// "we ran out of money" somewhere different from "the actor was denied
	// by policy". Nothing was dispatched, so there is nothing to retry —
	// and policy_denied is not retryable anyway, which is why this sits
	// above the retry ladder rather than inside it.
	if name := c.req.RefusalOutcome; name != "" {
		if hasEdgeFrom(c.wf, c.nodeRun.NodeID, name) {
			return c.transition(ctx, name, NodeRunFailed)
		}
		return c.failRun(ctx, NodeRunFailed, fmt.Sprintf(
			"node %q was refused before dispatch (%s) and the workflow declares no edge from %q",
			c.nodeRun.NodeID, c.status, name))
	}

	if c.status.retryable() && c.attemptNumber < c.node.Retry.MaxAttempts {
		refusal := c.retryRefusal()
		if refusal == "" {
			return c.scheduleRetry(ctx)
		}
		if err := c.refuseRetry(ctx, refusal); err != nil {
			return err
		}
		// Fall through: the budget was there and went unspent, so from here
		// on this behaves exactly as an exhausted one does.
	}

	// A workflow may route a technical status (§3.4's "the engine's own
	// statuses also route"), which is how a timeout or a contract rejection
	// reaches a repair node instead of ending the run. The node run is still
	// failed — the attempt did not produce a domain answer — but the token
	// keeps moving.
	//
	// A refused retry lands here too, and that is deliberate: refusing to
	// re-dispatch is not the same as ending the run. An author who declared
	// an edge from `timed_out` gets it followed exactly as they would if the
	// retry budget had run out, so the fence changes which SESSION runs next,
	// never which NODE does.
	if hasEdgeFrom(c.wf, c.nodeRun.NodeID, string(c.status)) {
		return c.transition(ctx, string(c.status), NodeRunFailed)
	}

	detail := fmt.Sprintf("node %q attempt %d ended %s and the workflow declares no edge from that status",
		c.nodeRun.NodeID, c.attemptNumber, c.status)
	if c.result.RetryRefused != "" {
		detail = fmt.Sprintf("%s; the remaining retry budget was refused (%s)", detail, c.result.RetryRefused)
	}
	if c.rejection != nil {
		detail = fmt.Sprintf("%s (%s)", detail, c.rejection.Detail)
	}
	return c.failRun(ctx, NodeRunFailed, detail)
}

// retryRefusal is the workspace fence (spec claims c42/c49, task t10): the
// control plane's own veto over a re-dispatch the node's declared retry
// budget would otherwise permit. It returns the reason to refuse, or "" to
// allow the retry.
//
// It is asked only after retryable() and the attempt budget have already said
// yes, because it answers a different question from either of them. Those two
// ask whether ANOTHER ATTEMPT is worth making. This asks whether the PREVIOUS
// SESSION is out of the way — and it is the only one of the three whose
// answer can be "I do not know".
//
// Today it fences exactly one status. `timed_out` is the status the control
// plane itself can produce against a session that is still running: a
// waiting_external deadline expires, the scheduler finds the node's declared
// continuation does not hold, and the completion commits BEFORE the cancel is
// even sent (decision q3/c48, because sending it first would stall every
// timer in the deployment — c40). Re-dispatching there is how spec claim c42
// describes production today: two writing sessions for one node run, which
// with per-writer worktrees (c51) is no longer byte-level corruption but is
// two real bodies of work with nothing saying which is authoritative, and a
// second billable session bought for one node run.
//
// `failed` and `contract_rejected` are not fenced, and that is a statement
// about their producers rather than an oversight: both arrive from a worker
// or an actor that has finished with the invocation and is reporting on it.
// Neither is ever minted by the control plane against a live session.
func (c *completion) retryRefusal() string {
	if c.req.RetryRefusal != "" {
		return c.req.RetryRefusal
	}
	if c.status != StatusTimedOut {
		return ""
	}
	switch c.req.TimeoutOrigin {
	case TimeoutOriginActor:
		// The actor's own terminal report. Its invocation is over because it
		// said so, so there is no session left to fence against.
		return ""
	case TimeoutOriginDeadline:
		return "the deadline expired against a session the control plane has asked to stop but has not observed stop"
	default:
		// Fail closed. The hazard has no detection — nothing downstream ever
		// notices two sessions writing for one node run — so "I cannot tell"
		// has to resolve the same way "unsafe" does. A producer that knows
		// better says so by setting TimeoutOrigin.
		return "the timeout names no origin, so whether the previous session stopped cannot be determined"
	}
}

// refuseRetry records the fence firing. The unspent budget and the reason are
// both on the event, because "maxAttempts 3 and only one attempt exists" is
// otherwise indistinguishable from a misconfigured workflow, and an operator
// asking why the second session never happened deserves to find the answer in
// the run's own event stream rather than in this file.
//
// It changes no state: the node run is failed or routed by the caller exactly
// as an exhausted budget would be. Refusal is a decision NOT to act.
func (c *completion) refuseRetry(ctx context.Context, reason string) error {
	c.result.RetryRefused = reason
	return c.emit(ctx, TypeAttemptRetryRefused, map[string]any{
		"run_id":           c.run.ID,
		"node_run_id":      c.nodeRun.ID,
		"node_id":          c.nodeRun.NodeID,
		"attempt_id":       c.attemptID,
		"attempt_number":   c.attemptNumber,
		"max_attempts":     c.node.Retry.MaxAttempts,
		"tech_status":      string(c.status),
		"timeout_origin":   string(c.req.TimeoutOrigin),
		"refusal_reason":   reason,
		"attempts_unspent": c.node.Retry.MaxAttempts - c.attemptNumber,
	})
}

func (c *completion) scheduleRetry(ctx context.Context) error {
	delay := c.engine.backoff(c.node.Retry, c.attemptNumber+1)
	availableAt := c.now.Add(delay)

	workID, err := c.tx.EnqueueWork(ctx, c.nodeRun.ID, availableAt)
	if err != nil {
		return err
	}
	// The node run goes back to ready: the same logical node execution is
	// being attempted again, so it keeps its identity and its attempt history
	// rather than being replaced by a new node run.
	if err := c.tx.UpdateNodeRun(ctx, c.nodeRun.ID, NodeRunReady, ""); err != nil {
		return err
	}

	if err := c.emit(ctx, TypeAttemptRetryScheduled, map[string]any{
		"run_id":         c.run.ID,
		"node_run_id":    c.nodeRun.ID,
		"node_id":        c.nodeRun.NodeID,
		"attempt_id":     c.attemptID,
		"attempt_number": c.attemptNumber,
		"next_attempt":   c.attemptNumber + 1,
		"max_attempts":   c.node.Retry.MaxAttempts,
		"backoff":        c.node.Retry.Backoff,
		"tech_status":    string(c.status),
		"work_id":        workID,
		"available_at":   availableAt.Format(time.RFC3339Nano),
	}); err != nil {
		return err
	}

	c.result.Retried = true
	c.result.RetryAvailableAt = availableAt
	c.result.NodeRunState = NodeRunReady
	c.result.RunState = c.run.State
	return c.finish(ctx)
}
