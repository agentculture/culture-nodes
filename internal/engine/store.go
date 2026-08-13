package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// The engine declares the persistence surface it needs and lets the store
// implement it, the same way internal/ledger declares ledger.Store. The point
// is not portability — PostgreSQL is authoritative (PRD §12.2) and there is
// no second implementation planned — it is that the §12.5 transaction is
// stated as one interface, so what "one database transaction" contains is
// readable in one place rather than inferred from a call graph.

// Store opens the engine's transactions and answers read-only questions
// outside them.
type Store interface {
	// NamespaceID is the namespace every row this store writes belongs to.
	NamespaceID() string

	// InTx runs fn inside a single transaction. When fn returns an error,
	// nothing fn wrote is applied — which is what makes a stale completion
	// leave no trace at all.
	InTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error

	// Run, NodeRun, and Attempts are read-only accessors for callers and
	// tests. They do not participate in a transaction.
	Run(ctx context.Context, runID string) (Run, error)
	NodeRun(ctx context.Context, nodeRunID string) (NodeRun, error)
	Attempts(ctx context.Context, nodeRunID string) ([]Attempt, error)
}

// WorkItemRef is the little a completion needs to know about the work item it
// is completing: which node run it belongs to, and whether it is still
// leased.
type WorkItemRef struct {
	ID        string
	NodeRunID string
	State     string
	Attempt   int
}

// EventInput is one audit event, written to the event log and to the outbox
// inside the same transaction that made the state change it describes.
//
// Every engine event uses the *run* as its aggregate. Sequence is monotonic
// per aggregate, so one aggregate per run means a run's audit trail is a
// single strictly increasing sequence — the property a consumer needs to know
// it has not missed a transition. Node run, attempt, and token identifiers
// travel in Data.
type EventInput struct {
	Type string
	Data json.RawMessage
}

// WorkflowVersionInput publishes (or re-resolves) the immutable workflow
// version a run pins.
type WorkflowVersionInput struct {
	WorkflowKey   string
	SourceFormat  string
	Source        string
	NormalizedIR  json.RawMessage
	ContentDigest string
}

// Tx is the §12.5 transaction boundary. Every method on it participates in
// one database transaction, and the transaction commits only when the
// function passed to InTx returns nil.
type Tx interface {
	// Lock takes a transaction-scoped advisory lock. The engine takes
	// ledger.RunLockKey(runID) before it touches a run, which is the same key
	// the ledger runtime takes — so a completion and a concurrent ledger
	// append or review of the same run queue behind each other instead of
	// interleaving.
	Lock(ctx context.Context, key string) error

	// EnsureWorkflowVersion returns the immutable version row for a content
	// digest, publishing it if this is the first time it has been seen. The
	// digest is unique per namespace, so the same definition always resolves
	// to the same version.
	EnsureWorkflowVersion(ctx context.Context, in WorkflowVersionInput) (versionID string, err error)
	// WorkflowIR returns the pinned definition: its digest and normalized IR.
	WorkflowIR(ctx context.Context, versionID string) (digest string, ir json.RawMessage, err error)

	// CompleteWork is the fenced completion guard (PRD §12.4): it must match
	// work ID, expected state, lease owner, fencing token, and attempt. Zero
	// rows matched returns ErrStaleClaim and nothing else in the transaction
	// may proceed.
	CompleteWork(ctx context.Context, workID, workerID string, fencingToken int64, attempt int) error
	// WorkItem resolves a work ID to the node run it belongs to. The engine
	// never trusts a caller-supplied node run: the work item is the binding.
	WorkItem(ctx context.Context, workID string) (WorkItemRef, error)
	// EnqueueWork inserts a ready work item for a node run, claimable from
	// availableAt (zero meaning now).
	EnqueueWork(ctx context.Context, nodeRunID string, availableAt time.Time) (workID string, err error)

	InsertRun(ctx context.Context, run Run) error
	UpdateRunState(ctx context.Context, runID string, state RunState, output json.RawMessage) error
	Run(ctx context.Context, runID string) (Run, error)

	InsertToken(ctx context.Context, token Token) error
	ConsumeToken(ctx context.Context, tokenID string) error

	InsertNodeRun(ctx context.Context, nodeRun NodeRun) error
	UpdateNodeRun(ctx context.Context, nodeRunID string, state NodeRunState, outcome string) error
	NodeRun(ctx context.Context, nodeRunID string) (NodeRun, error)

	// InsertHumanTask records an approval node's human task (PRD §9.9). It is
	// what an approval-kind dispatch writes *instead of* EnqueueWork — see
	// humantask.go's dispatchNode — so the node run it belongs to never gets
	// a work_items row: nothing to lease, nothing to hold open while the run
	// waits on a human.
	InsertHumanTask(ctx context.Context, task HumanTask) (id string, err error)
	// GetHumanTask returns one human_tasks row, or ErrNotFound.
	GetHumanTask(ctx context.Context, id string) (HumanTask, error)
	// MarkHumanTaskDecided flips a human task from pending to decided,
	// recording its response and resolved_at, and reports whether this call
	// was the one that did so. The status is part of the WHERE clause (the
	// same pattern ledger.MarkReviewCommitted uses), so two concurrent
	// decisions on the same task cannot both win: a false return means a
	// decision already applied, and DecideHumanTask refuses rather than
	// resuming the run a second time.
	MarkHumanTaskDecided(ctx context.Context, id string, response json.RawMessage, resolvedAt time.Time) (bool, error)

	InsertAttempt(ctx context.Context, attempt Attempt) error
	// NextAttemptNumber is one past the highest attempt number recorded for a
	// node run. Attempt numbering is derived rather than carried by the
	// worker, so two workers cannot disagree about which attempt this is.
	NextAttemptNumber(ctx context.Context, nodeRunID string) (int, error)

	// TransitionCount is how many transitions the run has taken, and
	// NodeVisits how many node runs each node has. Both are derived from the
	// node_runs rows rather than kept in a counter column: in the sequential
	// MVP every transition creates exactly one node run, so the rows already
	// hold the answer, and a derived count cannot drift from the state it
	// describes across a crash.
	TransitionCount(ctx context.Context, runID string) (int, error)
	NodeVisits(ctx context.Context, runID string) (map[string]int, error)

	// NodeOutput returns the output of the most recent succeeded attempt of a
	// node within a run — what a `/nodes/<id>/output` binding resolves to.
	NodeOutput(ctx context.Context, runID, nodeID string) (json.RawMessage, error)

	// NodeEvidence returns the run's live evidence ledger records belonging
	// to a node's node runs, in id order — what a `/nodes/<id>/evidence`
	// binding resolves to. Evidence identity is the node run (the engine
	// stamps node_run_id on every accepted delta record); zero records is an
	// empty slice, not an error.
	NodeEvidence(ctx context.Context, runID, nodeID string) ([]ledger.Record, error)

	// AppendEvent writes one audit event for a run and its outbox row, in
	// this transaction (PRD §12.5 steps 7 and 10). It returns the sequence
	// assigned.
	AppendEvent(ctx context.Context, runID string, in EventInput) (int64, error)

	// Ledger is the work-ledger runtime bound to this transaction: records it
	// appends commit or roll back with everything else here.
	Ledger() *ledger.Ledger
}
