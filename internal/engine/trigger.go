package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TriggerWorkflow is the immutable published definition offered to an event
// delivery. Delivery selects only the newest version of each workflow key.
type TriggerWorkflow struct {
	Digest, SourceFormat, Source string
	IR                           json.RawMessage
}

// TriggeredRun reports a handler that matched and the run it created.
type TriggeredRun struct {
	WorkflowDigest string `json:"workflow_digest"`
	RunID          string `json:"run_id"`
}

// EventTriggerRunner is the engine slice used by signal delivery.
type EventTriggerRunner interface {
	TriggerEvent(context.Context, Tx, TriggerWorkflow, PickupEvent) ([]TriggeredRun, error)
}

var _ EventTriggerRunner = (*Engine)(nil)

// TriggerEvent evaluates the definition's handlers and creates one run for
// each match. The event payload is the run input. It is called inside the
// signal delivery transaction, after the immutable fact has been appended.
func (e *Engine) TriggerEvent(ctx context.Context, tx Tx, candidate TriggerWorkflow, ev PickupEvent) ([]TriggeredRun, error) {
	wf, err := e.Workflow(candidate.Digest, candidate.IR)
	if err != nil {
		return nil, err
	}
	var out []TriggeredRun
	for _, trigger := range wf.Triggers {
		if trigger.OnEvent != ev.Name {
			continue
		}
		if trigger.Condition != nil {
			value, _, evalErr := trigger.Condition.Eval(map[string]any{
				celVarInput: map[string]any{}, celVarOutput: map[string]any{}, celVarOutcome: "",
				celVarEvent: map[string]any{"name": ev.Name, "emitter": ev.Emitter, "payload": decodeActivation(ev.Payload)},
			})
			if evalErr != nil {
				return nil, fmt.Errorf("trigger condition %q: %w", trigger.When, evalErr)
			}
			ok, err := truthy(value)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		if err := validatePayload(wf.InputSchema, ev.Payload); err != nil {
			return nil, &ContractError{What: "triggered run input", Detail: err.Error()}
		}
		run, err := e.createTriggeredRunTx(ctx, tx, wf, candidate, ev)
		if err != nil {
			return nil, err
		}
		out = append(out, TriggeredRun{WorkflowDigest: candidate.Digest, RunID: run.ID})
	}
	return out, nil
}

func (e *Engine) createTriggeredRunTx(ctx context.Context, tx Tx, wf *Workflow, candidate TriggerWorkflow, ev PickupEvent) (Run, error) {
	now := e.now().UTC()
	run := Run{ID: e.newID(), NamespaceID: e.store.NamespaceID(), WorkflowDigest: candidate.Digest,
		State: RunRunning, Input: jsonOrNull(ev.Payload), CreatedAt: now, UpdatedAt: now}
	format := candidate.SourceFormat
	if format != string(compiler.FormatJSON) {
		format = string(compiler.FormatYAML)
	}
	versionID, err := tx.EnsureWorkflowVersion(ctx, WorkflowVersionInput{WorkflowKey: wf.Name,
		SourceFormat: format, Source: candidate.Source, NormalizedIR: candidate.IR, ContentDigest: candidate.Digest})
	if err != nil {
		return Run{}, err
	}
	run.WorkflowVersionID = versionID
	if err := tx.InsertRun(ctx, run); err != nil {
		return Run{}, err
	}
	if err := tx.Lock(ctx, ledger.RunLockKey(run.ID)); err != nil {
		return Run{}, err
	}
	token := Token{ID: e.newID(), NamespaceID: run.NamespaceID, RunID: run.ID, NodeID: wf.Entry, State: TokenActive, CreatedAt: now}
	if err := tx.InsertToken(ctx, token); err != nil {
		return Run{}, err
	}
	entry := wf.Nodes[wf.Entry]
	nr := NodeRun{ID: e.newID(), NamespaceID: run.NamespaceID, RunID: run.ID, TokenID: token.ID,
		NodeID: wf.Entry, State: dispatchState(entry.Kind), VisitCount: 1, CreatedAt: now, UpdatedAt: now}
	if err := tx.InsertNodeRun(ctx, nr); err != nil {
		return Run{}, err
	}
	if err := e.materializeEventRoutes(ctx, tx, wf, run, now); err != nil {
		return Run{}, err
	}
	workID, humanTaskID, err := e.dispatchNode(ctx, tx, entry, run, nr, "", "", now)
	if err != nil {
		return Run{}, err
	}
	if _, err = tx.AppendEvent(ctx, run.ID, event(TypeRunCreated, map[string]any{
		"run_id": run.ID, "workflow_version_id": versionID, "workflow_digest": candidate.Digest,
		"workflow_key": wf.Name, "entry": wf.Entry, "trigger_event_id": ev.ID,
	})); err != nil {
		return Run{}, err
	}
	if humanTaskID != "" {
		_, err = tx.AppendEvent(ctx, run.ID, event(TypeHumanTaskCreated, map[string]any{"run_id": run.ID, "node_run_id": nr.ID, "node_id": nr.NodeID, "token_id": token.ID, "human_task_id": humanTaskID, "visit": 1}))
	} else {
		_, err = tx.AppendEvent(ctx, run.ID, event(TypeNodeRunReady, map[string]any{"run_id": run.ID, "node_run_id": nr.ID, "node_id": nr.NodeID, "token_id": token.ID, "work_id": workID, "visit": 1}))
	}
	return run, err
}
