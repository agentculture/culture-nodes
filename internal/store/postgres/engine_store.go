package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
)

// EngineStore is the PostgreSQL implementation of engine.Store: the durable
// backing for runs, tokens, node runs, and attempts, and the transaction
// boundary the PRD §12.5 completion commits inside.
//
// Two design notes worth stating here rather than leaving to be inferred:
//
//   - Transition and visit counts are *derived* from node_runs rather than
//     kept in counter columns. In the sequential MVP every transition creates
//     exactly one node run, so the rows already carry the answer, and a
//     derived count cannot drift from the state it describes across a crash
//     the way a separately-incremented counter can. It also means the §9.7
//     bounds need no schema of their own.
//   - The ledger runtime handed to a transaction is bound to *that*
//     transaction. ledger.Ledger writes through a ledger.Store, and the one
//     built here shares the engine's pgx.Tx, so an accepted ledger record and
//     the node run it belongs to commit together or not at all — which is
//     what §12.5 step 4 means by appending records inside the transaction.
type EngineStore struct {
	engineQueries
	pool      *pgxpool.Pool
	validator *contracts.Validator
}

// NewEngineStore returns an engine store over s, scoped to namespaceID.
//
// It compiles the embedded schemas once, so every transaction's ledger
// runtime reuses one validator instead of recompiling the schema set per
// completion.
func NewEngineStore(s *Store, namespaceID string) (*EngineStore, error) {
	if s == nil {
		return nil, errors.New("postgres: NewEngineStore requires a store")
	}
	if namespaceID == "" {
		return nil, errors.New("postgres: NewEngineStore requires a namespace id")
	}
	validator, err := contracts.NewValidator()
	if err != nil {
		return nil, fmt.Errorf("postgres: NewEngineStore: compile schemas: %w", err)
	}
	return &EngineStore{
		engineQueries: engineQueries{q: s.pool, namespaceID: namespaceID},
		pool:          s.pool,
		validator:     validator,
	}, nil
}

// NewEngine returns an engine backed by s and scoped to namespaceID. It is
// the one-line path callers want; the store is an implementation detail of
// the engine, not something most callers hold separately.
func NewEngine(s *Store, namespaceID string, opts ...engine.Option) (*engine.Engine, error) {
	es, err := NewEngineStore(s, namespaceID)
	if err != nil {
		return nil, err
	}
	opts = append([]engine.Option{engine.WithValidator(es.validator)}, opts...)
	return engine.New(es, opts...)
}

// InTx runs fn inside one transaction: the §12.5 boundary. Everything fn
// wrote is applied together, or — if fn returns an error — none of it is, so
// a completion refused by the fencing guard leaves no trace at all.
func (es *EngineStore) InTx(ctx context.Context, fn func(context.Context, engine.Tx) error) error {
	tx, err := es.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: engine: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	ledgerStore := &LedgerStore{pool: es.pool, q: tx, namespaceID: es.namespaceID, inTx: true}
	runtime, err := ledger.New(ledgerStore, ledger.WithValidator(es.validator))
	if err != nil {
		return fmt.Errorf("postgres: engine: build ledger runtime: %w", err)
	}

	inner := &engineTx{
		engineQueries: engineQueries{q: tx, namespaceID: es.namespaceID},
		ledger:        runtime,
	}
	if err := fn(ctx, inner); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: engine: commit transaction: %w", err)
	}
	return nil
}

// engineTx is one transaction's view of the engine's tables.
type engineTx struct {
	engineQueries
	ledger *ledger.Ledger
}

// Lock takes a transaction-scoped advisory lock, using the same hashing as
// the ledger store so that engine.Tx.Lock(ledger.RunLockKey(runID)) and
// ledger.Ledger's own lock on the same run are genuinely the same lock.
func (tx *engineTx) Lock(ctx context.Context, key string) error {
	if _, err := tx.q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return fmt.Errorf("postgres: engine: lock %s: %w", key, err)
	}
	return nil
}

// Ledger returns the work-ledger runtime bound to this transaction.
func (tx *engineTx) Ledger() *ledger.Ledger { return tx.ledger }

// engineQueries holds every statement that reads or writes engine state. It
// is shared by the store (running against the pool) and by a transaction
// (running against the pgx.Tx), so a query is written once and behaves
// identically inside and outside a transaction.
type engineQueries struct {
	q           ledgerQuerier
	namespaceID string
}

// NamespaceID reports the namespace this store writes to.
func (eq engineQueries) NamespaceID() string { return eq.namespaceID }

const selectWorkflowVersionByDigestSQL = `
SELECT id FROM workflow_versions WHERE namespace_id = $1 AND content_digest = $2
`

const insertWorkflowVersionSQL = `
INSERT INTO workflow_versions (
	id, namespace_id, workflow_key, version, source_format, source, normalized_ir, content_digest
)
VALUES (
	$1, $2, $3,
	(SELECT COALESCE(MAX(version), 0) + 1 FROM workflow_versions WHERE namespace_id = $2 AND workflow_key = $3),
	$4, $5, $6, $7
)
RETURNING id
`

// EnsureWorkflowVersion resolves a definition to its immutable version row,
// publishing it the first time that digest is seen.
//
// It takes an advisory lock keyed on the workflow before it looks: without
// one, two concurrent CreateRun calls for a not-yet-published definition
// would both find nothing and both insert, and the loser's whole transaction
// would abort on the unique index rather than simply reusing the winner's
// row. The lock is per workflow key, so unrelated workflows never contend.
func (eq engineQueries) EnsureWorkflowVersion(ctx context.Context, in engine.WorkflowVersionInput) (string, error) {
	switch {
	case in.WorkflowKey == "":
		return "", errors.New("postgres: engine: EnsureWorkflowVersion requires a workflow key")
	case in.ContentDigest == "":
		return "", errors.New("postgres: engine: EnsureWorkflowVersion requires a content digest")
	}

	lockKey := "workflow:" + eq.namespaceID + ":" + in.WorkflowKey
	if _, err := eq.q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return "", fmt.Errorf("postgres: engine: EnsureWorkflowVersion: lock: %w", err)
	}

	var id string
	err := eq.q.QueryRow(ctx, selectWorkflowVersionByDigestSQL, eq.namespaceID, in.ContentDigest).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !isNoRows(err) {
		return "", fmt.Errorf("postgres: engine: EnsureWorkflowVersion: %w", err)
	}

	err = eq.q.QueryRow(ctx, insertWorkflowVersionSQL,
		store.NewULID(), eq.namespaceID, in.WorkflowKey,
		in.SourceFormat, in.Source, jsonOrEmptyObject(in.NormalizedIR), in.ContentDigest,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("postgres: engine: EnsureWorkflowVersion: %w", err)
	}
	return id, nil
}

// WorkflowIR returns the pinned definition a run executes.
func (eq engineQueries) WorkflowIR(ctx context.Context, versionID string) (string, json.RawMessage, error) {
	var (
		digest string
		ir     []byte
	)
	err := eq.q.QueryRow(ctx,
		`SELECT content_digest, normalized_ir FROM workflow_versions WHERE id = $1`, versionID,
	).Scan(&digest, &ir)
	if err != nil {
		if isNoRows(err) {
			return "", nil, fmt.Errorf("postgres: engine: workflow version %s: %w", versionID, engine.ErrNotFound)
		}
		return "", nil, fmt.Errorf("postgres: engine: WorkflowIR: %w", err)
	}
	return digest, ir, nil
}

// CompleteWork is the fenced completion guard, running the same guarded
// UPDATE Store.CompleteWork runs — the same SQL constant, so the invariant
// cannot drift between the two paths — but inside the engine's transaction,
// where a zero-row result must roll back everything else too.
//
// The returned error wraps both engine.ErrStaleClaim and this package's
// ErrStaleClaim, so callers on either side of the interface can match the
// sentinel they know.
func (eq engineQueries) CompleteWork(ctx context.Context, workID, workerID string, fencingToken int64, attempt int) error {
	switch {
	case workID == "":
		return errors.New("postgres: engine: CompleteWork requires a work id")
	case workerID == "":
		return errors.New("postgres: engine: CompleteWork requires a worker id")
	}

	tag, err := eq.q.Exec(ctx, completeWorkSQL, workID, workerID, fencingToken, int32(attempt))
	if err != nil {
		return fmt.Errorf("postgres: engine: CompleteWork: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: engine: work item %s: %w: %w", workID, engine.ErrStaleClaim, ErrStaleClaim)
	}
	return nil
}

// WorkItem resolves a work id to the node run it belongs to.
func (eq engineQueries) WorkItem(ctx context.Context, workID string) (engine.WorkItemRef, error) {
	var (
		ref     engine.WorkItemRef
		attempt int32
	)
	// Scoped by namespace as well as primary key: completion-critical
	// lookups must never resolve another namespace's row even when handed a
	// cross-namespace id (the engine is constructed for one namespace).
	err := eq.q.QueryRow(ctx,
		`SELECT id, node_run_id, state, attempt FROM work_items WHERE id = $1 AND namespace_id = $2`, workID, eq.namespaceID,
	).Scan(&ref.ID, &ref.NodeRunID, &ref.State, &attempt)
	if err != nil {
		if isNoRows(err) {
			return engine.WorkItemRef{}, fmt.Errorf("postgres: engine: work item %s: %w", workID, engine.ErrNotFound)
		}
		return engine.WorkItemRef{}, fmt.Errorf("postgres: engine: WorkItem: %w", err)
	}
	ref.Attempt = int(attempt)
	return ref, nil
}

// EnqueueWork inserts a ready work item for a node run.
func (eq engineQueries) EnqueueWork(ctx context.Context, nodeRunID string, availableAt time.Time) (string, error) {
	if nodeRunID == "" {
		return "", errors.New("postgres: engine: EnqueueWork requires a node run id")
	}
	workID := store.NewULID()
	if _, err := eq.q.Exec(ctx, enqueueWorkSQL, workID, eq.namespaceID, nodeRunID, tsOrNow(availableAt)); err != nil {
		return "", fmt.Errorf("postgres: engine: EnqueueWork: %w", err)
	}
	return workID, nil
}

const insertRunSQL = `
INSERT INTO runs (id, namespace_id, workflow_version_id, status, input, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $6)
`

// InsertRun records a new run.
func (eq engineQueries) InsertRun(ctx context.Context, run engine.Run) error {
	_, err := eq.q.Exec(ctx, insertRunSQL,
		run.ID, eq.namespaceID, run.WorkflowVersionID, string(run.State),
		jsonOrEmptyObject(run.Input), tsOrNow(run.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("postgres: engine: InsertRun: %w", err)
	}
	return nil
}

// updateRunStateSQL stamps completed_at only on a terminal state, and leaves
// output untouched when the caller passes none: a failing run should not
// erase a result an earlier statement recorded.
const updateRunStateSQL = `
UPDATE runs
SET status       = $2,
    output       = COALESCE($3, output),
    updated_at   = now(),
    completed_at = CASE WHEN $2 IN ('completed', 'failed', 'cancelled') THEN now() ELSE completed_at END
WHERE id = $1
`

// UpdateRunState moves a run to a new state, optionally recording its output.
func (eq engineQueries) UpdateRunState(ctx context.Context, runID string, state engine.RunState, output json.RawMessage) error {
	var payload any
	if len(output) > 0 {
		payload = []byte(output)
	}
	if _, err := eq.q.Exec(ctx, updateRunStateSQL, runID, string(state), payload); err != nil {
		return fmt.Errorf("postgres: engine: UpdateRunState: %w", err)
	}
	return nil
}

const selectRunSQL = `
SELECT r.id, r.namespace_id, r.workflow_version_id, wv.content_digest, r.status,
       r.input, r.output, r.created_at, r.updated_at, r.completed_at
FROM runs AS r
JOIN workflow_versions AS wv ON wv.id = r.workflow_version_id
WHERE r.id = $1 AND r.namespace_id = $2
`

// Run returns one run, including the content digest of the definition it
// pinned — a run that could not say which definition it is executing would
// not be pinned to anything.
func (eq engineQueries) Run(ctx context.Context, runID string) (engine.Run, error) {
	var (
		run         engine.Run
		status      string
		input       []byte
		output      []byte
		createdAt   pgtype.Timestamptz
		updatedAt   pgtype.Timestamptz
		completedAt pgtype.Timestamptz
	)
	err := eq.q.QueryRow(ctx, selectRunSQL, runID, eq.namespaceID).Scan(
		&run.ID, &run.NamespaceID, &run.WorkflowVersionID, &run.WorkflowDigest, &status,
		&input, &output, &createdAt, &updatedAt, &completedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return engine.Run{}, fmt.Errorf("postgres: engine: run %s: %w", runID, engine.ErrNotFound)
		}
		return engine.Run{}, fmt.Errorf("postgres: engine: Run: %w", err)
	}
	run.State = engine.RunState(status)
	run.Input = json.RawMessage(input)
	run.Output = json.RawMessage(output)
	run.CreatedAt = tsValue(createdAt)
	run.UpdatedAt = tsValue(updatedAt)
	run.CompletedAt = tsValue(completedAt)
	return run, nil
}

const insertTokenSQL = `
INSERT INTO tokens (id, namespace_id, run_id, node_key, state, parent_token_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
`

// InsertToken records a control token at a node.
func (eq engineQueries) InsertToken(ctx context.Context, token engine.Token) error {
	_, err := eq.q.Exec(ctx, insertTokenSQL,
		token.ID, eq.namespaceID, token.RunID, token.NodeID, string(token.State),
		textOrNull(token.ParentTokenID), tsOrNow(token.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("postgres: engine: InsertToken: %w", err)
	}
	return nil
}

// ConsumeToken retires a token. It is idempotent: consuming an already
// consumed token is a no-op, not an error, because a token is retired by the
// transition it authorized and a caller re-running that logic should not be
// told off for it.
func (eq engineQueries) ConsumeToken(ctx context.Context, tokenID string) error {
	if tokenID == "" {
		return nil
	}
	_, err := eq.q.Exec(ctx,
		`UPDATE tokens SET state = 'consumed', consumed_at = now() WHERE id = $1 AND state = 'active'`,
		tokenID,
	)
	if err != nil {
		return fmt.Errorf("postgres: engine: ConsumeToken: %w", err)
	}
	return nil
}

const insertNodeRunSQL = `
INSERT INTO node_runs (id, namespace_id, run_id, token_id, node_key, status, visit_count, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
`

// InsertNodeRun records a node's logical execution.
func (eq engineQueries) InsertNodeRun(ctx context.Context, nodeRun engine.NodeRun) error {
	visit := nodeRun.VisitCount
	if visit < 1 {
		visit = 1
	}
	_, err := eq.q.Exec(ctx, insertNodeRunSQL,
		nodeRun.ID, eq.namespaceID, nodeRun.RunID, textOrNull(nodeRun.TokenID),
		nodeRun.NodeID, string(nodeRun.State), int32(visit), tsOrNow(nodeRun.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("postgres: engine: InsertNodeRun: %w", err)
	}
	return nil
}

const updateNodeRunSQL = `
UPDATE node_runs
SET status       = $2,
    outcome      = COALESCE(NULLIF($3, ''), outcome),
    updated_at   = now(),
    completed_at = CASE WHEN $2 IN ('completed', 'failed', 'cancelled') THEN now() ELSE completed_at END
WHERE id = $1
`

// UpdateNodeRun moves a node run to a new state, recording the domain outcome
// that got it there when there is one.
func (eq engineQueries) UpdateNodeRun(ctx context.Context, nodeRunID string, state engine.NodeRunState, outcome string) error {
	if _, err := eq.q.Exec(ctx, updateNodeRunSQL, nodeRunID, string(state), outcome); err != nil {
		return fmt.Errorf("postgres: engine: UpdateNodeRun: %w", err)
	}
	return nil
}

const selectNodeRunSQL = `
SELECT id, namespace_id, run_id, token_id, node_key, status, outcome, visit_count,
       created_at, updated_at, completed_at
FROM node_runs
WHERE id = $1
`

// NodeRun returns one node run.
func (eq engineQueries) NodeRun(ctx context.Context, nodeRunID string) (engine.NodeRun, error) {
	var (
		nodeRun     engine.NodeRun
		tokenID     pgtype.Text
		status      string
		outcome     pgtype.Text
		visitCount  int32
		createdAt   pgtype.Timestamptz
		updatedAt   pgtype.Timestamptz
		completedAt pgtype.Timestamptz
	)
	err := eq.q.QueryRow(ctx, selectNodeRunSQL, nodeRunID).Scan(
		&nodeRun.ID, &nodeRun.NamespaceID, &nodeRun.RunID, &tokenID, &nodeRun.NodeID,
		&status, &outcome, &visitCount, &createdAt, &updatedAt, &completedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return engine.NodeRun{}, fmt.Errorf("postgres: engine: node run %s: %w", nodeRunID, engine.ErrNotFound)
		}
		return engine.NodeRun{}, fmt.Errorf("postgres: engine: NodeRun: %w", err)
	}
	nodeRun.TokenID = textOrEmpty(tokenID)
	nodeRun.State = engine.NodeRunState(status)
	nodeRun.Outcome = textOrEmpty(outcome)
	nodeRun.VisitCount = int(visitCount)
	nodeRun.CreatedAt = tsValue(createdAt)
	nodeRun.UpdatedAt = tsValue(updatedAt)
	nodeRun.CompletedAt = tsValue(completedAt)
	return nodeRun, nil
}

const insertAttemptSQL = `
INSERT INTO attempts (
	id, namespace_id, node_run_id, attempt_number, actor_id, status, fencing_token,
	result, started_at, completed_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
`

// InsertAttempt records one dispatch attempt's result. The
// attempts(node_run_id, attempt_number) unique constraint means a duplicate
// completion is a constraint violation rather than a second silent row.
func (eq engineQueries) InsertAttempt(ctx context.Context, attempt engine.Attempt) error {
	var result any
	if len(attempt.Result) > 0 {
		result = []byte(attempt.Result)
	}
	_, err := eq.q.Exec(ctx, insertAttemptSQL,
		attempt.ID, eq.namespaceID, attempt.NodeRunID, int32(attempt.Number),
		textOrNull(attempt.ActorID), string(attempt.Status), attempt.FencingToken,
		result, tsOrNow(attempt.StartedAt), tsOrNow(attempt.CompletedAt),
	)
	if err != nil {
		return fmt.Errorf("postgres: engine: InsertAttempt: %w", err)
	}
	return nil
}

// NextAttemptNumber is one past the highest attempt already recorded.
func (eq engineQueries) NextAttemptNumber(ctx context.Context, nodeRunID string) (int, error) {
	var next int32
	err := eq.q.QueryRow(ctx,
		`SELECT (COALESCE(MAX(attempt_number), 0) + 1)::int FROM attempts WHERE node_run_id = $1`,
		nodeRunID,
	).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("postgres: engine: NextAttemptNumber: %w", err)
	}
	return int(next), nil
}

// Attempts returns a node run's attempts in order.
func (eq engineQueries) Attempts(ctx context.Context, nodeRunID string) ([]engine.Attempt, error) {
	rows, err := eq.q.Query(ctx, `
		SELECT id, namespace_id, node_run_id, attempt_number, actor_id, status,
		       fencing_token, result, started_at, completed_at
		FROM attempts
		WHERE node_run_id = $1
		ORDER BY attempt_number
	`, nodeRunID)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: Attempts: %w", err)
	}
	defer rows.Close()

	var attempts []engine.Attempt
	for rows.Next() {
		var (
			attempt      engine.Attempt
			number       int32
			actorID      pgtype.Text
			status       string
			fencingToken pgtype.Int8
			result       []byte
			startedAt    pgtype.Timestamptz
			completedAt  pgtype.Timestamptz
		)
		if err := rows.Scan(
			&attempt.ID, &attempt.NamespaceID, &attempt.NodeRunID, &number, &actorID,
			&status, &fencingToken, &result, &startedAt, &completedAt,
		); err != nil {
			return nil, fmt.Errorf("postgres: engine: Attempts: scan: %w", err)
		}
		attempt.Number = int(number)
		attempt.ActorID = textOrEmpty(actorID)
		attempt.Status = engine.TechStatus(status)
		if fencingToken.Valid {
			attempt.FencingToken = fencingToken.Int64
		}
		attempt.Result = json.RawMessage(result)
		attempt.StartedAt = tsValue(startedAt)
		attempt.CompletedAt = tsValue(completedAt)
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: engine: Attempts: %w", err)
	}
	return attempts, nil
}

// TransitionCount is how many transitions the run has taken. Every transition
// creates exactly one node run and the entry node run is not a transition, so
// the count is the node-run count less one.
func (eq engineQueries) TransitionCount(ctx context.Context, runID string) (int, error) {
	var count int32
	err := eq.q.QueryRow(ctx,
		`SELECT GREATEST(COUNT(*) - 1, 0)::int FROM node_runs WHERE run_id = $1`, runID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("postgres: engine: TransitionCount: %w", err)
	}
	return int(count), nil
}

// NodeVisits is how many node runs each node has in this run.
func (eq engineQueries) NodeVisits(ctx context.Context, runID string) (map[string]int, error) {
	rows, err := eq.q.Query(ctx,
		`SELECT node_key, COUNT(*)::int FROM node_runs WHERE run_id = $1 GROUP BY node_key`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: engine: NodeVisits: %w", err)
	}
	defer rows.Close()

	visits := make(map[string]int)
	for rows.Next() {
		var (
			nodeKey string
			count   int32
		)
		if err := rows.Scan(&nodeKey, &count); err != nil {
			return nil, fmt.Errorf("postgres: engine: NodeVisits: scan: %w", err)
		}
		visits[nodeKey] = int(count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: engine: NodeVisits: %w", err)
	}
	return visits, nil
}

// NodeOutput returns the output of a node's most recent succeeded attempt in
// this run — what `/nodes/<id>/output` resolves to. Only a succeeded attempt
// counts: a failed or contract-rejected attempt produced no output a binding
// may treat as the node's answer.
func (eq engineQueries) NodeOutput(ctx context.Context, runID, nodeID string) (json.RawMessage, error) {
	var result []byte
	err := eq.q.QueryRow(ctx, `
		SELECT a.result
		FROM attempts AS a
		JOIN node_runs AS nr ON nr.id = a.node_run_id
		WHERE nr.run_id = $1 AND nr.node_key = $2 AND a.status = 'succeeded'
		ORDER BY a.completed_at DESC, a.id DESC
		LIMIT 1
	`, runID, nodeID).Scan(&result)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("postgres: engine: NodeOutput: %w", err)
	}
	return json.RawMessage(result), nil
}

const insertEngineEventSQL = `
INSERT INTO events (id, namespace_id, aggregate_type, aggregate_id, sequence, event_type, source, data, occurred_at)
VALUES ($1, $2, 'run', $3, $4, $5, 'nodes', $6, now())
`

const insertEngineOutboxSQL = `
INSERT INTO outbox (id, namespace_id, topic, payload, status, available_at)
VALUES ($1, $2, $3, $4, 'pending', now())
`

// AppendEvent writes one audit event for a run and its outbox row (PRD §12.5
// steps 7 and 10).
//
// The sequence is assigned by reading MAX(sequence) + 1 for the aggregate,
// which is only safe because the caller holds the run's advisory lock for the
// whole transaction — the engine takes ledger.RunLockKey(runID) before it
// writes anything, so two completions of the same run cannot both read the
// same maximum. The events(aggregate_id, sequence) unique index is the
// backstop if that discipline ever slips: a race becomes a rolled-back
// transaction, never a duplicate sequence.
func (eq engineQueries) AppendEvent(ctx context.Context, runID string, in engine.EventInput) (int64, error) {
	if runID == "" {
		return 0, errors.New("postgres: engine: AppendEvent requires a run id")
	}
	if in.Type == "" {
		return 0, errors.New("postgres: engine: AppendEvent requires an event type")
	}
	payload := jsonOrEmptyObject(in.Data)

	var sequence int64
	if err := eq.q.QueryRow(ctx,
		`SELECT (COALESCE(MAX(sequence), 0) + 1)::bigint FROM events WHERE aggregate_id = $1`, runID,
	).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("postgres: engine: AppendEvent: next sequence: %w", err)
	}

	if _, err := eq.q.Exec(ctx, insertEngineEventSQL,
		store.NewULID(), eq.namespaceID, runID, sequence, in.Type, []byte(payload),
	); err != nil {
		return 0, fmt.Errorf("postgres: engine: AppendEvent: %w", err)
	}
	if _, err := eq.q.Exec(ctx, insertEngineOutboxSQL,
		store.NewULID(), eq.namespaceID, in.Type, []byte(payload),
	); err != nil {
		return 0, fmt.Errorf("postgres: engine: AppendEvent: outbox: %w", err)
	}
	return sequence, nil
}

// Compile-time proof that the two halves implement the interfaces the engine
// declares. A missing method should be a build failure here, not a runtime
// surprise inside a transaction.
var (
	_ engine.Store = (*EngineStore)(nil)
	_ engine.Tx    = (*engineTx)(nil)
)
