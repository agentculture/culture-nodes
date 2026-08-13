package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// DecideHumanTask is t7's half of the seam humantask.go's package doc
// leaves: the resolution entry point for a human task, keyed on the task
// (or, through it, the node run) rather than a work id — a human task has
// no work_items row, so it cannot resume through CompleteAttempt, which
// resolves a node run by construction from the work item a worker claimed.
//
// One call is the PRD §12.5-shaped transaction for a human decision, or
// nothing happens at all:
//
//  1. load the task and its paused node run, refusing an already-decided
//     task before anything else is read (guard);
//  2. validate the decision's outcome against the task's own
//     allowed_outcomes — what the human was actually shown (checkOutcome);
//  3. record the decision as a human-authority review: append one proposed
//     `decision` record carrying the outcome and response, then confirm it
//     (plus any caller-named record_ids) through ledger.CreateReviewRequest
//     + ledger.CommitReview — the same atomic, ledger-version-guarded
//     transaction PRD §10.8 review commits use, so a stale
//     expected_ledger_version is refused with nothing written
//     (recordDecision);
//  4. flip the task from pending to decided, atomically — a race loses here,
//     not after the run has already moved (markDecided);
//  5. complete the waiting_human node run and route the edge the decision's
//     outcome selects, exactly like completion.transition routes a
//     succeeded attempt's domain outcome (transition, advance, ...).
//
// Every step runs inside one database transaction (Engine.store.InTx); an
// error at any step rolls the whole thing back, so a refused decision (a
// disallowed outcome, a stale ledger version, a task already decided)
// leaves the task exactly as it was.
func (e *Engine) DecideHumanTask(ctx context.Context, req HumanTaskDecisionRequest) (CompletionResult, error) {
	if err := req.validate(); err != nil {
		return CompletionResult{}, err
	}

	var result CompletionResult
	err := e.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		d := &humanTaskDecision{engine: e, tx: tx, req: req, now: e.now().UTC()}
		if err := d.do(ctx); err != nil {
			return err
		}
		result = d.result
		return nil
	})
	if err != nil {
		return CompletionResult{}, err
	}
	return result, nil
}

// HumanTaskDecisionRequest is one human decider's answer to a paused
// approval node (PRD §9.9).
type HumanTaskDecisionRequest struct {
	// HumanTaskID names the task being decided.
	HumanTaskID string
	// Outcome is the domain outcome the decider selected. It must be one of
	// the task's declared allowed_outcomes and is what routes the edge —
	// exactly parallel to CompletionRequest.Outcome for an ordinary attempt.
	Outcome string
	// Response is the decision payload the decider recorded — notes, the
	// decision schema's filled-in fields, whatever the surface presenting
	// this task collected. It is carried into human_tasks.response, into the
	// ledger decision record's data, and is the CEL "output" activation for
	// any guarded edge leaving this node (exactly as CompletionRequest.Output
	// is for a completed attempt).
	Response json.RawMessage
	// DeciderActorID is the human recorded as this decision's origin — the
	// ledger record's origin.actor_id and the review's reviewer_actor_id.
	// Required: a confirmation nobody is accountable for is not a
	// confirmation (PRD §10.8; see ledger.WithReviewer).
	DeciderActorID string
	// ExpectedLedgerVersion is the run's ledger version the decider last
	// read. A decision whose expectation does not match the run's current
	// version is refused, atomically, before anything is written.
	ExpectedLedgerVersion int64
	// RecordIDs optionally names other ledger records the decider is
	// confirming alongside this decision — e.g. records the task's
	// context_refs pointed at. They are folded into the same review commit
	// as the decision's own record, all confirmed together: this decision is
	// the human exercising authority over what it read, not a judgment on
	// whether those records' own content was accepted or rejected. Most
	// approval decisions review no discrete prior record, so this is
	// ordinarily empty.
	RecordIDs []string
}

func (r HumanTaskDecisionRequest) validate() error {
	switch {
	case r.HumanTaskID == "":
		return errors.New("engine: DecideHumanTask requires a human task id")
	case r.Outcome == "":
		return errors.New("engine: DecideHumanTask requires the decision's domain outcome")
	case r.DeciderActorID == "":
		return errors.New("engine: DecideHumanTask requires a decider actor id")
	}
	return nil
}

// humanTaskDecision carries one DecideHumanTask transaction's state between
// its steps, the way *completion does for CompleteAttempt. It duplicates
// completion's transition/advance/completeRun/failRun/failBound/finish
// routing methods rather than sharing them: a human decision has no attempt,
// no rejection, and no retry, and factoring those methods to serve both
// shapes would either leak attempt-only fields into this path or risk the
// well-tested completion path for a share this task does not need. Every
// method here that mirrors one of completion's calls the same package-level
// helpers (planTransition, dispatchNode, event) completion.go itself calls,
// so the two paths cannot silently diverge on how an edge is selected or a
// node is dispatched — only on what is attempt-shaped and what is not.
type humanTaskDecision struct {
	engine *Engine
	tx     Tx
	req    HumanTaskDecisionRequest
	now    time.Time

	wf      *Workflow
	node    *Node
	run     Run
	nodeRun NodeRun
	task    HumanTask

	// outcome is the decision's domain outcome, recorded on the node run
	// exactly like completion's own c.outcome field records a succeeded
	// attempt's outcome — never a technical status, always a real domain
	// answer here, since a human decision has no technical dimension at all.
	outcome string

	result CompletionResult
}

func (d *humanTaskDecision) do(ctx context.Context) error {
	if err := d.guard(ctx); err != nil {
		return err
	}
	if err := d.loadWorkflow(ctx); err != nil {
		return err
	}
	if err := d.checkOutcome(); err != nil {
		return err
	}
	d.outcome = d.req.Outcome

	if err := d.recordDecision(ctx); err != nil {
		return err
	}
	if err := d.markDecided(ctx); err != nil {
		return err
	}
	if err := d.emit(ctx, TypeHumanTaskDecided, map[string]any{
		"run_id":           d.run.ID,
		"node_run_id":      d.nodeRun.ID,
		"node_id":          d.nodeRun.NodeID,
		"human_task_id":    d.task.ID,
		"decider_actor_id": d.req.DeciderActorID,
		"outcome":          d.outcome,
	}); err != nil {
		return err
	}

	return d.transition(ctx, d.outcome, NodeRunCompleted)
}

// guard loads the task and the node run it pauses, refusing an
// already-decided task or a terminal run before anything else happens. It
// reads once, takes the run's advisory lock — the same ledger.RunLockKey a
// completion takes before it touches a run, so a decision and a concurrent
// attempt completion of the same run queue behind each other rather than
// interleaving — and re-reads under the lock, mirroring
// completion.guard's "another writer may have committed between the first
// read and the lock" reasoning.
func (d *humanTaskDecision) guard(ctx context.Context) error {
	task, err := d.tx.GetHumanTask(ctx, d.req.HumanTaskID)
	if err != nil {
		return err
	}
	if task.Status != HumanTaskStatusPending {
		return &HumanTaskAlreadyDecidedError{HumanTaskID: task.ID, Status: task.Status}
	}

	nodeRun, err := d.tx.NodeRun(ctx, task.NodeRunID)
	if err != nil {
		return err
	}
	if err := d.tx.Lock(ctx, ledger.RunLockKey(nodeRun.RunID)); err != nil {
		return err
	}

	if task, err = d.tx.GetHumanTask(ctx, d.req.HumanTaskID); err != nil {
		return err
	}
	if task.Status != HumanTaskStatusPending {
		return &HumanTaskAlreadyDecidedError{HumanTaskID: task.ID, Status: task.Status}
	}
	if nodeRun, err = d.tx.NodeRun(ctx, task.NodeRunID); err != nil {
		return err
	}
	if nodeRun.State.Terminal() {
		return &TerminalNodeRunError{NodeRunID: nodeRun.ID, NodeID: nodeRun.NodeID, State: nodeRun.State}
	}
	if nodeRun.State != NodeRunWaitingHuman {
		return &HumanTaskNotWaitingError{HumanTaskID: task.ID, NodeRunID: nodeRun.ID, State: nodeRun.State}
	}

	run, err := d.tx.Run(ctx, nodeRun.RunID)
	if err != nil {
		return err
	}
	if run.State.Terminal() {
		return &TerminalRunError{RunID: run.ID, State: run.State}
	}

	d.task = task
	d.nodeRun = nodeRun
	d.run = run
	d.result.RunID = run.ID
	d.result.NodeRunID = nodeRun.ID
	d.result.NodeID = nodeRun.NodeID
	return nil
}

func (d *humanTaskDecision) loadWorkflow(ctx context.Context) error {
	digest, ir, err := d.tx.WorkflowIR(ctx, d.run.WorkflowVersionID)
	if err != nil {
		return err
	}
	d.run.WorkflowDigest = digest
	if d.wf, err = d.engine.Workflow(digest, ir); err != nil {
		return err
	}
	d.node = d.wf.Nodes[d.nodeRun.NodeID]
	if d.node == nil {
		return &WorkflowError{
			Digest: digest,
			Detail: fmt.Sprintf("node run %s names node %q, which the pinned definition does not declare",
				d.nodeRun.ID, d.nodeRun.NodeID),
		}
	}
	return nil
}

// checkOutcome validates the decision against the task's own request payload
// (PRD §9.9's allowed_outcomes) — what the human was actually shown — rather
// than re-deriving the set from the live node. For a pinned, immutable
// workflow version the two cannot actually differ, but judging the decision
// against what was presented is the more honest check to make.
func (d *humanTaskDecision) checkOutcome() error {
	var request humanTaskRequest
	if len(d.task.Request) > 0 {
		if err := json.Unmarshal(d.task.Request, &request); err != nil {
			return fmt.Errorf("engine: decode human task %s request: %w", d.task.ID, err)
		}
	}
	for _, allowed := range request.AllowedOutcomes {
		if allowed == d.req.Outcome {
			return nil
		}
	}
	return &OutcomeNotAllowedError{HumanTaskID: d.task.ID, Outcome: d.req.Outcome, Allowed: request.AllowedOutcomes}
}

// humanTaskDecisionRecordType is the ledger record type a human decision's
// own content is written as — PRD §10.2's `decision`: "a selected option and
// the authority that selected it".
const humanTaskDecisionRecordType = ledger.RecordDecision

// humanTaskDecisionData is the payload of the decision record recordDecision
// appends: what was decided and on what task.
type humanTaskDecisionData struct {
	HumanTaskID string          `json:"human_task_id"`
	Outcome     string          `json:"outcome"`
	Response    json.RawMessage `json:"response,omitempty"`
}

// recordDecision is PRD §10.8's review transaction, reused as the mechanism
// by which a human decision gains authority in the ledger (PRD §9.11: "its
// review operation becomes an atomic human-review transaction protected by
// ledger version or checksum").
//
// A human may append a `proposed` record of any type directly
// (ledger.checkHumanAuthority), but `confirmed` authority is reachable only
// through ledger.CommitReview, and only for a `review` record — that flag is
// unexported and CommitReview is the only code that sets it (see
// internal/ledger's package doc). So the decision's own content is appended
// first as a proposed `decision` record, and then immediately confirmed by a
// review naming it — the human both proposing and, in the same breath,
// exercising the authority to confirm it. That composite is exactly what PRD
// §10.8's review transaction is for: nothing here mutates the decision
// record, the review append references it, and the whole thing is one
// ledger-version-guarded commit — a stale expected_ledger_version leaves
// neither record written.
//
// Any caller-named RecordIDs are folded into the same review and confirmed
// alongside the decision: this call is the human exercising authority over
// everything it read to make this call, not a separate judgment on each.
//
// The staleness check runs *before* the decision record is appended, and
// deliberately does not delegate that first check to CreateReviewRequest:
// appending the decision record itself advances the run's ledger version by
// one, so if the check ran after that append it would compare the caller's
// expected_ledger_version (read before the decision) against a version this
// very call already moved — every decision would look stale by
// construction. The run's advisory lock (taken in guard, before this method
// runs) is what makes checking first and appending second race-free: no
// other writer of this run's ledger can act between the two.
func (d *humanTaskDecision) recordDecision(ctx context.Context) error {
	l := d.tx.Ledger()

	current, err := l.LedgerVersion(ctx, d.run.ID)
	if err != nil {
		return err
	}
	if current != d.req.ExpectedLedgerVersion {
		return &ledger.StaleReviewError{
			Reason:   ledger.StaleLedgerMoved,
			Expected: d.req.ExpectedLedgerVersion,
			Actual:   current,
			Detail:   "the run's ledger has moved since the decider last read it",
		}
	}

	data, err := json.Marshal(humanTaskDecisionData{
		HumanTaskID: d.task.ID,
		Outcome:     d.req.Outcome,
		Response:    d.req.Response,
	})
	if err != nil {
		return fmt.Errorf("engine: encode human task %s decision: %w", d.task.ID, err)
	}

	decisionRecord, err := l.Append(ctx, ledger.Record{
		RecordType: humanTaskDecisionRecordType,
		RunID:      d.run.ID,
		NodeRunID:  ledger.NullableID(d.nodeRun.ID),
		Origin:     ledger.Origin{Kind: ledger.OriginHuman, ActorID: d.req.DeciderActorID},
		Authority:  ledger.AuthorityProposed,
		Data:       data,
	})
	if err != nil {
		return err
	}

	recordIDs := append([]string{decisionRecord.ID}, d.req.RecordIDs...)
	decisions := make(map[string]ledger.Verdict, len(recordIDs))
	for _, id := range recordIDs {
		decisions[id] = ledger.VerdictConfirm
	}

	// The decision record's own append already moved the run to current+1;
	// the review is created and committed against that new version, which
	// is what CreateReviewRequest/CommitReview will independently see as
	// "the run's current version" when they re-check it themselves.
	reviewVersion := current + 1

	review, err := l.CreateReviewRequest(ctx, d.run.ID, recordIDs, reviewVersion, ledger.WithReviewer(d.req.DeciderActorID))
	if err != nil {
		return err
	}
	committed, err := l.CommitReview(ctx, review.ID, decisions, reviewVersion)
	if err != nil {
		return err
	}

	d.result.LedgerRecords = append([]ledger.Record{decisionRecord}, committed.Records...)
	return nil
}

// markDecided flips the task from pending to decided. A false return from
// MarkHumanTaskDecided means a racing decision already won — guard's
// pending check already covers the ordinary case, this is the belt to that
// braces for the window between guard's read and this write.
func (d *humanTaskDecision) markDecided(ctx context.Context) error {
	response, err := json.Marshal(humanTaskResponse{
		Outcome:        d.req.Outcome,
		DeciderActorID: d.req.DeciderActorID,
		Response:       d.req.Response,
		DecidedAt:      d.now,
	})
	if err != nil {
		return fmt.Errorf("engine: encode human task %s response: %w", d.task.ID, err)
	}

	decided, err := d.tx.MarkHumanTaskDecided(ctx, d.task.ID, response, d.now)
	if err != nil {
		return err
	}
	if !decided {
		return &HumanTaskAlreadyDecidedError{HumanTaskID: d.task.ID, Status: HumanTaskStatusDecided}
	}
	return nil
}

// humanTaskResponse is the JSON shape written to human_tasks.response.
type humanTaskResponse struct {
	Outcome        string          `json:"outcome"`
	DeciderActorID string          `json:"decider_actor_id"`
	Response       json.RawMessage `json:"response,omitempty"`
	DecidedAt      time.Time       `json:"decided_at"`
}

func (d *humanTaskDecision) emit(ctx context.Context, eventType string, data map[string]any) error {
	if _, err := d.tx.AppendEvent(ctx, d.run.ID, event(eventType, data)); err != nil {
		return err
	}
	d.result.EventTypes = append(d.result.EventTypes, eventType)
	return nil
}

// transition is completion.transition's counterpart for a human decision:
// complete the waiting_human node run and route the edge the decision's
// outcome selects (§12.5 steps 6, 8, 9's shape, without an attempt).
func (d *humanTaskDecision) transition(ctx context.Context, outcome string, state NodeRunState) error {
	transitions, err := d.tx.TransitionCount(ctx, d.run.ID)
	if err != nil {
		return err
	}
	visits, err := d.tx.NodeVisits(ctx, d.run.ID)
	if err != nil {
		return err
	}

	plan := planTransition(transitionInput{
		Workflow:    d.wf,
		NodeID:      d.nodeRun.NodeID,
		Outcome:     outcome,
		RunInput:    d.run.Input,
		Output:      d.req.Response,
		Transitions: transitions,
		Visits:      visits,
		Elapsed:     d.now.Sub(d.run.CreatedAt),
	})

	if err := d.tx.UpdateNodeRun(ctx, d.nodeRun.ID, state, outcome); err != nil {
		return err
	}
	if err := d.tx.ConsumeToken(ctx, d.nodeRun.TokenID); err != nil {
		return err
	}
	d.result.NodeRunState = state
	d.result.Outcome = outcome

	switch {
	case plan.Bound != nil:
		return d.failBound(ctx, plan.Bound, transitions)
	case plan.Complete:
		return d.completeRun(ctx, d.nodeRun.NodeID, transitions)
	case len(plan.Targets) == 0:
		return d.failRun(ctx, state, plan.Diagnostic)
	default:
		return d.advance(ctx, plan, transitions, visits)
	}
}

// advance mirrors completion.advance: create the next token position and
// node run, dispatching it exactly as any other transition would (another
// approval node's decision may itself route straight into a further human
// task, and that is not a special case here).
func (d *humanTaskDecision) advance(ctx context.Context, plan transitionPlan, transitions int, visits map[string]int) error {
	target := plan.Targets[0] // an approval node is never a parallel node, so the plan is singular
	next := d.wf.Nodes[target.NextNodeID]
	if next == nil {
		return &WorkflowError{
			Digest: d.wf.Digest,
			Detail: fmt.Sprintf("edge %q targets node %q, which the pinned definition does not declare", target.Edge.From, target.NextNodeID),
		}
	}
	if next.Kind == kindJoin {
		// The join-arrival path lives on the completion transaction
		// (parallel.go); this human-decision slice does not implement it,
		// and the compiler refuses approval-outcome edges into a join
		// (graph.approval_into_join) so authored workflows cannot get here.
		// This guard keeps a hand-built IR loud instead of corrupting a
		// barrier with a non-arrival node run.
		return d.failRun(ctx, d.result.NodeRunState, fmt.Sprintf(
			"approval node %q routed directly into join node %q, which the human-decision path does not support; route the decision through an intermediate node", d.nodeRun.NodeID, next.ID))
	}

	// Group propagation (parallel-tokens design §3.3): an approval node
	// inside a split branch keeps its branch in the group.
	sourceToken, err := d.tx.Token(ctx, d.nodeRun.TokenID)
	if err != nil {
		return err
	}

	token := Token{
		ID:            d.engine.newID(),
		NamespaceID:   d.run.NamespaceID,
		RunID:         d.run.ID,
		NodeID:        target.NextNodeID,
		State:         TokenActive,
		ParentTokenID: d.nodeRun.TokenID,
		GroupID:       sourceToken.GroupID,
		CreatedAt:     d.now,
	}
	if err := d.tx.InsertToken(ctx, token); err != nil {
		return err
	}

	nodeRun := NodeRun{
		ID:          d.engine.newID(),
		NamespaceID: d.run.NamespaceID,
		RunID:       d.run.ID,
		TokenID:     token.ID,
		NodeID:      target.NextNodeID,
		State:       dispatchState(next.Kind),
		VisitCount:  visits[target.NextNodeID] + 1,
		CreatedAt:   d.now,
		UpdatedAt:   d.now,
	}
	if err := d.tx.InsertNodeRun(ctx, nodeRun); err != nil {
		return err
	}

	d.result.NextNodeID = nodeRun.NodeID
	d.result.NextNodeRunID = nodeRun.ID
	d.result.EdgeFrom = target.Edge.From
	d.result.Transitions = transitions + 1

	if err := d.emit(ctx, TypeTokenTransitioned, map[string]any{
		"run_id":        d.run.ID,
		"node_run_id":   nodeRun.ID,
		"from_node":     d.nodeRun.NodeID,
		"to_node":       nodeRun.NodeID,
		"outcome":       target.Edge.FromOutcome,
		"edge":          target.Edge.From,
		"guard":         target.Edge.When,
		"from_token_id": d.nodeRun.TokenID,
		"token_id":      token.ID,
		"transition":    transitions + 1,
		"visit":         nodeRun.VisitCount,
	}); err != nil {
		return err
	}

	if next.Kind == kindEnd {
		if err := d.tx.UpdateNodeRun(ctx, nodeRun.ID, NodeRunCompleted, ""); err != nil {
			return err
		}
		if err := d.tx.ConsumeToken(ctx, token.ID); err != nil {
			return err
		}
		return d.completeRun(ctx, nodeRun.NodeID, transitions+1)
	}

	workID, humanTaskID, err := d.engine.dispatchNode(ctx, d.tx, next, d.run, nodeRun, plan.Edge.FromNode, plan.Edge.FromOutcome, d.now)
	if err != nil {
		return err
	}

	if humanTaskID != "" {
		d.result.NextHumanTaskID = humanTaskID
		if err := d.emit(ctx, TypeHumanTaskCreated, map[string]any{
			"run_id":        d.run.ID,
			"node_run_id":   nodeRun.ID,
			"node_id":       nodeRun.NodeID,
			"token_id":      token.ID,
			"human_task_id": humanTaskID,
			"visit":         nodeRun.VisitCount,
		}); err != nil {
			return err
		}
	} else {
		if err := d.emit(ctx, TypeNodeRunReady, map[string]any{
			"run_id":      d.run.ID,
			"node_run_id": nodeRun.ID,
			"node_id":     nodeRun.NodeID,
			"token_id":    token.ID,
			"work_id":     workID,
			"visit":       nodeRun.VisitCount,
		}); err != nil {
			return err
		}
	}

	d.result.RunState = RunRunning
	return d.finish(ctx)
}

func (d *humanTaskDecision) completeRun(ctx context.Context, endNodeID string, transitions int) error {
	output, err := d.resolveRunOutput(ctx, endNodeID)
	if err != nil {
		var contractErr *ContractError
		if errors.As(err, &contractErr) {
			return d.failRun(ctx, d.result.NodeRunState, contractErr.Error())
		}
		return err
	}

	if err := d.tx.UpdateRunState(ctx, d.run.ID, RunCompleted, output); err != nil {
		return err
	}
	if err := d.emit(ctx, TypeRunCompleted, map[string]any{
		"run_id":      d.run.ID,
		"node_run_id": d.nodeRun.ID,
		"end_node":    endNodeID,
		"transitions": transitions,
	}); err != nil {
		return err
	}

	d.result.RunState = RunCompleted
	d.result.RunOutput = output
	d.result.Transitions = transitions
	return d.finish(ctx)
}

func (d *humanTaskDecision) failRun(ctx context.Context, nodeRunState NodeRunState, detail string) error {
	if err := d.tx.UpdateNodeRun(ctx, d.nodeRun.ID, nodeRunState, d.outcome); err != nil {
		return err
	}
	if err := d.tx.ConsumeToken(ctx, d.nodeRun.TokenID); err != nil {
		return err
	}
	if err := d.tx.UpdateRunState(ctx, d.run.ID, RunFailed, nil); err != nil {
		return err
	}
	if err := d.emit(ctx, TypeRunFailed, map[string]any{
		"run_id":        d.run.ID,
		"node_run_id":   d.nodeRun.ID,
		"node_id":       d.nodeRun.NodeID,
		"human_task_id": d.task.ID,
		"state":         string(RunFailed),
		"detail":        detail,
	}); err != nil {
		return err
	}

	d.result.NodeRunState = nodeRunState
	d.result.RunState = RunFailed
	d.result.Diagnostic = detail
	return d.finish(ctx)
}

func (d *humanTaskDecision) failBound(ctx context.Context, bound *BoundExceeded, transitions int) error {
	if err := d.tx.UpdateRunState(ctx, d.run.ID, RunFailed, nil); err != nil {
		return err
	}
	if err := d.emit(ctx, TypeRunBounded, map[string]any{
		"run_id":       d.run.ID,
		"node_run_id":  d.nodeRun.ID,
		"from_node":    d.nodeRun.NodeID,
		"blocked_node": bound.NodeID,
		"bound":        string(bound.Kind),
		"limit":        bound.Limit,
		"actual":       bound.Actual,
		"transitions":  transitions,
	}); err != nil {
		return err
	}

	d.result.Bound = bound
	d.result.RunState = RunFailed
	d.result.Transitions = transitions
	d.result.Diagnostic = fmt.Sprintf("run stopped at %s: %s reached %s (limit %s)",
		bound.NodeID, bound.Kind, bound.Actual, bound.Limit)
	return d.finish(ctx)
}

func (d *humanTaskDecision) finish(ctx context.Context) error {
	if d.result.Transitions == 0 {
		transitions, err := d.tx.TransitionCount(ctx, d.run.ID)
		if err != nil {
			return err
		}
		d.result.Transitions = transitions
	}
	if d.result.RunState == "" {
		d.result.RunState = d.run.State
	}
	return nil
}

func (d *humanTaskDecision) resolveRunOutput(ctx context.Context, endNodeID string) (json.RawMessage, error) {
	end := d.wf.Nodes[endNodeID]
	if end == nil || end.OutputFrom == "" {
		return json.RawMessage("null"), nil
	}

	output, err := resolveBinding(ctx, d.tx, d.run, end.OutputFrom)
	if err != nil {
		return nil, &ContractError{
			What:   "run output",
			Detail: fmt.Sprintf("end node %q binds %s, which did not resolve: %v", endNodeID, end.OutputFrom, err),
		}
	}
	if err := validatePayload(d.wf.OutputSchema, output); err != nil {
		return nil, &ContractError{
			What:   "run output",
			Detail: fmt.Sprintf("end node %q produced a result that violates the workflow output contract: %v", endNodeID, err),
		}
	}
	return output, nil
}
