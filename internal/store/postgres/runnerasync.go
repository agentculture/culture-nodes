package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
)

// Durable state for parked runner-protocol operations (task t9;
// migrations/0011, api/runner-protocol).
//
// This file is async.go's sibling for the OTHER asynchronous boundary, and
// the shape is deliberately the same because the invariant is the same one:
// §12.6 forbids a worker from holding a lease or a goroutine while somebody
// else's process does the work. An asynchronous ACTOR reports back through an
// inbound callback; a runner SERVICE does not report back at all — the
// runtime samples its status endpoint — so this table has one thing
// actor_invocations does not: a due time the sampler claims on.
//
// The whole life cycle lives here:
//
//	StartRunnerWait          park: release the lease, keep the fencing tuple,
//	                         schedule the deadline timer, record the row
//	ClaimDueRunnerOperations the sampler's queue read; claiming IS advancing
//	                         next_poll_at, so two samplers never overlap
//	RescheduleRunnerPoll     a sample that learned nothing terminal
//	TightenRunnerPoll        a callback: bring the next sample forward, and
//	                         nothing else, ever
//	CloseRunnerOperation     the operation is no longer in flight
//
// Nothing in this file commits workflow state. A terminal result is committed
// through CallbackStore.ResumeWaitingWork + engine.CompleteAttempt, exactly as
// an actor's terminal callback is — that path already enforces the fencing
// discipline, and a second implementation of it would be a second place for
// the rule to differ.

// Runner operation states. They are the same vocabulary actor_invocations
// uses (internal/actors' Invocation* constants), and deliberately so: an
// operator asking "what is this run waiting on" reads one set of words, and
// the scheduler's deadline path reads `waiting_external` from either table
// with the same comparison.
const (
	// RunnerOperationWaiting is an operation the control plane is still
	// waiting on: the work item is parked and no worker holds it.
	RunnerOperationWaiting = actors.InvocationWaiting
	// RunnerOperationCompleted is an operation whose terminal status
	// committed.
	RunnerOperationCompleted = actors.InvocationCompleted
	// RunnerOperationSuperseded is an operation whose terminal status arrived
	// too late to commit anything — the work had already been reclaimed,
	// cancelled, or completed by another path.
	RunnerOperationSuperseded = actors.InvocationSuperseded
	// RunnerOperationCancelled is an operation cancelled by the control plane.
	RunnerOperationCancelled = actors.InvocationCancelled
)

// RunnerOperation is one row of runner_invocations: everything a later sample
// needs in order to know what to ask, whom to ask, and what it is allowed to
// commit when the answer is terminal.
type RunnerOperation struct {
	// AttemptID is the protocol attempt id, and the row's key.
	AttemptID   string
	NamespaceID string
	RunID       string
	NodeRunID   string
	TokenID     string

	// The fencing tuple the dispatch held. A resume matches on all four or
	// commits nothing; see ResumeWaitingWork.
	WorkID       string
	WorkerID     string
	FencingToken int64
	Attempt      int

	NodeID string
	// RunnerRef is the registry name the dispatch resolved through. A sample
	// re-resolves the identity by this name so the registry stays the
	// enforcement point.
	RunnerRef string
	// Endpoint is where the operation was dispatched, for the operator
	// question "where is this running". It never carries a credential.
	Endpoint string
	// OperationID is what the status endpoint is sampled by.
	OperationID string

	State string
	// ObservedState is the last non-terminal state a sample read.
	ObservedState string

	PollAfterSeconds int
	NextPollAt       time.Time
	PollCount        int64
	LastSampledAt    time.Time
	// LastSampleError is the most recent classified dispatch failure. It is a
	// diagnostic, never evidence: nothing was measured.
	LastSampleError string

	StatusRetentionSeconds int
	SupportsCancellation   bool
	SupportsCallback       bool
	DeadlineTimerID        string
}

// PendingInvocation projects the fencing tuple onto the shape
// internal/actors' resume-and-commit path takes.
//
// It is a projection rather than a separate record because the two boundaries
// genuinely agree on this part: "which work item, under which lease owner,
// fencing token and attempt may this completion commit" is one question with
// one answer, and CallbackStore.ResumeWaitingWork is the one implementation
// of it. ActorRef carries the runner reference so an operator reading a late
// diagnostic sees what the wait was on.
func (r RunnerOperation) PendingInvocation() actors.PendingInvocation {
	return actors.PendingInvocation{
		AttemptID:    r.AttemptID,
		NamespaceID:  r.NamespaceID,
		RunID:        r.RunID,
		NodeRunID:    r.NodeRunID,
		TokenID:      r.TokenID,
		NodeID:       r.NodeID,
		WorkID:       r.WorkID,
		WorkerID:     r.WorkerID,
		FencingToken: r.FencingToken,
		Attempt:      r.Attempt,
		ActorRef:     r.RunnerRef,
		InvocationID: r.OperationID,
		State:        r.State,
	}
}

// StartRunnerWaitInput parks a leased work item on an accepted runner
// operation.
type StartRunnerWaitInput struct {
	// The fencing tuple exactly as ClaimWork handed it out. A mismatch means
	// the caller no longer holds the claim and nothing is written.
	WorkID       string
	WorkerID     string
	FencingToken int64
	Attempt      int

	NamespaceID string
	RunID       string
	NodeRunID   string
	TokenID     string
	NodeID      string

	// AttemptID is the protocol attempt id; it keys the row.
	AttemptID string
	// RunnerRef is the registry name the operation resolved through.
	RunnerRef string
	// Endpoint is the registered runner service's base URL.
	Endpoint string
	// OperationID is the id the runner acknowledged and the status endpoint
	// is sampled by.
	OperationID string

	// PollAfterSeconds is the runner's requested minimum sampling interval,
	// from its acceptance. Zero means it expressed no preference.
	PollAfterSeconds       int
	StatusRetentionSeconds int
	SupportsCancellation   bool
	SupportsCallback       bool

	// NextPollAt is when this operation first becomes due. Zero means "now",
	// which is right for a runner that asked for nothing: the first sample
	// then happens on the next sampler pass.
	NextPollAt time.Time

	// Deadline schedules the durable §12.7 deadline timer that fails this
	// attempt if no terminal status is ever read. Zero schedules none — which
	// is honest but means nothing will ever unstick the wait, so the worker
	// is expected to always pass one.
	Deadline time.Time
}

const parkRunnerWorkSQL = `
UPDATE work_items
SET state            = 'waiting',
    lease_owner      = NULL,
    lease_expires_at = NULL,
    state_version    = state_version + 1,
    updated_at       = now()
WHERE id = $1
  AND state = 'leased'
  AND lease_owner = $2
  AND fencing_token = $3
  AND attempt = $4
`

const insertRunnerInvocationSQL = `
INSERT INTO runner_invocations (
    attempt_id, namespace_id, run_id, node_run_id, token_id, work_id, worker_id,
    fencing_token, attempt, node_key, runner_ref, endpoint, operation_id, state,
    poll_after_seconds, next_poll_at, status_retention_seconds,
    supports_cancellation, supports_callback, deadline_timer_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'waiting_external',
        $14, $15, $16, $17, $18, $19)
`

// StartRunnerWait commits the whole §12.6 transition for a runner operation in
// one transaction: release the worker's lease without completing the item,
// mark the node run waiting_external, record the durable row, schedule the
// deadline timer, and append the audit event.
//
// It is one transaction for exactly the reason StartAsyncWait is: a partial
// application is a wedged run in either direction. A parked work item with no
// runner_invocations row would never be sampled again and never be resumable;
// a row over a still-leased item would let a status sample and a lease expiry
// both try to complete the same attempt.
//
// A caller whose fencing tuple no longer matches gets engine.ErrStaleClaim and
// writes nothing.
func (s *Store) StartRunnerWait(ctx context.Context, in StartRunnerWaitInput) error {
	switch {
	case in.WorkID == "":
		return fmt.Errorf("postgres: StartRunnerWait: workID is required")
	case in.WorkerID == "":
		return fmt.Errorf("postgres: StartRunnerWait: workerID is required")
	case in.AttemptID == "":
		return fmt.Errorf("postgres: StartRunnerWait: attemptID is required")
	case in.OperationID == "":
		return fmt.Errorf("postgres: StartRunnerWait: operationID is required; " +
			"without it there is no status to sample")
	case in.NamespaceID == "":
		return fmt.Errorf("postgres: StartRunnerWait: namespaceID is required")
	case in.RunID == "" || in.NodeRunID == "":
		return fmt.Errorf("postgres: StartRunnerWait: runID and nodeRunID are required")
	case in.Endpoint == "":
		return fmt.Errorf("postgres: StartRunnerWait: endpoint is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: StartRunnerWait: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	// The same per-run advisory lock the engine, the ledger and StartAsyncWait
	// take, so this transition queues behind (or ahead of) a completion for
	// the same run rather than interleaving with one.
	if _, err := tx.Exec(ctx, advisoryXactLockSQL, ledger.RunLockKey(in.RunID)); err != nil {
		return fmt.Errorf("postgres: StartRunnerWait: lock run %s: %w", in.RunID, err)
	}

	tag, err := tx.Exec(ctx, parkRunnerWorkSQL, in.WorkID, in.WorkerID, in.FencingToken, int32(in.Attempt))
	if err != nil {
		return fmt.Errorf("postgres: StartRunnerWait: park work item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: StartRunnerWait: work item %s: %w: %w", in.WorkID, engine.ErrStaleClaim, ErrStaleClaim)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE node_runs SET status = 'waiting_external', updated_at = now() WHERE id = $1`,
		in.NodeRunID,
	); err != nil {
		return fmt.Errorf("postgres: StartRunnerWait: mark node run waiting: %w", err)
	}

	timerID := ""
	if !in.Deadline.IsZero() {
		timerID = store.NewULID()
		payload, _ := json.Marshal(map[string]string{
			"attempt_id":   in.AttemptID,
			"operation_id": in.OperationID,
			"node_run_id":  in.NodeRunID,
			"runner_ref":   in.RunnerRef,
		})
		if _, err := tx.Exec(ctx,
			`INSERT INTO timers (id, namespace_id, run_id, node_run_id, timer_kind, fire_at, payload)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			timerID, in.NamespaceID, in.RunID, in.NodeRunID,
			string(TimerKindDeadline), tsOrNow(in.Deadline), payload,
		); err != nil {
			return fmt.Errorf("postgres: StartRunnerWait: schedule deadline timer: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, insertRunnerInvocationSQL,
		in.AttemptID, in.NamespaceID, in.RunID, in.NodeRunID, textOrNull(in.TokenID),
		in.WorkID, in.WorkerID, in.FencingToken, int32(in.Attempt), in.NodeID,
		in.RunnerRef, in.Endpoint, in.OperationID,
		int32OrNull(in.PollAfterSeconds), tsOrNow(in.NextPollAt), int32OrNull(in.StatusRetentionSeconds),
		in.SupportsCancellation, in.SupportsCallback, textOrNull(timerID),
	); err != nil {
		return fmt.Errorf("postgres: StartRunnerWait: record runner operation: %w", err)
	}

	if err := appendRunEventTx(ctx, tx, in.NamespaceID, in.RunID, TypeRunnerOperationParked, map[string]any{
		"run_id":        in.RunID,
		"node_run_id":   in.NodeRunID,
		"node_id":       in.NodeID,
		"attempt_id":    in.AttemptID,
		"work_id":       in.WorkID,
		"worker_id":     in.WorkerID,
		"fencing_token": in.FencingToken,
		"attempt":       in.Attempt,
		"operation_id":  in.OperationID,
		"runner_ref":    in.RunnerRef,
		"endpoint":      in.Endpoint,
	}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: StartRunnerWait: commit: %w", err)
	}
	return nil
}

// TypeRunnerOperationParked records a code node's dispatch entering the
// waiting_external park on a runner service. It is its own event type rather
// than TypeAttemptWaitingExternal's payload variant because "waiting on a
// runner we poll" and "waiting on an actor that calls back" are answered by
// different machinery, and an operator asking which one should not have to
// parse a payload to find out.
const TypeRunnerOperationParked = "dev.culture.nodes.runner.operation-parked"

const runnerInvocationColumns = `attempt_id, namespace_id, run_id, node_run_id, token_id, work_id, worker_id,
	fencing_token, attempt, node_key, runner_ref, endpoint, operation_id, state, observed_state,
	poll_after_seconds, next_poll_at, poll_count, last_sampled_at, last_sample_error,
	status_retention_seconds, supports_cancellation, supports_callback, deadline_timer_id`

// runnerInvocationColumnsR is runnerInvocationColumns qualified with the "r"
// alias the claim statement gives the table: an UPDATE ... FROM's RETURNING
// clause can see both the target table and the FROM-list, so unqualified
// column names there would be ambiguous (timers.go's timerColumnsT exists for
// the same reason).
const runnerInvocationColumnsR = `r.attempt_id, r.namespace_id, r.run_id, r.node_run_id, r.token_id, r.work_id,
	r.worker_id, r.fencing_token, r.attempt, r.node_key, r.runner_ref, r.endpoint, r.operation_id, r.state,
	r.observed_state, r.poll_after_seconds, r.next_poll_at, r.poll_count, r.last_sampled_at, r.last_sample_error,
	r.status_retention_seconds, r.supports_cancellation, r.supports_callback, r.deadline_timer_id`

func scanRunnerOperation(row invocationRowScanner) (RunnerOperation, error) {
	var (
		op                                             RunnerOperation
		tokenID, observedState, sampleErr, timerID     pgtype.Text
		pollAfter, retention                           pgtype.Int4
		nextPollAt, lastSampledAt                      pgtype.Timestamptz
		attempt                                        int32
		supportsCancellation, supportsCallback         bool
		runnerRef, endpoint, operationID, state, nodeK string
	)
	if err := row.Scan(
		&op.AttemptID, &op.NamespaceID, &op.RunID, &op.NodeRunID, &tokenID, &op.WorkID, &op.WorkerID,
		&op.FencingToken, &attempt, &nodeK, &runnerRef, &endpoint, &operationID, &state, &observedState,
		&pollAfter, &nextPollAt, &op.PollCount, &lastSampledAt, &sampleErr,
		&retention, &supportsCancellation, &supportsCallback, &timerID,
	); err != nil {
		return RunnerOperation{}, err
	}
	op.TokenID = textOrEmpty(tokenID)
	op.Attempt = int(attempt)
	op.NodeID = nodeK
	op.RunnerRef = runnerRef
	op.Endpoint = endpoint
	op.OperationID = operationID
	op.State = state
	op.ObservedState = textOrEmpty(observedState)
	op.PollAfterSeconds = int32Value(pollAfter)
	op.NextPollAt = tsValue(nextPollAt)
	op.LastSampledAt = tsValue(lastSampledAt)
	op.LastSampleError = textOrEmpty(sampleErr)
	op.StatusRetentionSeconds = int32Value(retention)
	op.SupportsCancellation = supportsCancellation
	op.SupportsCallback = supportsCallback
	op.DeadlineTimerID = textOrEmpty(timerID)
	return op, nil
}

// RunnerOperation loads one runner operation by its protocol attempt id.
func (s *Store) RunnerOperation(ctx context.Context, namespaceID, attemptID string) (RunnerOperation, error) {
	op, err := scanRunnerOperation(s.pool.QueryRow(ctx,
		`SELECT `+runnerInvocationColumns+` FROM runner_invocations WHERE attempt_id = $1 AND namespace_id = $2`,
		attemptID, namespaceID))
	if err != nil {
		if isNoRows(err) {
			return RunnerOperation{}, fmt.Errorf("postgres: runner operation %s: %w", attemptID, ErrNotFound)
		}
		return RunnerOperation{}, fmt.Errorf("postgres: RunnerOperation: %w", err)
	}
	return op, nil
}

// claimDueRunnerOperationsSQL is the sampler's queue read, and claiming a row
// IS rescheduling it: the same statement that returns an operation pushes its
// next_poll_at out by the caller's backoff and bumps poll_count.
//
// That single fact is what makes the sampler affordable and crash-safe at the
// same time:
//
//   - Two samplers (two worker processes, or a worker and an operator tool)
//     racing at the same instant cannot both take the same row. FOR UPDATE
//     SKIP LOCKED hands each caller a disjoint set, and the loser simply finds
//     the row not due.
//   - A sampler that dies between claiming and committing a terminal result
//     strands nothing. Nothing about the claim is "held": the row is due again
//     one backoff later, and whichever sampler is alive then picks it up. That
//     is the whole recovery story — there is no unstick step, no orphan sweep,
//     and no lease to expire.
//
// It deliberately does not stamp an owner. Unlike a work item, sampling is not
// something a caller can hold: the authority to COMMIT what a sample learned
// comes from the fencing tuple recorded at dispatch, checked by
// ResumeWaitingWork, not from having been the one who asked.
const claimDueRunnerOperationsSQL = `
UPDATE runner_invocations AS r
SET next_poll_at    = $2::timestamptz + ($4 * interval '1 second'),
    poll_count      = r.poll_count + 1,
    last_sampled_at = $2::timestamptz,
    updated_at      = now()
FROM (
    SELECT attempt_id
    FROM runner_invocations
    WHERE namespace_id = $1
      AND state = 'waiting_external'
      AND next_poll_at <= $2::timestamptz
    ORDER BY next_poll_at, attempt_id
    FOR UPDATE SKIP LOCKED
    LIMIT $3
) AS due
WHERE r.attempt_id = due.attempt_id
RETURNING ` + runnerInvocationColumnsR

// ClaimDueRunnerOperations claims up to limit parked operations whose next
// sample is due at or before now, pushing each one's next sample out by
// backoff.
//
// The returned rows carry the state as it was AFTER the claim (poll_count
// already incremented), which is what a caller wants: it is about to sample,
// and the count it reports in a diagnostic should include the sample it is
// making.
func (s *Store) ClaimDueRunnerOperations(
	ctx context.Context, namespaceID string, now time.Time, limit int, backoff time.Duration,
) ([]RunnerOperation, error) {
	switch {
	case namespaceID == "":
		return nil, fmt.Errorf("postgres: ClaimDueRunnerOperations: namespaceID is required")
	case limit <= 0:
		return nil, fmt.Errorf("postgres: ClaimDueRunnerOperations: limit must be positive")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if backoff <= 0 {
		// A non-positive backoff would leave the row due again immediately,
		// which is a hot loop against somebody else's service.
		backoff = time.Second
	}

	rows, err := s.pool.Query(ctx, claimDueRunnerOperationsSQL, namespaceID, now, int64(limit), backoff.Seconds())
	if err != nil {
		return nil, fmt.Errorf("postgres: ClaimDueRunnerOperations: %w", err)
	}
	defer rows.Close()

	var out []RunnerOperation
	for rows.Next() {
		op, err := scanRunnerOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: ClaimDueRunnerOperations: scan: %w", err)
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: ClaimDueRunnerOperations: %w", err)
	}
	return out, nil
}

// RescheduleRunnerPoll records what a completed sample learned about an
// operation that has NOT finished: the non-terminal state it reported, or the
// classified dispatch failure that stopped the sample from learning anything,
// plus when to try again.
//
// sampleError is a diagnostic, never evidence. A 404 from a runner that forgot
// an operation lands here rather than becoming a completion, and the attempt's
// own deadline timer is what eventually decides — see the protocol document's
// "a 404 on the status of an operation the runtime dispatched is not a
// completion and is never read as one".
func (s *Store) RescheduleRunnerPoll(
	ctx context.Context, namespaceID, attemptID string, nextPollAt time.Time, observedState, sampleError string,
) error {
	if attemptID == "" {
		return fmt.Errorf("postgres: RescheduleRunnerPoll: attemptID is required")
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE runner_invocations
		SET next_poll_at      = $3,
		    observed_state    = COALESCE(NULLIF($4, ''), observed_state),
		    last_sample_error = NULLIF($5, ''),
		    updated_at        = now()
		WHERE attempt_id = $1 AND namespace_id = $2 AND state = 'waiting_external'
	`, attemptID, namespaceID, tsOrNow(nextPollAt), observedState, sampleError)
	if err != nil {
		return fmt.Errorf("postgres: RescheduleRunnerPoll: %w", err)
	}
	return nil
}

// TightenRunnerPoll brings an operation's next sample forward to at, and does
// nothing else. It reports whether an in-flight operation was actually
// tightened.
//
// This is the entire mechanical power of the optional completion callback. The
// notification carries no result and is never believed: on receiving one the
// runtime samples the status endpoint it dispatched to, over the connection it
// authenticated, and learns the outcome there. A forged or replayed
// notification therefore costs at most one extra status read — and the
// `state = 'waiting_external'` guard means one aimed at an operation that
// already finished costs nothing at all.
//
// The `next_poll_at > $3` guard keeps it strictly a tightening: a callback can
// never push a sample LATER, so it cannot be used to delay the runtime's own
// discovery of a completion.
func (s *Store) TightenRunnerPoll(ctx context.Context, namespaceID, attemptID string, at time.Time) (bool, error) {
	if attemptID == "" {
		return false, fmt.Errorf("postgres: TightenRunnerPoll: attemptID is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE runner_invocations
		SET next_poll_at = $3, updated_at = now()
		WHERE attempt_id = $1 AND namespace_id = $2
		  AND state = 'waiting_external'
		  AND next_poll_at > $3
	`, attemptID, namespaceID, at)
	if err != nil {
		return false, fmt.Errorf("postgres: TightenRunnerPoll: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// CloseRunnerOperation moves an operation out of the waiting state.
//
// Like CloseInvocation it only ever moves a row OUT of 'waiting_external', so
// retiring something already retired is a no-op rather than an error. That is
// not politeness: two samples racing to a terminal status is expected traffic
// under this protocol, and the loser must be able to finish its own bookkeeping
// without turning a correctly handled duplicate into an error somebody retries.
func (s *Store) CloseRunnerOperation(ctx context.Context, namespaceID, attemptID, state string) error {
	if attemptID == "" {
		return fmt.Errorf("postgres: CloseRunnerOperation: attemptID is required")
	}
	if state == "" {
		state = RunnerOperationCompleted
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE runner_invocations
		SET state = $3, updated_at = now()
		WHERE attempt_id = $1 AND namespace_id = $2 AND state = 'waiting_external'
	`, attemptID, namespaceID, state)
	if err != nil {
		return fmt.Errorf("postgres: CloseRunnerOperation: %w", err)
	}
	return nil
}

// RunnerOperationByDeadlineTimer loads the parked runner operation a deadline
// timer belongs to, keyed by the deadline_timer_id StartRunnerWait stamped on
// it — the exact counterpart of InvocationByDeadlineTimer for the actor table,
// and the reason internal/scheduler's existing waiting_external deadline
// effect works for runner operations without a second implementation of it.
//
// It reports (zero, false, nil) rather than an error when no operation names
// this timer: a deadline timer that belongs to an actor invocation (or to
// nothing at all) is a legitimate case for the caller to treat as "not mine",
// not a fault.
func (s *Store) RunnerOperationByDeadlineTimer(ctx context.Context, timerID string) (actors.PendingInvocation, bool, error) {
	if timerID == "" {
		return actors.PendingInvocation{}, false, fmt.Errorf("postgres: RunnerOperationByDeadlineTimer: timerID is required")
	}
	op, err := scanRunnerOperation(s.pool.QueryRow(ctx,
		`SELECT `+runnerInvocationColumns+` FROM runner_invocations WHERE deadline_timer_id = $1`, timerID))
	if err != nil {
		if isNoRows(err) {
			return actors.PendingInvocation{}, false, nil
		}
		return actors.PendingInvocation{}, false, fmt.Errorf("postgres: RunnerOperationByDeadlineTimer: %w", err)
	}
	return op.PendingInvocation(), true, nil
}

// int32Value reads a nullable INTEGER column back as a plain int, with NULL
// reading as zero — the inverse of int32OrNull (async.go), which writes a
// non-positive int as NULL.
func int32Value(v pgtype.Int4) int {
	if !v.Valid {
		return 0
	}
	return int(v.Int32)
}
