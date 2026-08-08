package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// CompleteAttempt is the PRD §12.5 transaction: one node attempt's report
// becomes committed orchestration state, or nothing happens at all.
//
// The eleven steps §12.5 lists are performed in this order, each marked in
// the code below:
//
//  1. verify the fencing token and current state  — completion.guard
//  2. validate the output contract                — completion.checkOutput
//  3. validate the ledger delta and authority     — prepareDelta
//  4. append accepted ledger records              — completion.appendLedger
//  5. record the attempt result                   — completion.recordAttempt
//  6. complete the node run                       — completion.transition
//  7. append audit events                         — completion.emit (throughout)
//  8. calculate eligible edges                    — planTransition
//  9. create the next token or node runs          — completion.advance
//  10. insert outbox records                       — completion.emit (same call)
//  11. commit                                      — Store.InTx
//
// Two orderings differ from that list, both deliberately:
//
//   - the attempt row (5) is written before the ledger records (4), because
//     ledger_records.attempt_id is a foreign key to attempts(id). The *checks*
//     still run in §12.5's order — a delta is fully validated before anything
//     is written — and since both writes are in one transaction, no reader
//     ever observes one without the other.
//   - audit events (7) are appended alongside the state changes they describe
//     rather than in one batch, and each carries its outbox row (10) in the
//     same call, because an event and its outbox row that could diverge would
//     defeat the point of having an outbox.
//
// External side effects happen outside this transaction and therefore require
// idempotency (§12.5, §20.3): nothing here calls an actor.
func (e *Engine) CompleteAttempt(ctx context.Context, req CompletionRequest) (CompletionResult, error) {
	if err := req.validate(); err != nil {
		return CompletionResult{}, err
	}

	var result CompletionResult
	err := e.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		c := &completion{engine: e, tx: tx, req: req, now: e.now().UTC()}
		if err := c.do(ctx); err != nil {
			return err
		}
		result = c.result
		return nil
	})
	if err != nil {
		return CompletionResult{}, err
	}
	return result, nil
}

func (r CompletionRequest) validate() error {
	switch {
	case r.WorkID == "":
		return errors.New("engine: CompleteAttempt requires a work id")
	case r.WorkerID == "":
		return errors.New("engine: CompleteAttempt requires a worker id")
	case !r.TechStatus.Valid():
		return fmt.Errorf("engine: CompleteAttempt: technical status %q is not one of the statuses PRD §3.4 lists", r.TechStatus)
	case r.TechStatus == StatusSucceeded && r.Outcome == "":
		return errors.New("engine: CompleteAttempt: a succeeded attempt must name the domain outcome it produced")
	}
	return nil
}

// completion carries one §12.5 transaction's state between its steps.
type completion struct {
	engine *Engine
	tx     Tx
	req    CompletionRequest
	now    time.Time

	wf      *Workflow
	node    *Node
	run     Run
	nodeRun NodeRun

	attemptID     string
	attemptNumber int

	// status and outcome are what will be *recorded*, which is not always
	// what was reported: a succeeded attempt whose output or ledger delta was
	// refused is recorded as contract_rejected with no outcome.
	status    TechStatus
	outcome   string
	rejection *Rejection

	result CompletionResult
}

func (c *completion) do(ctx context.Context) error {
	if err := c.guard(ctx); err != nil {
		return err
	}
	if err := c.loadWorkflow(ctx); err != nil {
		return err
	}

	c.status, c.outcome = c.req.TechStatus, ""
	c.attemptID = c.engine.newID()

	// ---- §12.5(2) validate the output contract ----
	if c.status == StatusSucceeded {
		c.outcome = c.req.Outcome
		if rejection := c.checkOutput(); rejection != nil {
			c.reject(rejection)
		}
	}

	// ---- §12.5(3) validate the proposed ledger delta and producer authority ----
	var pending []ledger.Record
	if c.rejection == nil {
		records, rejection := c.engine.prepareDelta(c.wf, c.node, c.run, c.nodeRun, c.attemptID, c.req, c.now)
		if rejection != nil {
			c.reject(rejection)
		} else {
			pending = records
		}
	}

	// ---- §12.5(5) record the attempt result ----
	if err := c.recordAttempt(ctx); err != nil {
		return err
	}

	// ---- §12.5(4) append accepted ledger records ----
	if err := c.appendLedger(ctx, pending); err != nil {
		return err
	}

	if err := c.emitAttemptEvents(ctx); err != nil {
		return err
	}

	// ---- §12.5(6, 8, 9) complete the node run, calculate eligible edges,
	// create the next token and node run ----
	switch {
	case c.status == StatusSucceeded:
		return c.transition(ctx, c.outcome, NodeRunCompleted)
	case c.status == StatusCancelled:
		return c.cancel(ctx)
	default:
		return c.failOrRetry(ctx)
	}
}

// guard is §12.5 step 1: verify the fencing token and current state.
//
// The work item is read first because it — not the caller — is what binds a
// completion to a node run. The advisory lock is taken before the fenced
// update so that everything downstream (the derived transition and visit
// counts, the ledger appends) sees one writer per run; the fenced update is
// then the authority on whether this worker may write at all. A zero-row
// result there returns ErrStaleClaim, the transaction rolls back, and the
// late worker leaves no trace (§12.4, §20.4).
func (c *completion) guard(ctx context.Context) error {
	work, err := c.tx.WorkItem(ctx, c.req.WorkID)
	if err != nil {
		return err
	}
	located, err := c.tx.NodeRun(ctx, work.NodeRunID)
	if err != nil {
		return err
	}
	if err := c.tx.Lock(ctx, ledger.RunLockKey(located.RunID)); err != nil {
		return err
	}
	if err := c.tx.CompleteWork(ctx, c.req.WorkID, c.req.WorkerID, c.req.FencingToken, c.req.Attempt); err != nil {
		return err
	}

	// Re-read under the lock: between the first read and the lock another
	// completion for this run may have committed.
	if c.nodeRun, err = c.tx.NodeRun(ctx, work.NodeRunID); err != nil {
		return err
	}
	if c.run, err = c.tx.Run(ctx, c.nodeRun.RunID); err != nil {
		return err
	}
	if c.nodeRun.State.Terminal() {
		return &TerminalNodeRunError{NodeRunID: c.nodeRun.ID, NodeID: c.nodeRun.NodeID, State: c.nodeRun.State}
	}
	if c.run.State.Terminal() {
		return &TerminalRunError{RunID: c.run.ID, State: c.run.State}
	}

	c.result.RunID = c.run.ID
	c.result.NodeRunID = c.nodeRun.ID
	c.result.NodeID = c.nodeRun.NodeID
	return nil
}

func (c *completion) loadWorkflow(ctx context.Context) error {
	digest, ir, err := c.tx.WorkflowIR(ctx, c.run.WorkflowVersionID)
	if err != nil {
		return err
	}
	c.run.WorkflowDigest = digest
	if c.wf, err = c.engine.Workflow(digest, ir); err != nil {
		return err
	}
	c.node = c.wf.Nodes[c.nodeRun.NodeID]
	if c.node == nil {
		return &WorkflowError{
			Digest: digest,
			Detail: fmt.Sprintf("node run %s names node %q, which the pinned definition does not declare",
				c.nodeRun.ID, c.nodeRun.NodeID),
		}
	}
	return nil
}

// checkOutput is §12.5 step 2. An undeclared outcome and output that violates
// the outcome's schema are both *technical* refusals: contract_rejected is a
// status, never a domain outcome, so neither can be routed as if the actor
// had produced a business answer (§3.4).
func (c *completion) checkOutput() *Rejection {
	if !c.node.declaresOutcome(c.req.Outcome) {
		return &Rejection{
			Kind:    RejectionOutcome,
			Outcome: c.req.Outcome,
			Detail: fmt.Sprintf("node %q does not declare outcome %q; it declares %v",
				c.node.ID, c.req.Outcome, c.node.Outcomes),
		}
	}
	if err := validatePayload(c.node.OutcomeSchemas[c.req.Outcome], c.req.Output); err != nil {
		return &Rejection{
			Kind:    RejectionOutput,
			Outcome: c.req.Outcome,
			Detail:  fmt.Sprintf("output does not satisfy the contract of outcome %q: %v", c.req.Outcome, err),
		}
	}
	return nil
}

func (c *completion) reject(rejection *Rejection) {
	c.rejection = rejection
	c.status = StatusContractRejected
	c.outcome = ""
	c.result.Rejection = rejection
}

// recordAttempt is §12.5 step 5. The attempt number is derived from the rows
// already recorded for this node run rather than taken from the request: two
// workers cannot then disagree about which attempt this is, and the
// attempts(node_run_id, attempt_number) unique constraint makes a double
// completion a constraint violation rather than a silent second row.
func (c *completion) recordAttempt(ctx context.Context) error {
	number, err := c.tx.NextAttemptNumber(ctx, c.nodeRun.ID)
	if err != nil {
		return err
	}
	c.attemptNumber = number

	if err := c.tx.InsertAttempt(ctx, Attempt{
		ID:           c.attemptID,
		NamespaceID:  c.run.NamespaceID,
		NodeRunID:    c.nodeRun.ID,
		Number:       number,
		ActorID:      c.req.ActorID,
		Status:       c.status,
		FencingToken: c.req.FencingToken,
		Result:       jsonOrNull(c.req.Output),
		StartedAt:    c.now,
		CompletedAt:  c.now,
	}); err != nil {
		return err
	}

	c.result.AttemptID = c.attemptID
	c.result.AttemptNumber = number
	c.result.TechStatus = c.status
	c.result.Outcome = c.outcome
	return nil
}

// appendLedger is §12.5 step 4. Every record goes through ledger.Append,
// which re-applies the §10.4 authority matrix and the record schema — this
// step does not trust prepareDelta's pre-flight, it merely guarantees the
// pre-flight rejected anything Append would have refused, so a rejection is
// recorded state instead of a rolled-back transaction.
func (c *completion) appendLedger(ctx context.Context, pending []ledger.Record) error {
	if len(pending) == 0 {
		return nil
	}
	var opts []ledger.AppendOption
	if c.req.RunnerManifest != nil {
		opts = append(opts, ledger.WithRunnerManifest(*c.req.RunnerManifest))
	}

	l := c.tx.Ledger()
	for _, record := range pending {
		appended, err := l.Append(ctx, record, opts...)
		if err != nil {
			return fmt.Errorf("engine: append ledger record for attempt %s: %w", c.attemptID, err)
		}
		c.result.LedgerRecords = append(c.result.LedgerRecords, appended)
	}
	return nil
}

func (c *completion) emitAttemptEvents(ctx context.Context) error {
	if err := c.emit(ctx, TypeAttemptCompleted, map[string]any{
		"run_id":         c.run.ID,
		"node_run_id":    c.nodeRun.ID,
		"node_id":        c.nodeRun.NodeID,
		"attempt_id":     c.attemptID,
		"attempt_number": c.attemptNumber,
		"work_id":        c.req.WorkID,
		"worker_id":      c.req.WorkerID,
		"fencing_token":  c.req.FencingToken,
		"tech_status":    string(c.status),
		"outcome":        c.outcome,
	}); err != nil {
		return err
	}

	if c.rejection != nil {
		if err := c.emit(ctx, TypeContractRejected, map[string]any{
			"run_id":           c.run.ID,
			"node_run_id":      c.nodeRun.ID,
			"node_id":          c.nodeRun.NodeID,
			"attempt_id":       c.attemptID,
			"rejection_kind":   string(c.rejection.Kind),
			"reported_outcome": c.rejection.Outcome,
			"detail":           c.rejection.Detail,
			"recorded_as":      string(StatusContractRejected),
		}); err != nil {
			return err
		}
	}

	for _, record := range c.result.LedgerRecords {
		if err := c.emit(ctx, TypeLedgerAppended, map[string]any{
			"run_id":      c.run.ID,
			"node_run_id": c.nodeRun.ID,
			"attempt_id":  c.attemptID,
			"record_id":   record.ID,
			"record_type": string(record.RecordType),
			"authority":   string(record.Authority),
		}); err != nil {
			return err
		}
	}
	return nil
}

// failOrRetry handles a technical failure: consult the node's retry policy
// first, then look for an edge the workflow declares from this technical
// status, and only then fail the node run and the run.
func (c *completion) failOrRetry(ctx context.Context) error {
	if c.status.retryable() && c.attemptNumber < c.node.Retry.MaxAttempts {
		return c.scheduleRetry(ctx)
	}

	// A workflow may route a technical status (§3.4's "the engine's own
	// statuses also route"), which is how a timeout or a contract rejection
	// reaches a repair node instead of ending the run. The node run is still
	// failed — the attempt did not produce a domain answer — but the token
	// keeps moving.
	if hasEdgeFrom(c.wf, c.nodeRun.NodeID, string(c.status)) {
		return c.transition(ctx, string(c.status), NodeRunFailed)
	}

	detail := fmt.Sprintf("node %q attempt %d ended %s and the workflow declares no edge from that status",
		c.nodeRun.NodeID, c.attemptNumber, c.status)
	if c.rejection != nil {
		detail = fmt.Sprintf("%s (%s)", detail, c.rejection.Detail)
	}
	return c.failRun(ctx, NodeRunFailed, detail)
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

// cancel ends the run: cancellation is an instruction, not a fault, so it is
// not retried and not routed.
func (c *completion) cancel(ctx context.Context) error {
	if err := c.tx.UpdateNodeRun(ctx, c.nodeRun.ID, NodeRunCancelled, ""); err != nil {
		return err
	}
	if err := c.tx.ConsumeToken(ctx, c.nodeRun.TokenID); err != nil {
		return err
	}
	if err := c.tx.UpdateRunState(ctx, c.run.ID, RunCancelled, nil); err != nil {
		return err
	}
	if err := c.emit(ctx, TypeRunCancelled, map[string]any{
		"run_id":      c.run.ID,
		"node_run_id": c.nodeRun.ID,
		"node_id":     c.nodeRun.NodeID,
		"state":       string(RunCancelled),
		"detail":      "the attempt reported technical status cancelled",
	}); err != nil {
		return err
	}
	c.result.NodeRunState = NodeRunCancelled
	c.result.RunState = RunCancelled
	return c.finish(ctx)
}

// transition is §12.5 steps 6, 8, and 9: complete the node run, calculate the
// eligible edge, enforce the §9.7 bounds, and create the next token and node
// run.
func (c *completion) transition(ctx context.Context, outcome string, state NodeRunState) error {
	transitions, err := c.tx.TransitionCount(ctx, c.run.ID)
	if err != nil {
		return err
	}
	visits, err := c.tx.NodeVisits(ctx, c.run.ID)
	if err != nil {
		return err
	}

	plan := planTransition(transitionInput{
		Workflow:    c.wf,
		NodeID:      c.nodeRun.NodeID,
		Outcome:     outcome,
		RunInput:    c.run.Input,
		Output:      c.req.Output,
		Transitions: transitions,
		Visits:      visits,
		Elapsed:     c.now.Sub(c.run.CreatedAt),
	})

	// §12.5(6): the node run is completed regardless of where the token goes
	// next — the work is over either way — and its token is consumed.
	if err := c.tx.UpdateNodeRun(ctx, c.nodeRun.ID, state, outcome); err != nil {
		return err
	}
	if err := c.tx.ConsumeToken(ctx, c.nodeRun.TokenID); err != nil {
		return err
	}
	c.result.NodeRunState = state
	c.result.Outcome = outcome
	if state == NodeRunFailed {
		if err := c.emit(ctx, TypeNodeRunFailed, map[string]any{
			"run_id":      c.run.ID,
			"node_run_id": c.nodeRun.ID,
			"node_id":     c.nodeRun.NodeID,
			"attempt_id":  c.attemptID,
			"tech_status": string(c.status),
			"routed_as":   outcome,
		}); err != nil {
			return err
		}
	}

	switch {
	case plan.Bound != nil:
		return c.failBound(ctx, plan.Bound, transitions)
	case plan.Complete:
		return c.completeRun(ctx, c.nodeRun.NodeID, transitions)
	case plan.Edge == nil:
		return c.failRun(ctx, state, plan.Diagnostic)
	default:
		return c.advance(ctx, plan, transitions, visits)
	}
}

// advance is §12.5 step 9: create the next token position and node run.
func (c *completion) advance(ctx context.Context, plan transitionPlan, transitions int, visits map[string]int) error {
	next := c.wf.Nodes[plan.NextNodeID]
	if next == nil {
		return &WorkflowError{
			Digest: c.wf.Digest,
			Detail: fmt.Sprintf("edge %q targets node %q, which the pinned definition does not declare", plan.Edge.From, plan.NextNodeID),
		}
	}

	token := Token{
		ID:            c.engine.newID(),
		NamespaceID:   c.run.NamespaceID,
		RunID:         c.run.ID,
		NodeID:        plan.NextNodeID,
		State:         TokenActive,
		ParentTokenID: c.nodeRun.TokenID,
		CreatedAt:     c.now,
	}
	if err := c.tx.InsertToken(ctx, token); err != nil {
		return err
	}

	nodeRun := NodeRun{
		ID:          c.engine.newID(),
		NamespaceID: c.run.NamespaceID,
		RunID:       c.run.ID,
		TokenID:     token.ID,
		NodeID:      plan.NextNodeID,
		State:       NodeRunReady,
		VisitCount:  visits[plan.NextNodeID] + 1,
		CreatedAt:   c.now,
		UpdatedAt:   c.now,
	}
	if err := c.tx.InsertNodeRun(ctx, nodeRun); err != nil {
		return err
	}

	c.result.NextNodeID = nodeRun.NodeID
	c.result.NextNodeRunID = nodeRun.ID
	c.result.EdgeFrom = plan.Edge.From
	c.result.Transitions = transitions + 1

	if err := c.emit(ctx, TypeTokenTransitioned, map[string]any{
		"run_id":        c.run.ID,
		"node_run_id":   nodeRun.ID,
		"from_node":     c.nodeRun.NodeID,
		"to_node":       nodeRun.NodeID,
		"outcome":       plan.Edge.FromOutcome,
		"edge":          plan.Edge.From,
		"guard":         plan.Edge.When,
		"from_token_id": c.nodeRun.TokenID,
		"token_id":      token.ID,
		"transition":    transitions + 1,
		"visit":         nodeRun.VisitCount,
	}); err != nil {
		return err
	}

	// An end node produces the workflow result rather than dispatching work,
	// so the token arriving there ends the run inside this same transaction.
	if next.Kind == kindEnd {
		if err := c.tx.UpdateNodeRun(ctx, nodeRun.ID, NodeRunCompleted, ""); err != nil {
			return err
		}
		if err := c.tx.ConsumeToken(ctx, token.ID); err != nil {
			return err
		}
		return c.completeRun(ctx, nodeRun.NodeID, transitions+1)
	}

	workID, err := c.tx.EnqueueWork(ctx, nodeRun.ID, time.Time{})
	if err != nil {
		return err
	}
	if err := c.emit(ctx, TypeNodeRunReady, map[string]any{
		"run_id":      c.run.ID,
		"node_run_id": nodeRun.ID,
		"node_id":     nodeRun.NodeID,
		"token_id":    token.ID,
		"work_id":     workID,
		"visit":       nodeRun.VisitCount,
	}); err != nil {
		return err
	}

	c.result.RunState = RunRunning
	return c.finish(ctx)
}

func (c *completion) completeRun(ctx context.Context, endNodeID string, transitions int) error {
	output, err := c.resolveRunOutput(ctx, endNodeID)
	if err != nil {
		var contractErr *ContractError
		if errors.As(err, &contractErr) {
			return c.failRun(ctx, c.result.NodeRunState, contractErr.Error())
		}
		return err
	}

	if err := c.tx.UpdateRunState(ctx, c.run.ID, RunCompleted, output); err != nil {
		return err
	}
	if err := c.emit(ctx, TypeRunCompleted, map[string]any{
		"run_id":      c.run.ID,
		"node_run_id": c.nodeRun.ID,
		"end_node":    endNodeID,
		"transitions": transitions,
	}); err != nil {
		return err
	}

	c.result.RunState = RunCompleted
	c.result.RunOutput = output
	c.result.Transitions = transitions
	return c.finish(ctx)
}

func (c *completion) failRun(ctx context.Context, nodeRunState NodeRunState, detail string) error {
	if err := c.tx.UpdateNodeRun(ctx, c.nodeRun.ID, nodeRunState, c.outcome); err != nil {
		return err
	}
	if err := c.tx.ConsumeToken(ctx, c.nodeRun.TokenID); err != nil {
		return err
	}
	if err := c.tx.UpdateRunState(ctx, c.run.ID, RunFailed, nil); err != nil {
		return err
	}
	if err := c.emit(ctx, TypeRunFailed, map[string]any{
		"run_id":      c.run.ID,
		"node_run_id": c.nodeRun.ID,
		"node_id":     c.nodeRun.NodeID,
		"attempt_id":  c.attemptID,
		"state":       string(RunFailed),
		"detail":      detail,
	}); err != nil {
		return err
	}

	c.result.NodeRunState = nodeRunState
	c.result.RunState = RunFailed
	c.result.Diagnostic = detail
	return c.finish(ctx)
}

// failBound records a run stopped by one of PRD §9.7's loop bounds. The bound
// is enforced by the engine, not by the actor: this is the guarantee that no
// loop relies solely on an agent deciding when to stop.
func (c *completion) failBound(ctx context.Context, bound *BoundExceeded, transitions int) error {
	if err := c.tx.UpdateRunState(ctx, c.run.ID, RunFailed, nil); err != nil {
		return err
	}
	if err := c.emit(ctx, TypeRunBounded, map[string]any{
		"run_id":       c.run.ID,
		"node_run_id":  c.nodeRun.ID,
		"from_node":    c.nodeRun.NodeID,
		"blocked_node": bound.NodeID,
		"bound":        string(bound.Kind),
		"limit":        bound.Limit,
		"actual":       bound.Actual,
		"transitions":  transitions,
	}); err != nil {
		return err
	}

	c.result.Bound = bound
	c.result.RunState = RunFailed
	c.result.Transitions = transitions
	c.result.Diagnostic = fmt.Sprintf("run stopped at %s: %s reached %s (limit %s)",
		bound.NodeID, bound.Kind, bound.Actual, bound.Limit)
	return c.finish(ctx)
}

// finish records the run's final transition count when a branch has not
// already set it.
func (c *completion) finish(ctx context.Context) error {
	if c.result.Transitions == 0 {
		transitions, err := c.tx.TransitionCount(ctx, c.run.ID)
		if err != nil {
			return err
		}
		c.result.Transitions = transitions
	}
	if c.result.RunState == "" {
		c.result.RunState = c.run.State
	}
	return nil
}

// emit appends one audit event and its outbox row (§12.5 steps 7 and 10).
func (c *completion) emit(ctx context.Context, eventType string, data map[string]any) error {
	if _, err := c.tx.AppendEvent(ctx, c.run.ID, event(eventType, data)); err != nil {
		return err
	}
	c.result.EventTypes = append(c.result.EventTypes, eventType)
	return nil
}

// hasEdgeFrom reports whether the workflow declares any edge out of a node's
// named outcome, guarded or not.
func hasEdgeFrom(wf *Workflow, nodeID, outcome string) bool {
	for i := range wf.Edges {
		if wf.Edges[i].FromNode == nodeID && wf.Edges[i].FromOutcome == outcome {
			return true
		}
	}
	return false
}

// backoff computes the delay before the attempt numbered next. `none` is the
// compiler's default for a single-attempt policy and means immediately.
func (e *Engine) backoff(policy RetryPolicy, next int) time.Duration {
	if next < 2 {
		next = 2
	}
	var delay time.Duration
	switch policy.Backoff {
	case "", "none":
		return 0
	case "fixed":
		delay = e.retryBase
	case "linear":
		delay = e.retryBase * time.Duration(next-1)
	default: // "exponential", and anything the schema admits later
		delay = e.retryBase << (next - 2)
	}
	if e.retryMax > 0 && delay > e.retryMax {
		return e.retryMax
	}
	return delay
}

// resolveRunOutput resolves an end node's output binding and checks the
// result against the workflow's output contract.
func (c *completion) resolveRunOutput(ctx context.Context, endNodeID string) (json.RawMessage, error) {
	end := c.wf.Nodes[endNodeID]
	if end == nil || end.OutputFrom == "" {
		// An end node with no output binding produces no result. Saying so
		// with an explicit null is more honest than inventing one from the
		// last node that happened to run.
		return json.RawMessage("null"), nil
	}

	output, err := resolveBinding(ctx, c.tx, c.run, end.OutputFrom)
	if err != nil {
		return nil, &ContractError{
			What:   "run output",
			Detail: fmt.Sprintf("end node %q binds %s, which did not resolve: %v", endNodeID, end.OutputFrom, err),
		}
	}
	if err := validatePayload(c.wf.OutputSchema, output); err != nil {
		return nil, &ContractError{
			What:   "run output",
			Detail: fmt.Sprintf("end node %q produced a result that violates the workflow output contract: %v", endNodeID, err),
		}
	}
	return output, nil
}
