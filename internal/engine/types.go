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
// GroupID names the token group (split fan-out set) this token belongs to,
// empty for the entry token and for every token outside any split. An
// ordinary transition copies the consumed token's group to the next token; a
// split stamps each fanned token with the fresh group; the post-join token
// takes the fired group's parent — the join closes the group and re-enters
// the enclosing one (parallel-tokens design §3.3).
type Token struct {
	ID            string
	NamespaceID   string
	RunID         string
	NodeID        string
	State         TokenState
	ParentTokenID string
	GroupID       string
	// OriginEventID names the signal_events fact that created this token, set
	// only on an event pickup (issue #43, design D9) and empty everywhere
	// else.
	//
	// It is the answer to review finding D4. A pickup token genuinely has no
	// PARENT TOKEN: nothing in this run handed it control, and the emitter may
	// be another run or an external system entirely, so stamping some
	// emitter's token as its parent would make the ancestry tree assert a
	// causal edge that does not exist. Rather than either lie about a parent
	// or leave an unexplained orphan, the token names the FACT it came from,
	// and the run-detail surface renders it as an explained root.
	OriginEventID string
	CreatedAt     time.Time
	ConsumedAt    time.Time
}

// TokenGroup records one split: the fan-out set of sibling tokens created by
// a single parallel-node completion. Cardinality is fixed at creation — the
// join barrier counts arrivals against it (design D4: the sibling set is
// discovered at split time, never declared at the join, because guarded
// split edges make a declared count wrong by construction).
type TokenGroup struct {
	ID             string
	NamespaceID    string
	RunID          string
	SplitNodeRunID string // the parallel node run whose completion fanned out
	ParentGroupID  string // enclosing group; "" at top level (nested splits)
	Cardinality    int    // how many tokens the eligible edge set produced
	CreatedAt      time.Time
}

// JoinArrival is one branch reaching a barrier (design §4.1). Rows are
// append-only; counting them under the run's advisory lock is the barrier.
type JoinArrival struct {
	ID            string
	NamespaceID   string
	RunID         string
	JoinNodeRunID string
	GroupID       string
	TokenID       string          // the consumed branch token
	FromNode      string          // the branch node that completed into the join
	Outcome       string          // the domain outcome that routed here
	Output        json.RawMessage // that outcome's output payload
	ArrivedAt     time.Time
}

// Join policies a join node's barrier fires under (PRD §9.8; the schema's
// #/$defs/joinConfig). `first_success` is a recorded PRD deviation — domain
// outcomes carry no success typing, so it has no honest meaning yet.
const (
	JoinPolicyAll    = "all"
	JoinPolicyAny    = "any"
	JoinPolicyQuorum = "quorum"
)

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
	// NodeRunWaitingJoin is a join node run parked as a barrier (issue #43,
	// parallel-tokens design D3): created by the first sibling arrival,
	// waiting for the rest of the token group. It parallels waiting_human's
	// shape exactly — parked, not terminal, nothing to lease — and there is
	// deliberately no work item, because there is no claimable work until
	// the barrier satisfies. Unlike every other parked state, what wakes it
	// is a COMPLETION path, not a scheduler tick or an event delivery: the
	// sibling arrival that reaches the threshold flips it to ready and
	// enqueues the join's work inside its own §12.5 transaction.
	NodeRunWaitingJoin NodeRunState = "waiting_join"
	NodeRunCompleted   NodeRunState = "completed"
	NodeRunFailed      NodeRunState = "failed"
	NodeRunCancelled   NodeRunState = "cancelled"
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
	// Preserve is task t25/t26's bridge-reported preserve-on-failure branch
	// (issue #49), nil on every attempt that reports none — every
	// successful attempt, and a failed one whose bridge had nothing to
	// commit or ran with preserve-on-failure disabled. See the Preserve
	// type's own doc comment for what NULL on the row means.
	Preserve *Preserve
}

// Preserve is one attempt's bridge-reported preserve-on-failure outcome
// (task t25/t26, issue #49; migrations/0025_attempt_preserve_branch.sql).
// It is deliberately a small, engine-native mirror of
// internal/actors.Preserve rather than that package's own type — the
// engine cannot import internal/actors (actors already imports engine) —
// and it carries only the facts task t26 actually persists: a
// minted-and-committed branch name, whether it reached the configured
// remote, and which remote that was.
//
// It is never constructed for a branch that was only minted, never
// committed (internal/actors.Preserve.ToEngine gates on Committed) — a
// name with no git ref behind it names nothing that exists in any
// repository, and persisting one would show an operator a link to nowhere.
//
// It is a bridge's own claim about what IT did on ITS own host, not
// observed evidence (PRD §10.4): nothing on this path writes an
// `observed`-authority ledger record from it.
type Preserve struct {
	// Branch is the branch name the bridge's plumbing commit actually
	// created a local ref for.
	Branch string
	// Pushed is true when that branch reached Remote, false when the
	// commit exists only in the bridge host's local object database — the
	// expected common case today, since bridge-host push credentials are
	// unverified (the plan's risk register). A reader must be able to tell
	// the two apart: Pushed is exactly that distinction, never inferred.
	Pushed bool
	// Remote is the remote name the bridge attempted (or reached) the push
	// against, e.g. "origin" — informational only. It is NEVER combined
	// with Branch to construct a forge URL on this path: a link may only
	// come from configuration the operator actually set (see web/README.md
	// on VITE_PRESERVE_BRANCH_URL_TEMPLATE), never a guess from this name.
	Remote string
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

	// RefusalOutcome is the name a dispatch the CONTROL PLANE refused routes
	// under — today only OutcomeBudgetExhausted (task t11, spec claim c6).
	//
	// It is a separate field from Outcome because it is a different kind of
	// statement made by a different producer. Outcome is the actor's answer,
	// and setting it requires TechStatus succeeded. This is the control
	// plane's own answer, produced when nothing was dispatched at all: the
	// attempt's technical status is the honest one for "a declared policy
	// denied this" (policy_denied), and this names the edge the workflow
	// author declared for that refusal. Reusing Outcome would have meant
	// either claiming a dispatch succeeded when none happened, or quietly
	// giving a field that is "ignored otherwise" a second meaning on failure
	// paths that already set nothing.
	//
	// It may only accompany a non-succeeded status, and it only routes when
	// the workflow declares an edge from it; otherwise the run fails with a
	// diagnostic naming it, exactly as an unrouted technical status does.
	RefusalOutcome string

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

	// Preserve is task t25/t26's bridge-reported preserve-on-failure branch
	// (issue #49), nil when the actor reported none. Like Usage it rides
	// straight onto the attempt row with no derivation.
	Preserve *Preserve
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
	// BoundParallelTokens is the §9.8/§9.7 cap on concurrently active tokens,
	// enforced at split fan-out: a split whose eligible set would push the
	// run past limits.maxParallelTokens is refused WHOLE — never a partial
	// fan-out — as a bound failure (parallel-tokens design D8).
	BoundParallelTokens BoundKind = "max_parallel_tokens"
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

	// Split is set when this completion fanned a parallel node out (issue
	// #43): NextNodeID/NextNodeRunID stay empty, because a split has no
	// single "next" — every created branch is listed here instead.
	Split *SplitResult
	// JoinNodeRunID is set when this completion routed into a join node: the
	// barrier node run the arrival was recorded against (created by this
	// arrival when it was the first). JoinSatisfied reports whether this
	// arrival was the one that fired the barrier and enqueued the join's
	// work. ReapedBranchNodeRuns lists the sibling node runs this completion
	// cancelled — losers of an any/quorum barrier, or every remaining branch
	// when a terminal branch failure failed the run (design D6/§4.4);
	// propagating those cancellations to async actors mid-invocation is the
	// caller's best-effort, post-commit job, mirroring cancelpropagate.
	JoinNodeRunID        string
	JoinSatisfied        bool
	ReapedBranchNodeRuns []string

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

// SplitResult is what one committed split fan-out created (design §3.3): the
// token group and every branch, in normalized edge order.
type SplitResult struct {
	GroupID     string
	Cardinality int
	Branches    []SplitBranch
}

// SplitBranch is one fanned-out branch of a split.
type SplitBranch struct {
	NodeID    string
	NodeRunID string
	TokenID   string
	// WorkID is empty when the branch did not enqueue claimable work — an
	// approval node's human-task park, or a branch edge that routed straight
	// into a join barrier.
	WorkID string
}
