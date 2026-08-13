package engine

import (
	"encoding/json"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// The four runtime records PRD §3.1 keeps separate — run, token, node run,
// attempt — are four types here for the same reason they are four tables: a
// run may use several agents, one agent may execute several nodes, and a
// parallel branch may create several tokens. Collapsing any two of them would
// make one of those statements unsayable.

// RunState is the lifecycle of one execution of a workflow version.
type RunState string

// The run states. `created` exists for a run that has been recorded but not
// yet given a token; CreateRun never leaves a run in it, because the entry
// token and its node run are created in the same transaction, so a committed
// run is always already running.
const (
	RunCreated   RunState = "created"
	RunRunning   RunState = "running"
	RunWaiting   RunState = "waiting"
	RunCompleted RunState = "completed"
	RunFailed    RunState = "failed"
	RunCancelled RunState = "cancelled"
)

// Terminal reports whether the run has reached a state it never leaves.
func (s RunState) Terminal() bool {
	switch s {
	case RunCompleted, RunFailed, RunCancelled:
		return true
	default:
		return false
	}
}

// Run is one execution of one immutable workflow version (PRD §3.1).
//
// WorkflowDigest is the pin: a run names the content digest of the normalized
// IR it executes, so the definition a run is executing can never change
// underneath it even if the workflow is republished.
type Run struct {
	ID                string
	NamespaceID       string
	WorkflowVersionID string
	WorkflowDigest    string
	State             RunState
	Input             json.RawMessage
	Output            json.RawMessage
	CreatedAt         time.Time
	UpdatedAt         time.Time
	CompletedAt       time.Time

	// Name/Description/Category are operator-facing run metadata
	// (migrations/0013). The engine never branches on them — they ride
	// Run only so CreateRun can persist them inside the same transaction
	// that inserts the run row (a post-commit UPDATE gave POST /runs an
	// unknown-success window: a 5xx after the run already existed).
	// Empty string means absent; InsertRun stores NULL. They are write-
	// through fields: engine read paths do not populate them (the API
	// reads metadata back through its own queries).
	Name        string
	Description string
	Category    string
}

// RunOption adjusts the Run a CreateRun call is about to persist, inside
// the same transaction as the run row itself.
type RunOption func(*Run)

// WithRunMetadata sets operator-facing name/description/category on the
// run at creation, atomically with the insert. Empty strings mean absent.
func WithRunMetadata(name, description, category string) RunOption {
	return func(r *Run) {
		r.Name = name
		r.Description = description
		r.Category = category
	}
}

// TokenState is the lifecycle of a control token.
type TokenState string

// The token states. A token is consumed when the transition it authorized has
// been recorded; it is never deleted, so the path a run took stays readable.
const (
	TokenActive   TokenState = "active"
	TokenConsumed TokenState = "consumed"
)

// Token is a unit of control flow moving through the graph (PRD §3.2).
//
// The MVP engine is sequential — one active token per run, which is what
// limits.maxParallelTokens = 1 means — but tokens are still first-class rows
// rather than a field on the run, because §9.8's split and join model needs
// them to be, and a token that only exists implicitly cannot later be forked.
type Token struct {
	ID            string
	NamespaceID   string
	RunID         string
	NodeID        string
	State         TokenState
	ParentTokenID string
	CreatedAt     time.Time
	ConsumedAt    time.Time
}

// NodeRunState is the lifecycle of one node's logical execution.
type NodeRunState string

// The node-run states.
const (
	NodeRunReady           NodeRunState = "ready"
	NodeRunLeased          NodeRunState = "leased"
	NodeRunRunning         NodeRunState = "running"
	NodeRunWaitingExternal NodeRunState = "waiting_external"
	// NodeRunWaitingHuman is a node run parked on a human_tasks row (PRD
	// §9.9): an approval node's dispatch writes that row *instead of*
	// EnqueueWork, so this node run never had a work item to lease in the
	// first place. It parallels waiting_external's "paused, not terminal"
	// shape without sharing its machinery — no actor invocation, no
	// callback, no lease to release, because none was ever taken. See
	// internal/engine/humantask.go for the dispatch decision and the seam
	// this leaves for resolving the task later.
	NodeRunWaitingHuman NodeRunState = "waiting_human"
	NodeRunCompleted    NodeRunState = "completed"
	NodeRunFailed       NodeRunState = "failed"
	NodeRunCancelled    NodeRunState = "cancelled"
)

// Terminal reports whether the node run has reached a state it never leaves.
// A terminal node run never transitions again: see CompleteAttempt, which
// refuses one with TerminalNodeRunError.
func (s NodeRunState) Terminal() bool {
	switch s {
	case NodeRunCompleted, NodeRunFailed, NodeRunCancelled:
		return true
	default:
		return false
	}
}

// NodeRun is one node's logical execution within a run (PRD §3.1). It may
// span several attempts; Outcome is set once, by the attempt that completed
// it.
type NodeRun struct {
	ID          string
	NamespaceID string
	RunID       string
	TokenID     string
	NodeID      string
	State       NodeRunState
	Outcome     string
	VisitCount  int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt time.Time
}

// TechStatus is a *technical* status (PRD §3.4) — how the dispatch itself
// went. It is never a business answer. A verification actor that runs to
// completion and reports `changes_required` has TechStatus succeeded and
// domain outcome `changes_required`; using `failed` to mean "the change needs
// work" is the confusion §3.4 exists to forbid.
type TechStatus string

// The technical statuses PRD §3.4 lists.
const (
	StatusSucceeded        TechStatus = "succeeded"
	StatusFailed           TechStatus = "failed"
	StatusTimedOut         TechStatus = "timed_out"
	StatusCancelled        TechStatus = "cancelled"
	StatusPolicyDenied     TechStatus = "policy_denied"
	StatusContractRejected TechStatus = "contract_rejected"
)

// Valid reports whether s is one of the statuses §3.4 lists.
func (s TechStatus) Valid() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusTimedOut,
		StatusCancelled, StatusPolicyDenied, StatusContractRejected:
		return true
	default:
		return false
	}
}

// retryable reports whether another attempt could plausibly answer
// differently. A cancelled attempt is not retried — cancellation is an
// instruction, not a fault — and a policy denial is not retried, because the
// policy would deny the next attempt for the same reason.
func (s TechStatus) retryable() bool {
	switch s {
	case StatusFailed, StatusTimedOut, StatusContractRejected:
		return true
	default:
		return false
	}
}

// Attempt is one dispatch attempt against a node run (PRD §3.1). Number is
// per node run and starts at 1; the fencing token is recorded so a completed
// attempt names the lease it committed under.
type Attempt struct {
	ID           string
	NamespaceID  string
	NodeRunID    string
	Number       int
	ActorID      string
	Status       TechStatus
	FencingToken int64
	Result       json.RawMessage
	StartedAt    time.Time
	CompletedAt  time.Time
	// Usage is the §13.2 telemetry block this attempt reported, nil when it
	// reported none. See CompletionRequest.Usage for why it is never
	// fabricated.
	Usage *Usage
	// TerminationReason is how the turn ended as the provider reported it,
	// nil when nothing reported one. It is a sibling of Usage rather than a
	// field of it because it can be known when no usage block exists at all
	// (docs/adr/0009-usage-telemetry-extension.md) — the attempts column is
	// `termination_reason`, not `usage_termination_reason`, for the same
	// reason.
	TerminationReason *string
	// ContinuationRef is the §13.2 handle the actor offered for continuing
	// the conversation this attempt had, nil when it offered none
	// (docs/adr/0010-continuation-ref-on-request.md). It is opaque: the
	// engine stores it and hands it back to the same actor on a later
	// dispatch, and never parses or derives from it.
	//
	// It is NOT Usage.ThreadID. The thread id is telemetry about where the
	// turn's usage accrued; this is the handle a resume takes. A backend
	// can honestly report either without the other, and neither is derived
	// from the other (ADR 0009 §1).
	ContinuationRef *string
}

// Usage is the §13.2 telemetry block as the engine persists it. It mirrors
// internal/actors.Usage field-for-field but is declared independently here:
// the engine cannot import internal/actors, because actors already imports
// engine (for the Completer interface and CompletionRequest that its
// callback commit path uses), and the reverse import would cycle. Cost and
// Currency stay pointers for the reason actors.Usage's do: an actor that
// does not price its work says so with null, not with a zero that reads as
// "free". The four extended fields (ADR 0009) are pointers for that same
// reason and are each independently absent-able within a reported block:
// none of them may stand in for "this attempt reported usage", which is
// still what a non-nil Usage (persisted as a non-NULL usage_input_tokens)
// means.
type Usage struct {
	InputTokens       int64
	OutputTokens      int64
	Cost              *float64
	Currency          *string
	CachedInputTokens *int64
	ReasoningTokens   *int64
	// Model is the model that produced these counts; ThreadID is the
	// provider-side thread they accrued on. ThreadID is telemetry, not the
	// resume handle a later dispatch passes back to a bridge.
	Model    *string
	ThreadID *string
}

// HumanTaskStatusPending is the status a human task is created with
// (human_tasks.status).
const HumanTaskStatusPending = "pending"

// HumanTaskStatusDecided is the status DecideHumanTask moves a task to. It
// is terminal: MarkHumanTaskDecided's WHERE clause only ever flips
// pending -> decided, so a task cannot be decided twice.
const HumanTaskStatusDecided = "decided"

// HumanTask is one human_tasks row (PRD §9.9,
// migrations/0002_runtime_execution.sql): the durable record an approval
// node's dispatch writes instead of an attempt. Request carries everything
// PRD §9.9 says a human task must contain that is not already one of this
// row's own columns — see humanTaskRequest in humantask.go for its shape.
//
// AssignedOwnerID is left empty by the engine on purpose. ApproverRef (the
// requested role or group, e.g. "team/platform-ai-approvers") is a
// free-form reference the workflow author wrote, not a foreign key the
// compiler resolves against the owners table (PRD §9.4's ownerRef pattern,
// which approverRef reuses) — turning a role into one concrete assignable
// owner is an assignment policy this task does not implement. Request
// carries ApproverRef for whoever builds that surface to resolve.
type HumanTask struct {
	ID              string
	NamespaceID     string
	RunID           string
	NodeRunID       string
	Kind            string
	AssignedOwnerID string
	Status          string
	Request         json.RawMessage
	Response        json.RawMessage
	CreatedAt       time.Time
	ResolvedAt      time.Time
}

// CompletionRequest is one worker's report that a dispatched attempt is over
// (PRD §12.5). The first four fields are the fencing tuple §12.4 requires
// every completion to match; the rest is what the attempt produced.
type CompletionRequest struct {
	// WorkID, WorkerID, FencingToken, and Attempt are the claim the worker
	// holds, exactly as ClaimWork handed it out. A completion whose tuple no
	// longer matches the work item's current lease commits nothing and
	// returns ErrStaleClaim.
	WorkID       string
	WorkerID     string
	FencingToken int64
	Attempt      int

	// TechStatus is how the dispatch went. Required.
	TechStatus TechStatus

	// Outcome is the domain outcome the actor produced — a port declared by
	// the node's contract, e.g. `passed` or `changes_required`. It is
	// required when TechStatus is succeeded and ignored otherwise: a dispatch
	// that did not succeed has no domain answer to route.
	Outcome string

	// Output is the payload for that outcome. It is validated against the
	// outcome's schema; a violation is recorded as the *technical* status
	// contract_rejected, never as a domain outcome.
	Output json.RawMessage

	// LedgerDelta is what the actor proposes to write to the work ledger. The
	// engine stamps run, node run, and attempt on each record, checks the
	// node's declared propose/observe permissions and the PRD §10.4 producer
	// matrix, and only then appends. Agent-origin records may only be
	// proposed: an agent saying "done" is a completion claim, not evidence.
	LedgerDelta []ledger.Record

	// RunnerManifest declares what a trusted runner directly measured. It is
	// required for, and only consulted for, runner-origin observed evidence.
	RunnerManifest *ledger.RunnerManifest

	// ActorID is the actor that performed the attempt, recorded on the
	// attempt row. Optional: identity is not execution, and a worker
	// completing on behalf of an unregistered actor still gets its attempt
	// recorded.
	ActorID string

	// Usage is the §13.2 telemetry block the actor reported, nil when it
	// reported none. It rides straight onto the recorded attempt row
	// (recordAttempt) with no derivation and no fabricated zero — the same
	// null Cost/Currency semantics a Usage block arrived with are preserved
	// into storage.
	Usage *Usage

	// TerminationReason is how the actor's turn ended, as the actor
	// reported it, nil when it reported nothing. Like Usage it rides
	// straight onto the attempt row with no derivation, and it is carried
	// separately from Usage because an actor can know the reason without
	// holding a usage block to attach it to (ADR 0009).
	TerminationReason *string

	// ContinuationRef is the §13.2 handle the actor offered for continuing
	// its conversation, nil when it offered none. Like Usage it rides
	// straight onto the attempt row with no derivation: a handle nobody
	// offered is never invented, and NULL there means "not reported", never
	// "the session ended" (ADR 0010).
	ContinuationRef *string
}

// RejectionKind names which contract refused a completion.
type RejectionKind string

// The rejection kinds. Each is a *technical* refusal — the engine could not
// accept what the attempt reported — and each is recorded as TechStatus
// contract_rejected, never as a domain outcome.
const (
	// RejectionOutcome is an outcome the node does not declare.
	RejectionOutcome RejectionKind = "outcome"
	// RejectionOutput is output that violates the outcome's schema.
	RejectionOutput RejectionKind = "output"
	// RejectionLedger is a ledger delta that exceeds the node's declared
	// permissions, its record budget, or the §10.4 producer matrix.
	RejectionLedger RejectionKind = "ledger"
)

// Rejection is why a completion was refused by a contract. It is carried in
// the result rather than returned as an error, because the transaction that
// records it *succeeded*: a rejection is committed state, not a failure of
// the engine.
type Rejection struct {
	Kind    RejectionKind
	Outcome string
	Detail  string
}

// BoundKind names which of PRD §9.7's loop bounds a run hit.
type BoundKind string

// The loop bounds. All three are enforced by the engine before it creates the
// next node run, which is what makes "no loop may rely solely on an agent
// deciding when to stop" true of the runtime and not merely of the authoring
// guidance.
const (
	BoundTransitions BoundKind = "max_transitions"
	BoundVisits      BoundKind = "max_visits_per_node"
	BoundDuration    BoundKind = "max_duration"
)

// BoundExceeded is a loop bound the engine refused to cross.
type BoundExceeded struct {
	Kind   BoundKind
	NodeID string
	Limit  string
	Actual string
}

// CompletionResult is what one committed CompleteAttempt did. Every field
// describes state that is now durable: if CompleteAttempt returned no error,
// all of this committed together.
type CompletionResult struct {
	RunID         string
	NodeRunID     string
	NodeID        string
	AttemptID     string
	AttemptNumber int

	// TechStatus is the status as *recorded*, which is not always the status
	// reported: a succeeded dispatch whose output violated its contract is
	// recorded as contract_rejected.
	TechStatus TechStatus
	// Outcome is the domain outcome that was routed, empty when none was.
	Outcome string
	// Rejection is non-nil when a contract refused the completion.
	Rejection *Rejection

	// LedgerRecords are the records appended, as stored.
	LedgerRecords []ledger.Record

	// Retried reports that another attempt was enqueued for the same node
	// run; NextNodeID and NextNodeRunID are then empty.
	Retried bool
	// RetryAvailableAt is when the re-enqueued work becomes claimable.
	RetryAvailableAt time.Time

	// NextNodeID and NextNodeRunID name the node run this completion created,
	// empty when the run ended or retried.
	NextNodeID    string
	NextNodeRunID string
	// NextHumanTaskID is set instead of a claimable work item when
	// NextNodeID's kind is approval (PRD §9.9): the node run named by
	// NextNodeRunID is parked in NodeRunWaitingHuman and this is the
	// human_tasks row that pause is waiting on, not an attempt in progress.
	NextHumanTaskID string
	// EdgeFrom is the "<node>.<outcome>" the followed edge originated from.
	EdgeFrom string

	// Bound is non-nil when a loop bound stopped the run.
	Bound *BoundExceeded
	// Diagnostic explains a run failure the caller did not cause, e.g. a
	// declared outcome with no eligible edge.
	Diagnostic string

	// NodeRunState and RunState are the states as committed.
	NodeRunState NodeRunState
	RunState     RunState
	// RunOutput is the workflow result, set only when the run completed.
	RunOutput json.RawMessage

	// Transitions is the run's transition count after this completion, and
	// EventTypes lists the audit events appended, in order.
	Transitions int
	EventTypes  []string
}
