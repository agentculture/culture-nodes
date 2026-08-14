package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Approval nodes and human tasks (PRD §9.9):
//
//	"An approval node creates a human task containing: decision schema;
//	requested approver role or group; deadline; exact context and artifact
//	references; audit identity; allowed outcomes. The workflow pauses
//	without holding a worker or database transaction."
//
// This file is the engine's half of that sentence. dispatchNode is the one
// place both CreateRun (the entry node) and completion.advance (every later
// node) decide what a newly created node run's work looks like. For every
// kind but approval that is EnqueueWork, exactly as before this task.
//
// For an approval node it is instead one INSERT into human_tasks, inside the
// very same transaction that created the node run — never a call to
// EnqueueWork. That ordering is the whole trick: because no work item is
// ever created for the node run, there is nothing for it to hold a lease on,
// and the transaction that recorded the pause commits and closes exactly
// like any other completion. Nothing here waits for a human to answer;
// dispatchNode returns as soon as the row is written. This is the "park"
// model the PRD asks for, not the code-node lease-holding model: a
// lease-and-release design (like §12.6's async-actor waiting_external) would
// still imply a work item existed briefly, and none ever does here.
//
// # The seam t7/t8 build on
//
// A human task has a node_run_id but no work_id, so the node run it parks
// cannot be resumed through CompleteAttempt(WorkID, ...) — that path is
// bound to a work item by construction (see completion.guard, which starts
// by resolving WorkID to a node run). Resolving a human task therefore needs
// its own engine entry point, keyed on the human task or its node_run_id
// rather than a work id; that entry point, the API that lets a human answer
// it, and the worker-side HumanDispatcher named in internal/worker/doc.go
// are what t7/t8 build. What they inherit from this task:
//
//   - NodeRunWaitingHuman marks exactly the node runs waiting on a human
//     task, the same way NodeRunWaitingExternal marks ones waiting on an
//     async actor;
//   - human_tasks.request carries every field PRD §9.9 asks the task to
//     contain (see humanTaskRequest below);
//   - human_tasks.node_run_id is the join back to the paused run, and
//     CompletionResult.NextHumanTaskID surfaces the row's id to whatever
//     called CreateRun/CompleteAttempt so a caller need not re-derive it.
const kindApproval = "approval"

// dispatchState is the node_runs.status a newly created node run gets before
// its work is dispatched: waiting_human for an approval node (PRD §9.9 — no
// work item is ever created for it), ready for every other kind. Both
// CreateRun and completion.advance call this before InsertNodeRun, so the
// row is written once with its final initial state rather than inserted
// ready and immediately corrected.
func dispatchState(kind string) NodeRunState {
	if kind == kindApproval {
		return NodeRunWaitingHuman
	}
	return NodeRunReady
}

// dispatchNode is §12.5 step 9's dispatch half for a node run that has
// already been inserted: it either enqueues ready work (every kind but
// approval) or writes the node's human task (approval). edgeFromNode and
// edgeFromOutcome are the edge that produced this node run, empty for the
// entry node, which CreateRun passes through so a human task can say what
// put the run here.
func (e *Engine) dispatchNode(ctx context.Context, tx Tx, node *Node, run Run, nodeRun NodeRun, edgeFromNode, edgeFromOutcome string, now time.Time) (workID, humanTaskID string, err error) {
	if node.Kind != kindApproval {
		workID, err = tx.EnqueueWork(ctx, nodeRun.ID, time.Time{})
		return workID, "", err
	}

	request, err := buildHumanTaskRequest(node, run, nodeRun, edgeFromNode, edgeFromOutcome, now)
	if err != nil {
		return "", "", err
	}

	task := HumanTask{
		ID:          e.newID(),
		NamespaceID: run.NamespaceID,
		RunID:       run.ID,
		NodeRunID:   nodeRun.ID,
		Kind:        kindApproval,
		Status:      HumanTaskStatusPending,
		Request:     request,
		CreatedAt:   now,
	}
	humanTaskID, err = tx.InsertHumanTask(ctx, task)
	if err != nil {
		return "", "", err
	}
	return "", humanTaskID, nil
}

// humanTaskRequest is the JSON shape written to human_tasks.request. It
// covers every field PRD §9.9 lists except audit identity, which is already
// this row's own namespace_id/run_id/node_run_id columns; Audit repeats the
// node, token, and definition identity that are *not* columns, so a reader
// of the JSON alone still has the full trail without a join.
type humanTaskRequest struct {
	// DecisionSchemaRef is the node's decisionSchemaRef, an unresolved
	// reference like every other schemaRef this engine carries (resolving
	// one needs a source root, which is a deployment-level concern — see
	// this package's Workflow doc comment on InputSchema/OutputSchema).
	DecisionSchemaRef string `json:"decision_schema_ref,omitempty"`
	// ApproverRef is the requested approver role or group, exactly as
	// authored (e.g. "team/platform-ai-approvers") — not resolved to an
	// owners.id; see HumanTask's doc comment for why.
	ApproverRef string `json:"approver_ref,omitempty"`
	// Deadline is the absolute time this task becomes overdue: now plus the
	// node's deadline duration. Omitted (never fabricated as "now") when the
	// node carries no deadline at all, which the compiler only leaves true
	// for hand-built IR bypassing its own default-expansion.
	Deadline *time.Time `json:"deadline,omitempty"`
	// AllowedOutcomes is the node's resolved outcome set — for an approval
	// node with no explicit contract, the compiler's implied
	// approved/expired/rejected (PRD §9.2).
	AllowedOutcomes []string `json:"allowed_outcomes,omitempty"`
	// ContextRefs is the node's own input binding, carried as a reference —
	// not resolved to a value. It is "exact context and artifact
	// references" in the same sense every other node kind's input binding
	// is: a pointer the approver (or whatever surface presents this task)
	// resolves, not a payload the engine built.
	ContextRefs *humanTaskContext `json:"context_refs,omitempty"`
	Audit       humanTaskAudit    `json:"audit"`
}

// humanTaskContext mirrors the authoring input-binding shape
// (schemas/workflow/workflow.schema.json's inputBinding): a single pointer,
// named bindings, or both. A named binding renders as the author wrote it — a
// pointer string, or a `{"literal": ...}` object (issue #73) — so the surface
// presenting this task shows the reader the same declaration the workflow does.
type humanTaskContext struct {
	From     string                  `json:"from,omitempty"`
	Bindings map[string]InputBinding `json:"bindings,omitempty"`
}

// humanTaskAudit is the PRD §9.9 "audit identity" that is not already one of
// human_tasks' own columns.
type humanTaskAudit struct {
	NodeID         string `json:"node_id"`
	TokenID        string `json:"token_id,omitempty"`
	WorkflowDigest string `json:"workflow_digest,omitempty"`
	// FromNode/FromOutcome name the edge that produced this node run — empty
	// when the approval node is the workflow's entry node, which has none.
	FromNode    string `json:"from_node,omitempty"`
	FromOutcome string `json:"from_outcome,omitempty"`
}

func buildHumanTaskRequest(node *Node, run Run, nodeRun NodeRun, edgeFromNode, edgeFromOutcome string, now time.Time) (json.RawMessage, error) {
	req := humanTaskRequest{
		DecisionSchemaRef: node.DecisionSchemaRef,
		ApproverRef:       node.ApproverRef,
		AllowedOutcomes:   append([]string(nil), node.Outcomes...),
		Audit: humanTaskAudit{
			NodeID:         node.ID,
			TokenID:        nodeRun.TokenID,
			WorkflowDigest: run.WorkflowDigest,
			FromNode:       edgeFromNode,
			FromOutcome:    edgeFromOutcome,
		},
	}
	if node.Deadline > 0 {
		deadline := now.Add(node.Deadline)
		req.Deadline = &deadline
	}
	if node.InputFrom != "" || len(node.InputBindings) > 0 {
		req.ContextRefs = &humanTaskContext{From: node.InputFrom, Bindings: node.InputBindings}
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("engine: build human task request for node %q: %w", node.ID, err)
	}
	return payload, nil
}
