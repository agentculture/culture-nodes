package api

import (
	"encoding/json"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The types in this file are the API's own wire shapes (api/openapi/openapi.yaml's
// components.schemas), kept separate from internal/engine's and
// internal/store/postgres's Go types even where the fields line up 1:1: those
// packages' structs carry no json tags (they were never meant to cross a
// wire), and coupling the API's JSON shape to their field names would let an
// unrelated refactor of either package silently change the documented
// contract. internal/ledger.Record and internal/ledger.Projection are the
// one exception — both already carry the exact json tags PRD §10.3 and §10.9
// specify, so this package serializes them directly.

// WorkflowVersionOut is one workflow_versions row, as documented in
// components.schemas.WorkflowVersion.
type WorkflowVersionOut struct {
	ID           string          `json:"id"`
	WorkflowKey  string          `json:"workflow_key"`
	Version      int32           `json:"version"`
	SourceFormat string          `json:"source_format"`
	Source       string          `json:"source"`
	NormalizedIR json.RawMessage `json:"normalized_ir"`
	Digest       string          `json:"digest"`
	CreatedAt    time.Time       `json:"created_at"`
}

func workflowVersionOut(v postgres.WorkflowVersion) WorkflowVersionOut {
	return WorkflowVersionOut{
		ID:           v.ID,
		WorkflowKey:  v.WorkflowKey,
		Version:      v.Version,
		SourceFormat: v.SourceFormat,
		Source:       v.Source,
		NormalizedIR: nonNullJSON(v.NormalizedIR),
		Digest:       v.ContentDigest,
		CreatedAt:    v.CreatedAt,
	}
}

// WorkflowVersionListOut is components.schemas.WorkflowVersionList.
type WorkflowVersionListOut struct {
	Items []WorkflowVersionOut `json:"items"`
}

// RunOut is one run, as documented in components.schemas.Run.
type RunOut struct {
	ID             string          `json:"id"`
	WorkflowDigest string          `json:"workflow_digest"`
	State          string          `json:"state"`
	Input          json.RawMessage `json:"input,omitempty"`
	Output         json.RawMessage `json:"output,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
}

func runOut(r engine.Run) RunOut {
	out := RunOut{
		ID:             r.ID,
		WorkflowDigest: r.WorkflowDigest,
		State:          string(r.State),
		Input:          nonNullJSON(r.Input),
		Output:         nonNullJSON(r.Output),
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
	if !r.CompletedAt.IsZero() {
		completedAt := r.CompletedAt
		out.CompletedAt = &completedAt
	}
	return out
}

// RunListOut is components.schemas.RunList.
type RunListOut struct {
	Items []RunOut `json:"items"`
}

// TokenOut is one control token, as documented in components.schemas.Token.
type TokenOut struct {
	ID            string     `json:"id"`
	NodeID        string     `json:"node_id"`
	State         string     `json:"state"`
	ParentTokenID string     `json:"parent_token_id,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ConsumedAt    *time.Time `json:"consumed_at,omitempty"`
}

// AttemptOut is one dispatch attempt, as documented in components.schemas.Attempt.
type AttemptOut struct {
	ID            string          `json:"id"`
	NodeRunID     string          `json:"node_run_id"`
	AttemptNumber int             `json:"attempt_number"`
	ActorID       string          `json:"actor_id,omitempty"`
	Status        string          `json:"status"`
	FencingToken  int64           `json:"fencing_token,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	StartedAt     time.Time       `json:"started_at"`
	CompletedAt   *time.Time      `json:"completed_at,omitempty"`
}

// NodeRunOut is one node run with its attempts nested, as documented in
// components.schemas.NodeRun.
type NodeRunOut struct {
	ID          string       `json:"id"`
	TokenID     string       `json:"token_id,omitempty"`
	NodeID      string       `json:"node_id"`
	State       string       `json:"state"`
	Outcome     string       `json:"outcome,omitempty"`
	VisitCount  int          `json:"visit_count"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
	CompletedAt *time.Time   `json:"completed_at,omitempty"`
	Attempts    []AttemptOut `json:"attempts"`
}

// RunViewOut is the Run-view payload, as documented in components.schemas.RunView.
type RunViewOut struct {
	Run      RunOut       `json:"run"`
	Tokens   []TokenOut   `json:"tokens"`
	NodeRuns []NodeRunOut `json:"node_runs"`
}

// NodeRunListItemOut is one row of GET /v1alpha1/node-runs — the cross-run
// "jobs timeline" listing (task t11) — as documented in
// components.schemas.NodeRunListItem. It is the same node_runs row NodeRun
// (above) documents, listed across every run in the namespace rather than
// nested under one: NodeID and State carry forward NodeRunOut's own
// translation of this row's underlying node_runs.node_key/status columns
// (see this file's header comment), with RunID added (the parent run is not
// implied by a URL path here, unlike GET /v1alpha1/runs/{id}) and ActorID
// added (the most recent attempt's actor/runner reference — see
// queries.go's latestAttemptActorIDs; empty until the node run has been
// dispatched at least once, the same optional reference AttemptOut.ActorID
// already is).
type NodeRunListItemOut struct {
	ID          string     `json:"id"`
	RunID       string     `json:"run_id"`
	NodeID      string     `json:"node_id"`
	ActorID     string     `json:"actor_id,omitempty"`
	State       string     `json:"state"`
	Outcome     string     `json:"outcome,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// NodeRunListOut is components.schemas.NodeRunList: a page of
// NodeRunListItemOut plus the opaque keyset cursor for the next page (see
// queries.go's listNodeRunsAcrossRuns for why this endpoint paginates by
// cursor rather than offset). NextCursor is empty — and omitted — once the
// caller has reached the last page.
type NodeRunListOut struct {
	Items      []NodeRunListItemOut `json:"items"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

// LedgerRecordsOut is components.schemas.LedgerRecords.
type LedgerRecordsOut struct {
	Items         []ledger.Record `json:"items"`
	LedgerVersion int64           `json:"ledger_version"`
}

// ReviewRequestOut is components.schemas.ReviewRequest. ledger.ReviewRequest
// carries no json tags of its own (see this file's header comment), so this
// wrapper supplies the documented field names.
type ReviewRequestOut struct {
	ID              string    `json:"id"`
	RunID           string    `json:"run_id"`
	ReviewerActorID string    `json:"reviewer_actor_id,omitempty"`
	Status          string    `json:"status"`
	LedgerVersion   int64     `json:"ledger_version"`
	FrameChecksum   string    `json:"frame_checksum"`
	RecordIDs       []string  `json:"record_ids"`
	CreatedAt       time.Time `json:"created_at"`
}

func reviewRequestOut(r ledger.ReviewRequest) ReviewRequestOut {
	ids := r.RecordIDs
	if ids == nil {
		ids = []string{}
	}
	return ReviewRequestOut{
		ID:              r.ID,
		RunID:           r.RunID,
		ReviewerActorID: r.ReviewerActorID,
		Status:          string(r.Status),
		LedgerVersion:   r.LedgerVersion,
		FrameChecksum:   r.FrameChecksum,
		RecordIDs:       ids,
		CreatedAt:       r.CreatedAt,
	}
}

// ReviewCommitResultOut is components.schemas.ReviewCommitResult.
type ReviewCommitResultOut struct {
	ReviewID      string          `json:"review_id"`
	Records       []ledger.Record `json:"records"`
	LedgerVersion int64           `json:"ledger_version"`
}

// HumanTaskOut is one human_tasks row, as documented in
// components.schemas.HumanTask. Request is the PRD §9.9 payload t6's
// dispatch stored (decision schema, approver ref, deadline, allowed
// outcomes, context refs, audit) — carried verbatim, never re-derived,
// because it is a record of what the human was actually shown.
type HumanTaskOut struct {
	ID              string          `json:"id"`
	RunID           string          `json:"run_id"`
	NodeRunID       string          `json:"node_run_id,omitempty"`
	Kind            string          `json:"kind"`
	AssignedOwnerID string          `json:"assigned_owner_id,omitempty"`
	Status          string          `json:"status"`
	Request         json.RawMessage `json:"request"`
	Response        json.RawMessage `json:"response,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	ResolvedAt      *time.Time      `json:"resolved_at,omitempty"`
}

func humanTaskOut(t engine.HumanTask) HumanTaskOut {
	out := HumanTaskOut{
		ID:              t.ID,
		RunID:           t.RunID,
		NodeRunID:       t.NodeRunID,
		Kind:            t.Kind,
		AssignedOwnerID: t.AssignedOwnerID,
		Status:          t.Status,
		Request:         t.Request,
		Response:        nonNullJSON(t.Response),
		CreatedAt:       t.CreatedAt,
	}
	if !t.ResolvedAt.IsZero() {
		resolvedAt := t.ResolvedAt
		out.ResolvedAt = &resolvedAt
	}
	return out
}

// HumanTaskListOut is components.schemas.HumanTaskList.
type HumanTaskListOut struct {
	Items []HumanTaskOut `json:"items"`
}

// HumanTaskDecisionResultOut is components.schemas.HumanTaskDecisionResult:
// what committing a decision did, mirroring engine.CompletionResult's shape
// the way ReviewCommitResultOut mirrors ledger.ReviewResult.
type HumanTaskDecisionResultOut struct {
	HumanTaskID     string          `json:"human_task_id"`
	RunID           string          `json:"run_id"`
	NodeRunID       string          `json:"node_run_id"`
	Outcome         string          `json:"outcome"`
	LedgerRecords   []ledger.Record `json:"ledger_records"`
	NextNodeID      string          `json:"next_node_id,omitempty"`
	NextNodeRunID   string          `json:"next_node_run_id,omitempty"`
	NextHumanTaskID string          `json:"next_human_task_id,omitempty"`
	RunState        string          `json:"run_state"`
	RunOutput       json.RawMessage `json:"run_output,omitempty"`
}

func humanTaskDecisionResultOut(humanTaskID string, result engine.CompletionResult) HumanTaskDecisionResultOut {
	records := result.LedgerRecords
	if records == nil {
		records = []ledger.Record{}
	}
	return HumanTaskDecisionResultOut{
		HumanTaskID:     humanTaskID,
		RunID:           result.RunID,
		NodeRunID:       result.NodeRunID,
		Outcome:         result.Outcome,
		LedgerRecords:   records,
		NextNodeID:      result.NextNodeID,
		NextNodeRunID:   result.NextNodeRunID,
		NextHumanTaskID: result.NextHumanTaskID,
		RunState:        string(result.RunState),
		RunOutput:       nonNullJSON(result.RunOutput),
	}
}

// HealthOut is components.schemas.Health.
type HealthOut struct {
	Status string `json:"status"`
}

// nonNullJSON returns raw unchanged, or nil (which json.Marshal omits under
// `omitempty`) when raw is empty — distinct from an explicit JSON null,
// which a caller-supplied `json.RawMessage("null")` still renders as-is.
func nonNullJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
