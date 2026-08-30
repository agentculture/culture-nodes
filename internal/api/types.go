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

// RunOut is one run, as documented in components.schemas.Run. Usage is set
// only where the caller actually computed a rollup (runOut below, used by
// createRun/getRun/cancelRun) — never by listRuns' own runRow.out() in
// queries.go, which would otherwise need one extra query per listed run.
// Its absence (nil, omitted) from a runs-list response therefore means "not
// computed for this endpoint", distinguishable from run detail's Usage,
// which is always present (even when every field inside it is zero — see
// UsageOut's doc comment for what that zero state means).
type RunOut struct {
	ID             string          `json:"id"`
	WorkflowDigest string          `json:"workflow_digest"`
	WorkflowKey    string          `json:"workflow_key,omitempty"`
	State          string          `json:"state"`
	Input          json.RawMessage `json:"input,omitempty"`
	Output         json.RawMessage `json:"output,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	Usage          *UsageOut       `json:"usage,omitempty"`
	// Name, Description, and Category are task t3's optional run metadata
	// (migrations/0013): Name and Description are operator-given at
	// creation only (POST /v1alpha1/runs), never changed afterward.
	// Category alone is retaggable via PATCH /v1alpha1/runs/{id} (frame
	// decision q4) — see runMetadata's doc comment in queries.go.
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category,omitempty"`
	// DisplayHint is a truncated, best-effort guess at what this run is
	// about, derived at read time from a request/instruction/task-ish
	// string field in the run's own input (deriveDisplayHint in runs.go) —
	// never persisted, and never rendered when Name is set. A UI must read
	// Name (operator-given) and DisplayHint (a guess) as distinct fields
	// so it never presents a derived hint as if an operator had actually
	// named the run.
	DisplayHint string `json:"display_hint,omitempty"`
	// ActorAffinity is the routing this run's workflow declared and resolved
	// at creation (task t33, migrations/0034), keyed by node id:
	// {"fix":{"actor":"actor://company/developer","rule":"security-findings"}}.
	// Absent when the run resolved none. It is on the RUN rather than only in
	// the node runs because the point of recording it is the per-actor
	// comparative record -- what the workflow said this work WAS, readable
	// beside the run's own state, timings, and usage.
	ActorAffinity json.RawMessage `json:"actor_affinity,omitempty"`
	// Subject is the triggering event's correlation key (task t15,
	// migrations/0038, spec c31/h16), e.g. a Jira issue key. Empty for an
	// operator-created run, or a triggered run whose event carried none. It
	// is what makes "exactly one active run in the run list" for a subject a
	// question this listing can actually answer, rather than one that needs
	// a database query — see honesty condition h16.
	Subject string `json:"subject,omitempty"`
}

// runOut renders r with usage (the run-level §13.2 rollup task t2 adds,
// postgres.EngineStore.RunUsage) and meta (task t3's name/description/
// category, queries.go's runMetadataByID) — every call site fetches both
// fresh rather than this function reaching into the database itself,
// keeping runOut a pure function the way it always has been. DisplayHint
// is derived here, from r.Input, only when meta.Name is empty — see
// RunOut's doc comment above for why a UI must be able to tell the two
// apart.
func runOut(r engine.Run, usage postgres.UsageRollup, meta runMetadata) RunOut {
	out := RunOut{
		ID:             r.ID,
		WorkflowDigest: r.WorkflowDigest,
		State:          string(r.State),
		Input:          nonNullJSON(r.Input),
		Output:         nonNullJSON(r.Output),
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
		Usage:          usageOut(usage),
		Name:           meta.Name,
		Description:    meta.Description,
		Category:       meta.Category,
		ActorAffinity:  r.ActorAffinity,
		Subject:        r.Subject,
	}
	if meta.Name == "" {
		out.DisplayHint = deriveDisplayHint(r.Input)
	}
	if !r.CompletedAt.IsZero() {
		completedAt := r.CompletedAt
		out.CompletedAt = &completedAt
	}
	return out
}

// UsageOut is the §13.2 usage/cost rollup attached to a run or a node run,
// as documented in components.schemas.Usage. It renders postgres.UsageRollup
// (see that type's doc comment for the full aggregation rules — retry burn
// included, unreported attempts excluded from sums, cost never summed
// across currencies) as the wire shape:
//
//   - AttemptsReported == 0 (with InputTokens == OutputTokens == 0) is "no
//     attempt in scope reported usage" — distinct from AttemptsReported > 0
//     with InputTokens/OutputTokens == 0, which is "an attempt reported
//     usage and it was genuinely zero tokens". A caller must read
//     attempts_reported/attempts_not_reported to tell the two apart; the
//     token fields alone are ambiguous between them by design (0 is a
//     legitimate sum of an empty set AND a legitimate reported value).
//   - Cost/Currency are set together only when every cost-reporting
//     attempt in scope agreed on one currency ("coherent") — including the
//     case where that shared value is "" (cost reported, currency
//     unknown), which renders as Cost set and Currency omitted.
//   - CostByCurrency is set instead, as a list, whenever more than one
//     distinct currency was seen — mixed currencies are exposed
//     per-currency, never summed into one misleading number.
//   - When no attempt in scope reported a cost at all, neither Cost,
//     Currency, nor CostByCurrency is set.
//   - CachedInputTokens/ReasoningTokens (ADR 0009, task t2) sum
//     postgres.UsageRollup's own fields the same way InputTokens/OutputTokens
//     do: an attempt that reported tokens but no cache telemetry at all
//     contributes nothing to CachedInputTokens, never a fabricated zero
//     counted as "measured 0% cached" — see UsageRollup's doc comment. They
//     are NOT independently gated by their own reported/not-reported count;
//     AttemptsReported/AttemptsNotReported remains the one coverage signal,
//     per the ADR's explicit instruction not to invent a second sentinel.
//   - CacheRatio is CachedInputTokens/InputTokens, computed only when
//     InputTokens > 0 — never a fabricated 0/0 ratio when nothing in scope
//     reported any input tokens at all. Omitted (nil) in that case.
type UsageOut struct {
	InputTokens         int64             `json:"input_tokens"`
	OutputTokens        int64             `json:"output_tokens"`
	CachedInputTokens   int64             `json:"cached_input_tokens"`
	ReasoningTokens     int64             `json:"reasoning_tokens"`
	CacheRatio          *float64          `json:"cache_ratio,omitempty"`
	Cost                *float64          `json:"cost,omitempty"`
	Currency            string            `json:"currency,omitempty"`
	CostByCurrency      []CurrencyCostOut `json:"cost_by_currency,omitempty"`
	AttemptsReported    int               `json:"attempts_reported"`
	AttemptsNotReported int               `json:"attempts_not_reported"`
}

// CurrencyCostOut is one UsageOut.CostByCurrency entry. Currency is omitted
// (not the empty string in the rendered JSON) when the attempt(s) it
// summarizes reported a cost with no currency at all — see
// postgres.CurrencyCost's doc comment.
type CurrencyCostOut struct {
	Currency string  `json:"currency,omitempty"`
	Cost     float64 `json:"cost"`
}

// usageOut renders a postgres.UsageRollup as the wire shape — always a
// non-nil *UsageOut, per RunOut's doc comment above: computing a rollup at
// all always yields a usage object, even one whose every field is zero.
func usageOut(r postgres.UsageRollup) *UsageOut {
	out := &UsageOut{
		InputTokens:         r.InputTokens,
		OutputTokens:        r.OutputTokens,
		CachedInputTokens:   r.CachedInputTokens,
		ReasoningTokens:     r.ReasoningTokens,
		AttemptsReported:    r.AttemptsReported,
		AttemptsNotReported: r.AttemptsNotReported,
	}
	if r.InputTokens > 0 {
		ratio := float64(r.CachedInputTokens) / float64(r.InputTokens)
		out.CacheRatio = &ratio
	}
	switch len(r.Cost) {
	case 0:
		// No attempt in scope reported a cost — Cost/Currency/CostByCurrency
		// all stay unset.
	case 1:
		cost := r.Cost[0].Cost
		out.Cost = &cost
		out.Currency = r.Cost[0].Currency
	default:
		out.CostByCurrency = make([]CurrencyCostOut, len(r.Cost))
		for i, cc := range r.Cost {
			out.CostByCurrency[i] = CurrencyCostOut{Currency: cc.Currency, Cost: cc.Cost}
		}
	}
	return out
}

// RunListOut is components.schemas.RunList.
type RunListOut struct {
	Items      []RunOut `json:"items"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// TokenOut is one control token, as documented in components.schemas.Token.
//
// A token has a parent OR an origin event OR neither (the entry token) —
// never both. OriginEventID is how a run-detail surface renders an
// event-pickup token honestly: it is a ROOT, because nothing in this run
// handed it control, but it is an explained root rather than an orphan
// (issue #43, review finding D4). A consumer that draws the ancestry tree
// must therefore tolerate several roots per run and label the ones carrying
// origin_event_id with the fact that created them.
type TokenOut struct {
	ID            string     `json:"id"`
	NodeID        string     `json:"node_id"`
	State         string     `json:"state"`
	ParentTokenID string     `json:"parent_token_id,omitempty"`
	OriginEventID string     `json:"origin_event_id,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ConsumedAt    *time.Time `json:"consumed_at,omitempty"`
}

// AttemptOut is one dispatch attempt, as documented in components.schemas.Attempt.
//
// PreserveBranch/PreservePushed/PreserveRemote (task t26, issue #49, spec
// claim c32 / honesty h21; migrations/0025_attempt_preserve_branch.sql) are
// task t25's bridge-minted preserve-on-failure branch, present only on an
// attempt whose bridge actually committed one — most attempts, including
// every successful one, carry none. PreserveBranch is the presence check:
// PreservePushed/PreserveRemote are only ever populated alongside it (see
// the migration's own header), so a reader checks PreserveBranch first.
//
// Supersedes (task t11, ADR 0012; migrations/0028_attempt_supersedes.sql) is
// the attempt this record corrects, empty on every ordinary dispatch. It
// exists because a node run whose deadline expired and whose session later
// reported back has TWO attempts here — the timed_out record, and the
// correction carrying what the session actually did — and a reader shown
// both without being told which is which would reasonably conclude the node
// was dispatched twice.
type AttemptOut struct {
	ID                string           `json:"id"`
	NodeRunID         string           `json:"node_run_id"`
	AttemptNumber     int              `json:"attempt_number"`
	ActorID           string           `json:"actor_id,omitempty"`
	Status            string           `json:"status"`
	FencingToken      int64            `json:"fencing_token,omitempty"`
	Result            json.RawMessage  `json:"result,omitempty"`
	StartedAt         time.Time        `json:"started_at"`
	CompletedAt       *time.Time       `json:"completed_at,omitempty"`
	Usage             *AttemptUsageOut `json:"usage,omitempty"`
	TerminationReason string           `json:"termination_reason,omitempty"`
	ContinuationRef   string           `json:"continuation_ref,omitempty"`
	PreserveBranch    string           `json:"preserve_branch,omitempty"`
	PreservePushed    *bool            `json:"preserve_pushed,omitempty"`
	PreserveRemote    string           `json:"preserve_remote,omitempty"`
	Supersedes        string           `json:"supersedes,omitempty"`
}

// AttemptUsageOut is one attempt's reported telemetry, kept separate from
// UsageOut because it is attribution rather than an aggregate.
//
// UsageModel is emitted VERBATIM, including the "unknown:<backend>-backend-
// cannot-report" sentinels an adapter sends when its backend genuinely cannot
// name a model. Those are facts, not missing values, and collapsing them to
// null would restore the #77 ambiguity this field exists to remove: a null was
// indistinguishable from nobody having written the field at all.
//
// The sentinel shape is described rather than exemplified on purpose --
// tests/lint's neutrality gate refuses provider names in runtime code (PRD
// §9.5), and this API does not branch on them either. It relays what the
// adapter reported.
type AttemptUsageOut struct {
	InputTokens       int64    `json:"input_tokens"`
	OutputTokens      int64    `json:"output_tokens"`
	Cost              *float64 `json:"cost,omitempty"`
	Currency          string   `json:"currency,omitempty"`
	CachedInputTokens *int64   `json:"cached_input_tokens,omitempty"`
	ReasoningTokens   *int64   `json:"reasoning_tokens,omitempty"`
	UsageModel        *string  `json:"usage_model,omitempty"`
	ThreadID          *string  `json:"thread_id,omitempty"`
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
// already is). Usage is task t2's §13.2 rollup over this node run's own
// attempts (postgres.EngineStore.NodeRunUsages, batched across the page —
// see listNodeRunsAcrossRuns in queries.go), always present the same way
// RunOut's is on run detail — see UsageOut's doc comment for what its zero
// state means.
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
	Usage       *UsageOut  `json:"usage,omitempty"`
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
