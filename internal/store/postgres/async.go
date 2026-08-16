package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
)

// Asynchronous actor invocations (prd-spec §12.6, §13.3, §13.4) and the
// dispatch payload a worker needs to build one (§11.2, §13.1).
//
// The invariant this file exists to hold is §12.6's: "workers must not hold
// leases or goroutines for long-running agents. An asynchronous invocation
// changes the attempt to waiting_external and releases worker capacity."
// Releasing capacity means the work_items row leaves 'leased' — so nothing
// reclaims it, nothing re-claims it, and no goroutine anywhere is waiting on
// it — while KEEPING the fencing token and attempt number it was leased
// under. Those two numbers are the entire reason a callback arriving an hour
// later can still be told apart from one arriving after the work was
// reclaimed and re-run.
//
// Three states of work_items therefore exist rather than two:
//
//	ready     claimable
//	leased    a worker holds it, lease_expires_at applies
//	waiting   parked on an external party; no owner, no expiry, no reclaim
//	completed done
//
// 'waiting' is invisible to ClaimWork (which selects state = 'ready') and to
// ReclaimExpired (which selects state = 'leased'), which is exactly what
// "releases worker capacity without losing the claim" has to mean. The one
// thing that does move a waiting row is the scheduler's wait/retry timer
// effect, whose UPDATE targets `state <> 'completed'` — so a fired deadline
// timer returns the row to 'ready' and the next claim bumps the fencing
// token, which is precisely how a late callback comes to be late.

// WaitingWorkState is the work_items.state value for an item parked on an
// external party.
const WaitingWorkState = "waiting"

// StartAsyncWaitInput parks a leased work item on an asynchronous actor
// invocation.
type StartAsyncWaitInput struct {
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

	// AttemptID is the PROTOCOL attempt id: the value sent in §13.1's
	// `attempt_id`, used as the Idempotency-Key, and signed into the callback
	// token. It keys the actor_invocations row.
	AttemptID string
	// ActorID is the resolved actors-table row id (migration 0015) the
	// terminal callback commits into attempts.actor_id — per-actor stats'
	// attribution. Empty means unattributed (registry could not answer).
	ActorID string
	// ActorRef is the node's declared actor reference, kept for operator
	// questions ("what is this run waiting on").
	ActorRef string
	// InvocationID is the actor-assigned id from its §13.3 acceptance.
	InvocationID          string
	HeartbeatAfterSeconds int
	SupportsCancellation  bool

	// Deadline schedules a durable §12.7 deadline timer. Zero schedules none,
	// which is honest for an actor that declared no heartbeat interval and no
	// node timeout — but it does mean nothing will ever unstick the wait.
	Deadline time.Time
}

const parkWorkSQL = `
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

const insertActorInvocationSQL = `
INSERT INTO actor_invocations (
    attempt_id, namespace_id, run_id, node_run_id, token_id, work_id, worker_id,
    fencing_token, attempt, node_key, actor_ref, invocation_id, state,
    heartbeat_after_seconds, deadline_timer_id, supports_cancellation, actor_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'waiting_external', $13, $14, $15, $16)
`

// StartAsyncWait commits the whole §12.6 transition in one transaction:
// release the worker's lease without completing the item, mark the node run
// waiting_external, record the durable invocation, schedule its deadline
// timer, and append the audit event.
//
// It is one transaction because a partial application of that list is a
// wedged run in every direction: a parked work item with no invocation row
// can never be resumed, and an invocation row over a still-leased item would
// let a callback and a lease expiry both try to complete the same attempt.
//
// A caller whose fencing tuple no longer matches gets engine.ErrStaleClaim
// and writes nothing — the same guarantee CompleteWork gives.
func (s *Store) StartAsyncWait(ctx context.Context, in StartAsyncWaitInput) error {
	switch {
	case in.WorkID == "":
		return errors.New("postgres: StartAsyncWait: workID is required")
	case in.WorkerID == "":
		return errors.New("postgres: StartAsyncWait: workerID is required")
	case in.AttemptID == "":
		return errors.New("postgres: StartAsyncWait: attemptID is required")
	case in.NamespaceID == "":
		return errors.New("postgres: StartAsyncWait: namespaceID is required")
	case in.RunID == "" || in.NodeRunID == "":
		return errors.New("postgres: StartAsyncWait: runID and nodeRunID are required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: StartAsyncWait: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	// The same per-run advisory lock the engine and the ledger take, so this
	// transition queues behind (or ahead of) a completion for the same run
	// rather than interleaving with one.
	if _, err := tx.Exec(ctx, advisoryXactLockSQL, ledger.RunLockKey(in.RunID)); err != nil {
		return fmt.Errorf("postgres: StartAsyncWait: lock run %s: %w", in.RunID, err)
	}

	tag, err := tx.Exec(ctx, parkWorkSQL, in.WorkID, in.WorkerID, in.FencingToken, int32(in.Attempt))
	if err != nil {
		return fmt.Errorf("postgres: StartAsyncWait: park work item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: StartAsyncWait: work item %s: %w: %w", in.WorkID, engine.ErrStaleClaim, ErrStaleClaim)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE node_runs SET status = 'waiting_external', updated_at = now() WHERE id = $1`,
		in.NodeRunID,
	); err != nil {
		return fmt.Errorf("postgres: StartAsyncWait: mark node run waiting: %w", err)
	}

	timerID := ""
	if !in.Deadline.IsZero() {
		timerID = store.NewULID()
		payload, _ := json.Marshal(map[string]string{
			"attempt_id":    in.AttemptID,
			"invocation_id": in.InvocationID,
			"node_run_id":   in.NodeRunID,
		})
		if _, err := tx.Exec(ctx,
			`INSERT INTO timers (id, namespace_id, run_id, node_run_id, timer_kind, fire_at, payload)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			timerID, in.NamespaceID, in.RunID, in.NodeRunID,
			string(TimerKindDeadline), tsOrNow(in.Deadline), payload,
		); err != nil {
			return fmt.Errorf("postgres: StartAsyncWait: schedule deadline timer: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, insertActorInvocationSQL,
		in.AttemptID, in.NamespaceID, in.RunID, in.NodeRunID, textOrNull(in.TokenID),
		in.WorkID, in.WorkerID, in.FencingToken, int32(in.Attempt), in.NodeID,
		textOrNull(in.ActorRef), textOrNull(in.InvocationID),
		int32OrNull(in.HeartbeatAfterSeconds), textOrNull(timerID), in.SupportsCancellation,
		textOrNull(in.ActorID),
	); err != nil {
		return fmt.Errorf("postgres: StartAsyncWait: record invocation: %w", err)
	}

	if err := appendRunEventTx(ctx, tx, in.NamespaceID, in.RunID, TypeAttemptWaitingExternal, map[string]any{
		"run_id":        in.RunID,
		"node_run_id":   in.NodeRunID,
		"node_id":       in.NodeID,
		"attempt_id":    in.AttemptID,
		"work_id":       in.WorkID,
		"worker_id":     in.WorkerID,
		"fencing_token": in.FencingToken,
		"attempt":       in.Attempt,
		"invocation_id": in.InvocationID,
		"actor_ref":     in.ActorRef,
	}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: StartAsyncWait: commit: %w", err)
	}
	return nil
}

// TypeAttemptWaitingExternal records the §12.6 transition into
// waiting_external. It is its own event type because "this run is waiting on
// someone else" is an operational answer, and finding it inside a generic
// attempt event would mean parsing a payload to learn it.
const TypeAttemptWaitingExternal = "dev.culture.nodes.attempt.waiting-external"

const advisoryXactLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`

// CallbackStore is the internal/actors ingest surface, bound to a namespace.
//
// It is a distinct type from Store rather than more methods on Store because
// the ingest's questions are all scoped to one namespace, and threading a
// namespace id through every call would put the same argument on eight
// signatures for no benefit.
type CallbackStore struct {
	store       *Store
	namespaceID string
	// pickup lets a mid-execution `signal` emission fire the namespace's
	// event routes as well as its signal waits (issue #43, design D9/D11).
	// Nil is a complete callback store that simply does no route pickup —
	// the honest default for a deployment wired without an engine, and what
	// every pre-t21 caller gets.
	pickup engine.EventPickupRunner
}

// CallbackStoreOption configures a CallbackStore.
type CallbackStoreOption func(*CallbackStore)

// WithEventPickup lets emissions from this callback store pick up event
// routes. It takes the engine rather than building one, because the pickup
// has to run against the SAME workflow cache and id factory the rest of the
// process uses.
func WithEventPickup(pickup engine.EventPickupRunner) CallbackStoreOption {
	return func(cs *CallbackStore) {
		if pickup != nil {
			cs.pickup = pickup
		}
	}
}

// NewCallbackStore returns the actors.CallbackStore implementation for a
// namespace.
func NewCallbackStore(s *Store, namespaceID string, opts ...CallbackStoreOption) (*CallbackStore, error) {
	if s == nil {
		return nil, errors.New("postgres: NewCallbackStore requires a store")
	}
	if namespaceID == "" {
		return nil, errors.New("postgres: NewCallbackStore requires a namespace id")
	}
	cs := &CallbackStore{store: s, namespaceID: namespaceID}
	for _, opt := range opts {
		if opt != nil {
			opt(cs)
		}
	}
	return cs, nil
}

// invocationColumns is the column list both Invocation and
// InvocationByDeadlineTimer select -- the same row shape reached by two
// different keys (attempt_id, the protocol identity; deadline_timer_id, the
// reverse link a fired deadline timer names).
const invocationColumns = `attempt_id, namespace_id, run_id, node_run_id, token_id, node_key, work_id,
       worker_id, fencing_token, attempt, actor_ref, invocation_id, state, last_sequence, actor_id`

const selectInvocationSQL = `
SELECT ` + invocationColumns + `
FROM actor_invocations
WHERE attempt_id = $1 AND namespace_id = $2
`

// invocationRowScanner is satisfied by pgx.Row, matching timers.go's own
// timerRowScanner pattern.
type invocationRowScanner interface {
	Scan(dest ...any) error
}

// scanPendingInvocation scans one invocationColumns row into an
// actors.PendingInvocation, shared by every query that selects that column
// list.
func scanPendingInvocation(row invocationRowScanner) (actors.PendingInvocation, error) {
	var (
		inv                                      actors.PendingInvocation
		tokenID, actorRef, invocationID, actorID pgtype.Text
		attempt                                  int32
	)
	if err := row.Scan(
		&inv.AttemptID, &inv.NamespaceID, &inv.RunID, &inv.NodeRunID, &tokenID, &inv.NodeID,
		&inv.WorkID, &inv.WorkerID, &inv.FencingToken, &attempt, &actorRef, &invocationID,
		&inv.State, &inv.LastSequence, &actorID,
	); err != nil {
		return actors.PendingInvocation{}, err
	}
	inv.TokenID = textOrEmpty(tokenID)
	inv.ActorRef = textOrEmpty(actorRef)
	inv.InvocationID = textOrEmpty(invocationID)
	inv.ActorID = textOrEmpty(actorID)
	inv.Attempt = int(attempt)
	return inv, nil
}

// Invocation loads one in-flight invocation.
func (cs *CallbackStore) Invocation(ctx context.Context, attemptID string) (actors.PendingInvocation, error) {
	inv, err := scanPendingInvocation(cs.store.pool.QueryRow(ctx, selectInvocationSQL, attemptID, cs.namespaceID))
	if err != nil {
		if isNoRows(err) {
			return actors.PendingInvocation{}, fmt.Errorf("postgres: invocation %s: %w", attemptID, actors.ErrUnknownAttempt)
		}
		return actors.PendingInvocation{}, fmt.Errorf("postgres: Invocation: %w", err)
	}
	return inv, nil
}

const selectInvocationByDeadlineTimerSQL = `
SELECT ` + invocationColumns + `
FROM actor_invocations
WHERE deadline_timer_id = $1
`

// InvocationByDeadlineTimer loads the invocation a deadline timer belongs
// to, keyed by the deadline_timer_id StartAsyncWait stamped on it when the
// timer was scheduled -- the reverse of Invocation's own attempt_id lookup,
// which is what a fired TimerKindDeadline row has instead (it names a
// timer, not a protocol attempt id).
//
// It reports (zero, false, nil) rather than an error when no invocation
// names this timer: a deadline timer that was never StartAsyncWait's own
// (a hand-built test fixture, for instance) is a legitimate case for the
// caller to treat as "nothing to fail", not a fault.
func (s *Store) InvocationByDeadlineTimer(ctx context.Context, timerID string) (actors.PendingInvocation, bool, error) {
	if timerID == "" {
		return actors.PendingInvocation{}, false, fmt.Errorf("postgres: InvocationByDeadlineTimer: timerID is required")
	}
	inv, err := scanPendingInvocation(s.pool.QueryRow(ctx, selectInvocationByDeadlineTimerSQL, timerID))
	if err != nil {
		if isNoRows(err) {
			return actors.PendingInvocation{}, false, nil
		}
		return actors.PendingInvocation{}, false, fmt.Errorf("postgres: InvocationByDeadlineTimer: %w", err)
	}
	return inv, true, nil
}

// callbackScope namespaces one attempt's event ids inside idempotency_keys,
// so two attempts using the same event id (which nothing forbids — event ids
// are only required to be stable per actor) never collide.
func callbackScope(attemptID string) string { return "actor-callback:" + attemptID }

// ClaimCallbackEvent records an event id as ingested, reporting false when it
// was already recorded.
//
// The unique constraint on (namespace_id, scope, idempotency_key) is the
// whole mechanism: ON CONFLICT DO NOTHING makes "did I win the insert" the
// same question as "is this the first delivery", answered atomically. A
// read-then-insert would leave a window in which two concurrent redeliveries
// both believe they are first.
func (cs *CallbackStore) ClaimCallbackEvent(ctx context.Context, inv actors.PendingInvocation, eventID string) (bool, error) {
	tag, err := cs.store.pool.Exec(ctx, `
		INSERT INTO idempotency_keys (id, namespace_id, scope, idempotency_key, request_digest, status, completed_at)
		VALUES ($1, $2, $3, $4, $5, 'completed', now())
		ON CONFLICT (namespace_id, scope, idempotency_key) DO NOTHING
	`, store.NewULID(), cs.namespaceID, callbackScope(inv.AttemptID), eventID, inv.RunID)
	if err != nil {
		return false, fmt.Errorf("postgres: ClaimCallbackEvent: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ReleaseCallbackEvent forgets a claim so a redelivery is processed again.
func (cs *CallbackStore) ReleaseCallbackEvent(ctx context.Context, inv actors.PendingInvocation, eventID string) error {
	_, err := cs.store.pool.Exec(ctx,
		`DELETE FROM idempotency_keys WHERE namespace_id = $1 AND scope = $2 AND idempotency_key = $3`,
		cs.namespaceID, callbackScope(inv.AttemptID), eventID)
	if err != nil {
		return fmt.Errorf("postgres: ReleaseCallbackEvent: %w", err)
	}
	return nil
}

// AdvanceCallbackSequence raises the per-attempt high-water mark, reporting
// false when sequence did not exceed it.
//
// The comparison is inside the UPDATE's WHERE clause on purpose: it is a
// compare-and-set, so two concurrent deliveries of sequences 5 and 6 can
// never both succeed at raising the mark from 4 to their own value.
func (cs *CallbackStore) AdvanceCallbackSequence(ctx context.Context, attemptID string, sequence int64) (bool, error) {
	tag, err := cs.store.pool.Exec(ctx, `
		UPDATE actor_invocations
		SET last_sequence = $3, updated_at = now()
		WHERE attempt_id = $1 AND namespace_id = $2 AND last_sequence < $3
	`, attemptID, cs.namespaceID, sequence)
	if err != nil {
		return false, fmt.Errorf("postgres: AdvanceCallbackSequence: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RollbackCallbackSequence lowers the high-water mark back to previous, and
// only while it still equals sequence.
//
// The equality guard is what makes an undo safe here: it fires exactly when
// this delivery's own advance is still the newest one, and no-ops when
// anything else has moved the mark since. A no-op is the correct outcome in
// that case — a later event's mark outranks the compensation of an earlier
// one, and the event whose processing failed is protected by its released
// event-id claim, not by the mark.
func (cs *CallbackStore) RollbackCallbackSequence(ctx context.Context, attemptID string, sequence, previous int64) error {
	_, err := cs.store.pool.Exec(ctx, `
		UPDATE actor_invocations
		SET last_sequence = $4, updated_at = now()
		WHERE attempt_id = $1 AND namespace_id = $2 AND last_sequence = $3
	`, attemptID, cs.namespaceID, sequence, previous)
	if err != nil {
		return fmt.Errorf("postgres: RollbackCallbackSequence: %w", err)
	}
	return nil
}

// TouchInvocation records liveness, and fills in an invocation id the actor
// supplied late. An empty invocationID leaves the recorded one alone: an
// actor sending a heartbeat is not retracting the id it gave at acceptance.
func (cs *CallbackStore) TouchInvocation(ctx context.Context, attemptID, invocationID string, at time.Time) error {
	_, err := cs.store.pool.Exec(ctx, `
		UPDATE actor_invocations
		SET invocation_id = COALESCE(NULLIF($3, ''), invocation_id),
		    updated_at    = $4
		WHERE attempt_id = $1 AND namespace_id = $2
	`, attemptID, cs.namespaceID, invocationID, tsOrNow(at))
	if err != nil {
		return fmt.Errorf("postgres: TouchInvocation: %w", err)
	}
	return nil
}

// CloseInvocation moves an invocation out of the waiting state. It only ever
// moves a row out of 'waiting_external', matching this package's convention
// that retiring something already retired is a no-op rather than an error.
func (cs *CallbackStore) CloseInvocation(ctx context.Context, attemptID, state string) error {
	_, err := cs.store.pool.Exec(ctx, `
		UPDATE actor_invocations
		SET state = $3, updated_at = now()
		WHERE attempt_id = $1 AND namespace_id = $2 AND state = 'waiting_external'
	`, attemptID, cs.namespaceID, state)
	if err != nil {
		return fmt.Errorf("postgres: CloseInvocation: %w", err)
	}
	return nil
}

const resumeWaitingWorkSQL = `
UPDATE work_items
SET state            = 'leased',
    lease_owner      = $2,
    lease_expires_at = now() + ($5 * interval '1 second'),
    state_version    = state_version + 1,
    updated_at       = now()
WHERE id = $1
  AND state = 'waiting'
  AND lease_owner IS NULL
  AND fencing_token = $3
  AND attempt = $4
`

// ResumeWaitingWork re-leases a parked work item under the fencing tuple its
// dispatch held, so engine.CompleteAttempt's own fenced guard (which requires
// state = 'leased' under that exact tuple) can match.
//
// This is where §13.4's "completion after cancellation or attempt replacement
// cannot commit workflow state" is actually enforced for the async path. If a
// deadline timer returned the row to 'ready' and another worker claimed it,
// the row's fencing_token and attempt have both moved on, `state` is no
// longer 'waiting', and this UPDATE matches nothing — so the late completion
// never gets far enough to reach the engine at all.
func (cs *CallbackStore) ResumeWaitingWork(ctx context.Context, inv actors.PendingInvocation, lease time.Duration) error {
	if lease <= 0 {
		lease = actors.DefaultResumeLease
	}
	tx, err := cs.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: ResumeWaitingWork: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	tag, err := tx.Exec(ctx, resumeWaitingWorkSQL,
		inv.WorkID, inv.WorkerID, inv.FencingToken, int32(inv.Attempt), lease.Seconds())
	if err != nil {
		return fmt.Errorf("postgres: ResumeWaitingWork: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: work item %s: %w: %w", inv.WorkID, engine.ErrStaleClaim, ErrStaleClaim)
	}
	// The node run leaves waiting_external here rather than at completion, so
	// an observer never sees a node run still "waiting" while its completion
	// transaction is running.
	if _, err := tx.Exec(ctx,
		`UPDATE node_runs SET status = 'running', updated_at = now() WHERE id = $1 AND status = 'waiting_external'`,
		inv.NodeRunID,
	); err != nil {
		return fmt.Errorf("postgres: ResumeWaitingWork: mark node run running: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: ResumeWaitingWork: commit: %w", err)
	}
	return nil
}

const reparkResumedWorkSQL = `
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

// ReparkResumedWork is ResumeWaitingWork's inverse: the work item goes back to
// 'waiting' under the same fencing tuple, and its node run back to
// waiting_external, when the completion it was resumed for did not commit.
//
// It is the same statement as StartAsyncWait's parkWorkSQL, and deliberately
// so: the row must land in exactly the state the dispatch's park left it in,
// or the redelivery's own ResumeWaitingWork (state = 'waiting', no owner, same
// fencing tuple) will not match and a recoverable failure becomes a permanent
// one.
//
// Zero rows matched is success, not a fault. The only ways to get there are
// the engine having completed the item after all (the CloseInvocation-failed
// path never calls this, but a redelivery race can) or a newer claimant owning
// it — and in both, "leave it alone" is what an undo owes.
func (cs *CallbackStore) ReparkResumedWork(ctx context.Context, inv actors.PendingInvocation) error {
	tx, err := cs.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: ReparkResumedWork: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	tag, err := tx.Exec(ctx, reparkResumedWorkSQL,
		inv.WorkID, inv.WorkerID, inv.FencingToken, int32(inv.Attempt))
	if err != nil {
		return fmt.Errorf("postgres: ReparkResumedWork: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE node_runs SET status = 'waiting_external', updated_at = now() WHERE id = $1 AND status = 'running'`,
		inv.NodeRunID,
	); err != nil {
		return fmt.Errorf("postgres: ReparkResumedWork: mark node run waiting: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: ReparkResumedWork: commit: %w", err)
	}
	return nil
}

// AppendRunEvent appends one diagnostic event against a run.
func (cs *CallbackStore) AppendRunEvent(ctx context.Context, namespaceID, runID, eventType string, data map[string]any) error {
	if namespaceID == "" {
		namespaceID = cs.namespaceID
	}
	payload, err := json.Marshal(data)
	if err != nil {
		payload = []byte(`{}`)
	}
	_, err = cs.store.InsertEvent(ctx, InsertEventInput{
		NamespaceID:   namespaceID,
		AggregateType: "run",
		AggregateID:   runID,
		EventType:     eventType,
		Data:          payload,
	})
	if err != nil {
		return fmt.Errorf("postgres: AppendRunEvent: %w", err)
	}
	return nil
}

// appendRunEventTx is AppendRunEvent inside a caller's transaction. The
// sequence is MAX+1, which is safe only because every caller holds the run's
// advisory lock for the whole transaction — the same discipline
// engineQueries.AppendEvent relies on, with the events(aggregate_id,
// sequence) unique index as the backstop if it ever slips.
func appendRunEventTx(ctx context.Context, tx pgx.Tx, namespaceID, runID, eventType string, data map[string]any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		payload = []byte(`{}`)
	}
	var sequence int64
	if err := tx.QueryRow(ctx,
		`SELECT (COALESCE(MAX(sequence), 0) + 1)::bigint FROM events WHERE aggregate_id = $1`, runID,
	).Scan(&sequence); err != nil {
		return fmt.Errorf("postgres: appendRunEventTx: next sequence: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO events (id, namespace_id, aggregate_type, aggregate_id, sequence, event_type, source, data, occurred_at)
		 VALUES ($1, $2, 'run', $3, $4, $5, 'nodes', $6, now())`,
		store.NewULID(), namespaceID, runID, sequence, eventType, payload,
	); err != nil {
		return fmt.Errorf("postgres: appendRunEventTx: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox (id, namespace_id, topic, payload, status, available_at)
		 VALUES ($1, $2, $3, $4, 'pending', now())`,
		store.NewULID(), namespaceID, eventType, payload,
	); err != nil {
		return fmt.Errorf("postgres: appendRunEventTx: outbox: %w", err)
	}
	return nil
}

// Dispatch is everything a worker needs to build a §13.1 invocation for one
// node run, resolved in a single query.
//
// It is one query rather than four calls because a worker asks this question
// once per claimed item and the four rows involved (work item → node run →
// run → workflow version) are a fixed join, not a graph walk.
type Dispatch struct {
	NamespaceID    string
	RunID          string
	NodeRunID      string
	TokenID        string
	NodeID         string
	VisitCount     int
	RunInput       json.RawMessage
	WorkflowKey    string
	WorkflowDigest string
	NormalizedIR   json.RawMessage
	// ActorAffinity is the routing this run recorded at creation
	// (migrations/0033), nil when it resolved none. It is read here, with the
	// run's input and the pinned IR, because a dispatch has to apply it
	// BEFORE it reads the node's declared `uses` -- see
	// internal/worker/affinity.go.
	ActorAffinity json.RawMessage
}

const selectDispatchSQL = `
SELECT nr.namespace_id, nr.run_id, nr.id, nr.token_id, nr.node_key, nr.visit_count,
       r.input, wv.workflow_key, wv.content_digest, wv.normalized_ir, r.actor_affinity
FROM node_runs AS nr
JOIN runs AS r ON r.id = nr.run_id
JOIN workflow_versions AS wv ON wv.id = r.workflow_version_id
WHERE nr.id = $1
`

// LoadDispatch resolves a node run to the pinned definition and run input a
// dispatch is built from.
func (s *Store) LoadDispatch(ctx context.Context, nodeRunID string) (Dispatch, error) {
	var (
		d          Dispatch
		tokenID    pgtype.Text
		visitCount int32
		input      []byte
		ir         []byte
		affinity   []byte
	)
	err := s.pool.QueryRow(ctx, selectDispatchSQL, nodeRunID).Scan(
		&d.NamespaceID, &d.RunID, &d.NodeRunID, &tokenID, &d.NodeID, &visitCount,
		&input, &d.WorkflowKey, &d.WorkflowDigest, &ir, &affinity,
	)
	if err != nil {
		if isNoRows(err) {
			return Dispatch{}, fmt.Errorf("postgres: node run %s: %w", nodeRunID, ErrNotFound)
		}
		return Dispatch{}, fmt.Errorf("postgres: LoadDispatch: %w", err)
	}
	d.TokenID = textOrEmpty(tokenID)
	d.VisitCount = int(visitCount)
	d.RunInput = json.RawMessage(input)
	d.NormalizedIR = json.RawMessage(ir)
	d.ActorAffinity = jsonOrNil(affinity)
	return d, nil
}

// NodeOutput returns the output of a node's most recent succeeded attempt in
// a run — what a `/nodes/<id>/output` binding resolves to (§11.2). It is the
// same statement engineQueries.NodeOutput runs, exposed on Store so a worker
// resolving bindings outside an engine transaction gets the same answer the
// engine would give inside one.
func (s *Store) NodeOutput(ctx context.Context, runID, nodeID string) (json.RawMessage, error) {
	var result []byte
	err := s.pool.QueryRow(ctx, `
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
		return nil, fmt.Errorf("postgres: NodeOutput: %w", err)
	}
	return json.RawMessage(result), nil
}

// NodeEvidence returns the run's live evidence ledger records belonging to a
// node's node runs, in id (append) order — what a `/nodes/<id>/evidence`
// binding resolves to (§11.2). Evidence identity is the node run: the engine
// stamps node_run_id on every accepted delta record, and node evidence
// carries no SubjectRef, so the selector joins node_runs on run_id +
// node_key exactly like NodeOutput does for attempts. Zero records is an
// empty slice, not an error: "no evidence was appended" is a true answer.
func (s *Store) NodeEvidence(ctx context.Context, runID, nodeID string) ([]ledger.Record, error) {
	return queryNodeEvidence(ctx, s.pool, runID, nodeID)
}

var selectNodeEvidenceSQL = `
SELECT ` + prefixedLedgerRecordColumns("lr") + `
FROM ledger_records AS lr
JOIN node_runs AS nr ON nr.id = lr.node_run_id
WHERE nr.run_id = $1 AND nr.node_key = $2 AND lr.record_type = 'evidence'
  AND NOT EXISTS (
      SELECT 1 FROM ledger_records AS sup
      WHERE sup.run_id = lr.run_id AND sup.supersedes = lr.id
  )
ORDER BY lr.id
`

// queryNodeEvidence is NodeEvidence's statement, shared by Store (a worker
// resolving bindings outside a transaction) and engineQueries (the engine's
// end-node output binding inside one), so both give the same answer. The
// NOT EXISTS clause is ledger.Live's rule in SQL: a record another record of
// the same run supersedes is not live, whatever superseded the supersessor.
func queryNodeEvidence(ctx context.Context, q ledgerQuerier, runID, nodeID string) ([]ledger.Record, error) {
	rows, err := q.Query(ctx, selectNodeEvidenceSQL, runID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("postgres: NodeEvidence: %w", err)
	}
	defer rows.Close()

	out := make([]ledger.Record, 0)
	for rows.Next() {
		rec, err := scanLedgerRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: NodeEvidence: scan: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: NodeEvidence: %w", err)
	}
	return out, nil
}

func int32OrNull(v int) pgtype.Int4 {
	if v <= 0 {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(v), Valid: true}
}

// Compile-time proof that the callback store satisfies the ingest surface
// internal/actors declares. A missing method should break the build here, not
// at the first callback in production.
var _ actors.CallbackStore = (*CallbackStore)(nil)
