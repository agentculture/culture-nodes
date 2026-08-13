package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store"
)

// The signal half of the durable wait surface (task t10, issue #39, spec
// decision c35): a wait node's until.signal parks its run on a
// signal_subscriptions row exactly the way until.duration parks on a
// timers row (wait.go's StartDurableWait), and an inbound signal event's
// delivery is what wakes it.
//
// Single-writer authority (why delivery resumes here, in the API handler's
// transaction, and not via the scheduler): the resume effect is the same
// idempotent, guarded shape as the scheduler's TimerKindWait effect —
// flip the parked work item back to 'ready' (internal/scheduler's
// makeWorkItemAvailableSQL), though with a stricter `state = 'waiting'`
// guard (see fireSubscription) — and it runs in the one PostgreSQL
// transaction that also appends the event fact and marks the subscription
// fired.
// PostgreSQL therefore stays the single authority over waiting state: there
// is no cross-process hand-off to lose (no relayed resume that could lag or
// double-fire), and node-run COMPLETION authority is untouched — delivery
// never completes anything, it only makes the work item claimable again,
// and the worker that re-claims it completes the node run through the
// engine's ordinary §12.5 transaction (internal/worker/wait.go), which is
// what keeps the §9.7 loop bounds enforced across the park. Routing
// delivery through the scheduler instead would add a second hop
// (API → outbox → scheduler → the same UPDATE) with no additional safety,
// since the scheduler's own effect commits through this very database
// anyway.
//
// Delivery semantics in this pass (documented limitation, issue #43):
// subscription-then-event resumes; event-then-subscription stays parked.
// DeliverSignalEvent fires the subscriptions that are pending at append
// time and never scans event history for a later subscriber — see the
// migration comment (migrations/0016_signal_events.sql) for why the
// append-only fact table keeps retroactive/multi-consumer pickup buildable
// without a schema change.

// Signal subscription status values (signal_subscriptions.status). The
// 'canceled' spelling (one l) matches the timers table's own vocabulary
// (TimerStatusCanceled), since run cancellation retires both in the same
// breath (internal/api's cancelRun REAP step).
const (
	SignalSubscriptionPending  = "pending"
	SignalSubscriptionFired    = "fired"
	SignalSubscriptionCanceled = "canceled"
)

// SignalEvent is a signal_events row: one append-only external signal fact.
// RunID reads back "" when the event was namespace-wide.
type SignalEvent struct {
	ID          string
	NamespaceID string
	RunID       string
	Name        string
	Payload     json.RawMessage
	Emitter     string
	CreatedAt   time.Time
}

// SignalSubscription is a signal_subscriptions row: one node run parked on
// a named signal. FiredEventID and FiredAt read back zero while the row is
// pending or canceled.
type SignalSubscription struct {
	ID           string
	NamespaceID  string
	RunID        string
	NodeRunID    string
	EventName    string
	Status       string
	FiredEventID string
	CreatedAt    time.Time
	FiredAt      time.Time
}

// StartDurableSignalWaitInput parks a leased work item on a signal
// subscription. It is StartDurableWaitInput with the timer replaced by the
// subscription — same fencing tuple, same fenced release, and deliberately
// no deadline: an undelivered signal leaves the run parked and inspectable,
// never timed out by a dispatch default (the spec's honesty condition for
// issue #39).
type StartDurableSignalWaitInput struct {
	// The fencing tuple exactly as ClaimWork handed it out. A mismatch means
	// the caller no longer holds the claim and nothing is written.
	WorkID       string
	WorkerID     string
	FencingToken int64
	Attempt      int

	NamespaceID string
	RunID       string
	NodeRunID   string
	NodeID      string
	// AttemptID is the protocol attempt id of the dispatch that armed the
	// wait, recorded on the audit event so an operator can tie the park back
	// to the dispatch that produced it.
	AttemptID string

	// SubscriptionID is the subscription row's id. The worker derives it
	// deterministically from the node run (internal/worker's
	// signalSubscriptionID), so a crashed-and-redispatched park re-adopts
	// the same subscription instead of arming a second one.
	SubscriptionID string
	// EventName is the signal name the node run waits for (until.signal).
	EventName string
}

// insertSignalSubscriptionSQL is ON CONFLICT DO NOTHING for the same reason
// wait.go's insertWaitTimerSQL is: if the row already exists (a re-park
// after an anomalous early re-dispatch), the original subscription is the
// one a delivery may already have fired, and it must not be reset to
// pending.
const insertSignalSubscriptionSQL = `
INSERT INTO signal_subscriptions (id, namespace_id, run_id, node_run_id, event_name)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO NOTHING
`

// TypeAttemptWaitingSignal records the transition into a durable signal
// wait — the signal-kind sibling of TypeAttemptWaitingTimer, distinct for
// the same reason: "this run is waiting for signal <name>" is an
// operational answer an operator should not have to dig out of a generic
// waiting event's payload.
const TypeAttemptWaitingSignal = "dev.culture.nodes.attempt.waiting-signal"

// TypeSignalDelivered is the outbox topic every accepted signal delivery
// appends — the audit fact that an external event entered the system,
// whether or not anything was waiting for it.
const TypeSignalDelivered = "dev.culture.nodes.signal.delivered"

// TypeSignalResumed is the per-run audit event a delivery appends for each
// subscription it fired: this run's wait ended because that event arrived.
const TypeSignalResumed = "dev.culture.nodes.signal.resumed"

// StartDurableSignalWait commits the whole park in one transaction: release
// the worker's lease without completing the item, mark the node run
// waiting_external, persist the pending subscription, and append the audit
// event. It is StartDurableWait with a subscription where the timer would
// be, one transaction for the identical reason: a parked work item with no
// subscription can never be woken, and a subscription over a still-leased
// item would race the lease's own expiry. A caller whose fencing tuple no
// longer matches gets engine.ErrStaleClaim and writes nothing.
func (s *Store) StartDurableSignalWait(ctx context.Context, in StartDurableSignalWaitInput) error {
	switch {
	case in.WorkID == "":
		return errors.New("postgres: StartDurableSignalWait: workID is required")
	case in.WorkerID == "":
		return errors.New("postgres: StartDurableSignalWait: workerID is required")
	case in.NamespaceID == "":
		return errors.New("postgres: StartDurableSignalWait: namespaceID is required")
	case in.RunID == "" || in.NodeRunID == "":
		return errors.New("postgres: StartDurableSignalWait: runID and nodeRunID are required")
	case in.SubscriptionID == "":
		return errors.New("postgres: StartDurableSignalWait: subscriptionID is required")
	case in.EventName == "":
		return errors.New("postgres: StartDurableSignalWait: eventName is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: StartDurableSignalWait: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	// The same per-run advisory lock the engine and the ledger take, so this
	// transition queues behind (or ahead of) a completion, a cancellation,
	// or a concurrent delivery for the same run rather than interleaving.
	if _, err := tx.Exec(ctx, advisoryXactLockSQL, ledger.RunLockKey(in.RunID)); err != nil {
		return fmt.Errorf("postgres: StartDurableSignalWait: lock run %s: %w", in.RunID, err)
	}

	tag, err := tx.Exec(ctx, parkWorkSQL, in.WorkID, in.WorkerID, in.FencingToken, int32(in.Attempt))
	if err != nil {
		return fmt.Errorf("postgres: StartDurableSignalWait: park work item: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: StartDurableSignalWait: work item %s: %w: %w", in.WorkID, engine.ErrStaleClaim, ErrStaleClaim)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE node_runs SET status = 'waiting_external', updated_at = now() WHERE id = $1`,
		in.NodeRunID,
	); err != nil {
		return fmt.Errorf("postgres: StartDurableSignalWait: mark node run waiting: %w", err)
	}

	if _, err := tx.Exec(ctx, insertSignalSubscriptionSQL,
		in.SubscriptionID, in.NamespaceID, in.RunID, in.NodeRunID, in.EventName,
	); err != nil {
		return fmt.Errorf("postgres: StartDurableSignalWait: persist subscription: %w", err)
	}

	if err := appendRunEventTx(ctx, tx, in.NamespaceID, in.RunID, TypeAttemptWaitingSignal, map[string]any{
		"run_id":          in.RunID,
		"node_run_id":     in.NodeRunID,
		"node_id":         in.NodeID,
		"attempt_id":      in.AttemptID,
		"work_id":         in.WorkID,
		"worker_id":       in.WorkerID,
		"fencing_token":   in.FencingToken,
		"attempt":         in.Attempt,
		"subscription_id": in.SubscriptionID,
		"event_name":      in.EventName,
	}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: StartDurableSignalWait: commit: %w", err)
	}
	return nil
}

const signalSubscriptionColumns = `id, namespace_id, run_id, node_run_id, event_name,
	status, fired_event_id, created_at, fired_at`

// signalRowScanner is satisfied by pgx.Row and pgx.Rows — the same shape as
// timers.go's timerRowScanner, defined again here so this file reads
// standalone.
type signalRowScanner interface {
	Scan(dest ...any) error
}

func scanSignalSubscription(row signalRowScanner) (SignalSubscription, error) {
	var (
		sub                SignalSubscription
		runID              pgtype.Text
		firedEventID       pgtype.Text
		createdAt, firedAt pgtype.Timestamptz
	)
	if err := row.Scan(
		&sub.ID, &sub.NamespaceID, &runID, &sub.NodeRunID, &sub.EventName,
		&sub.Status, &firedEventID, &createdAt, &firedAt,
	); err != nil {
		return SignalSubscription{}, err
	}
	sub.RunID = textOrEmpty(runID)
	sub.FiredEventID = textOrEmpty(firedEventID)
	sub.CreatedAt = tsValue(createdAt)
	sub.FiredAt = tsValue(firedAt)
	return sub, nil
}

// SignalSubscriptionByID loads one subscription row. It reports
// (zero, false, nil) rather than an error when no subscription has this id:
// the wait dispatcher's "has this node run's wait been armed yet" question
// treats absence as a legitimate answer, exactly like TimerByID.
func (s *Store) SignalSubscriptionByID(ctx context.Context, id string) (SignalSubscription, bool, error) {
	if id == "" {
		return SignalSubscription{}, false, fmt.Errorf("postgres: SignalSubscriptionByID: id is required")
	}
	sub, err := scanSignalSubscription(s.pool.QueryRow(ctx,
		`SELECT `+signalSubscriptionColumns+` FROM signal_subscriptions WHERE id = $1`, id))
	if err != nil {
		if isNoRows(err) {
			return SignalSubscription{}, false, nil
		}
		return SignalSubscription{}, false, fmt.Errorf("postgres: SignalSubscriptionByID: %w", err)
	}
	return sub, true, nil
}

// SignalEventByID loads one signal event fact. (zero, false, nil) when no
// event has this id.
func (s *Store) SignalEventByID(ctx context.Context, id string) (SignalEvent, bool, error) {
	if id == "" {
		return SignalEvent{}, false, fmt.Errorf("postgres: SignalEventByID: id is required")
	}
	var (
		ev        SignalEvent
		runID     pgtype.Text
		createdAt pgtype.Timestamptz
		payload   []byte
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, namespace_id, run_id, name, payload, emitter, created_at FROM signal_events WHERE id = $1`, id,
	).Scan(&ev.ID, &ev.NamespaceID, &runID, &ev.Name, &payload, &ev.Emitter, &createdAt)
	if err != nil {
		if isNoRows(err) {
			return SignalEvent{}, false, nil
		}
		return SignalEvent{}, false, fmt.Errorf("postgres: SignalEventByID: %w", err)
	}
	ev.RunID = textOrEmpty(runID)
	ev.Payload = jsonOrEmptyObject(payload)
	ev.CreatedAt = tsValue(createdAt)
	return ev, true, nil
}

// DeliverSignalEventInput is one inbound signal event.
type DeliverSignalEventInput struct {
	NamespaceID string
	// Name is the signal name subscriptions match on.
	Name string
	// Payload is the emitter's free-form event body. Defaults to "{}".
	Payload json.RawMessage
	// Emitter is free text naming who delivered the fact.
	Emitter string
	// RunID optionally scopes the event to one run: set, it resumes only
	// that run's matching subscriptions; empty, it resumes every matching
	// subscription in the namespace. The caller is responsible for having
	// verified a non-empty RunID exists in this namespace (the API handler
	// does) — an unknown id would only fail the FK on insert.
	RunID string
}

// selectCandidateSubscriptionsSQL is the unlocked first read of the pending
// subscriptions an event may resume — only good enough to learn WHICH runs'
// advisory locks to take. The authoritative read is
// selectLockedSubscriptionsSQL, re-run under those locks.
const selectCandidateSubscriptionsSQL = `
SELECT ` + signalSubscriptionColumns + `
FROM signal_subscriptions
WHERE namespace_id = $1 AND event_name = $2 AND status = 'pending'
  AND ($3::text IS NULL OR run_id = $3)
ORDER BY run_id, id
`

// selectLockedSubscriptionsSQL is the authoritative matching read, executed
// only after the candidate runs' advisory locks are held, and restricted to
// exactly those runs: every writer of a run's waiting state (the park, the
// cancel REAP, another delivery) takes the same advisory lock first, so
// this read is stable and the lock ORDER (advisory locks sorted by run id,
// THEN subscription rows) is identical across all of them — the inversion
// that could deadlock against cancelRun is structurally impossible. A
// subscription whose run was not in the candidate set (parked concurrently
// with this delivery) is deliberately NOT chased: that race has no defined
// order, and treating it as event-before-subscription (stays parked) is
// this pass's documented semantics either way. FOR UPDATE is defense in
// depth against any future writer that skips the advisory lock.
const selectLockedSubscriptionsSQL = `
SELECT ` + signalSubscriptionColumns + `
FROM signal_subscriptions
WHERE namespace_id = $1 AND event_name = $2 AND status = 'pending'
  AND ($3::text IS NULL OR run_id = $3)
  AND run_id = ANY($4)
ORDER BY run_id, id
FOR UPDATE
`

// DeliverSignalEvent commits one inbound signal event: append the
// signal_events fact, fire every matching pending subscription, and return
// each fired subscription's parked work item to 'ready' — all in one
// transaction (see the file doc comment for why the resume lives here and
// what stays the worker's). The event is appended even when nothing is
// waiting: an event is a fact, not an error, and a later subscriber
// deliberately does NOT retroactively fire on it (issue #43's documented
// limitation). It returns the appended event and the subscriptions it
// fired.
func (s *Store) DeliverSignalEvent(ctx context.Context, in DeliverSignalEventInput) (SignalEvent, []SignalSubscription, error) {
	switch {
	case in.NamespaceID == "":
		return SignalEvent{}, nil, errors.New("postgres: DeliverSignalEvent: namespaceID is required")
	case in.Name == "":
		return SignalEvent{}, nil, errors.New("postgres: DeliverSignalEvent: name is required")
	case in.Emitter == "":
		return SignalEvent{}, nil, errors.New("postgres: DeliverSignalEvent: emitter is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SignalEvent{}, nil, fmt.Errorf("postgres: DeliverSignalEvent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	fired, err := matchSubscriptionsUnderRunLocks(ctx, tx, in)
	if err != nil {
		return SignalEvent{}, nil, err
	}

	ev := SignalEvent{
		ID:          store.NewULID(),
		NamespaceID: in.NamespaceID,
		RunID:       in.RunID,
		Name:        in.Name,
		Payload:     jsonOrEmptyObject(in.Payload),
		Emitter:     in.Emitter,
	}
	var createdAt pgtype.Timestamptz
	if err := tx.QueryRow(ctx,
		`INSERT INTO signal_events (id, namespace_id, run_id, name, payload, emitter)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING created_at`,
		ev.ID, ev.NamespaceID, textOrNull(ev.RunID), ev.Name, ev.Payload, ev.Emitter,
	).Scan(&createdAt); err != nil {
		return SignalEvent{}, nil, fmt.Errorf("postgres: DeliverSignalEvent: append event: %w", err)
	}
	ev.CreatedAt = tsValue(createdAt)

	for i := range fired {
		if err := fireSubscription(ctx, tx, &fired[i], ev); err != nil {
			return SignalEvent{}, nil, err
		}
	}

	// One outbox row for the delivery itself, whether or not anything was
	// waiting — the same transactional audit discipline every other state
	// change in this store follows (appendRunEventTx, the API's cancelRun).
	deliveredPayload, _ := json.Marshal(map[string]any{
		"event_id": ev.ID,
		"name":     ev.Name,
		"emitter":  ev.Emitter,
		"run_id":   ev.RunID,
		"resumed":  len(fired),
	})
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox (id, namespace_id, topic, payload, status, available_at)
		 VALUES ($1, $2, $3, $4, 'pending', now())`,
		store.NewULID(), in.NamespaceID, TypeSignalDelivered, deliveredPayload,
	); err != nil {
		return SignalEvent{}, nil, fmt.Errorf("postgres: DeliverSignalEvent: outbox: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return SignalEvent{}, nil, fmt.Errorf("postgres: DeliverSignalEvent: commit: %w", err)
	}
	return ev, fired, nil
}

// matchSubscriptionsUnderRunLocks resolves the pending subscriptions this
// delivery fires: an unlocked candidate read to learn which runs are
// involved, the per-run advisory locks in sorted order (the same
// ledger.RunLockKey every other writer of those runs' waiting state and
// audit timeline takes — appendRunEventTx's MAX+1 sequence is only safe
// under it), then the authoritative re-read restricted to the locked runs.
// See selectLockedSubscriptionsSQL's doc comment for the lock-ordering
// reasoning.
func matchSubscriptionsUnderRunLocks(ctx context.Context, tx pgx.Tx, in DeliverSignalEventInput) ([]SignalSubscription, error) {
	candidates, err := querySubscriptions(ctx, tx, selectCandidateSubscriptionsSQL,
		in.NamespaceID, in.Name, textOrNull(in.RunID))
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	runIDs := sortedRunIDs(candidates)
	for _, runID := range runIDs {
		if _, err := tx.Exec(ctx, advisoryXactLockSQL, ledger.RunLockKey(runID)); err != nil {
			return nil, fmt.Errorf("postgres: DeliverSignalEvent: lock run %s: %w", runID, err)
		}
	}

	return querySubscriptions(ctx, tx, selectLockedSubscriptionsSQL,
		in.NamespaceID, in.Name, textOrNull(in.RunID), runIDs)
}

// querySubscriptions runs one of the matching selects and scans its rows.
func querySubscriptions(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]SignalSubscription, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: DeliverSignalEvent: match subscriptions: %w", err)
	}
	defer rows.Close()

	var matched []SignalSubscription
	for rows.Next() {
		sub, err := scanSignalSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: DeliverSignalEvent: scan subscription: %w", err)
		}
		matched = append(matched, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: DeliverSignalEvent: match subscriptions: %w", err)
	}
	return matched, nil
}

// fireSubscription marks one locked subscription fired by ev and returns
// its parked work item to 'ready'. The work-item UPDATE matches only
// `state = 'waiting'` — the one state the signal park
// (StartDurableSignalWait's parkWorkSQL) leaves the item in — never the
// broader `state <> 'completed'` the scheduler's wait/retry effect uses: a
// 'cancelled' row matched by that broader guard would be a dead run's work
// item resurrected and re-executed. Zero work-item rows affected is a
// legitimate no-op, not an error: run cancellation retires the work item
// and the subscription together (the API's cancelRun REAP step), and when
// a pending subscription nonetheless outlives that reap, this guard is
// what keeps its delivery an ack (subscription retired, event appended)
// rather than a resurrection — a dead run's rows are left dead.
func fireSubscription(ctx context.Context, tx pgx.Tx, sub *SignalSubscription, ev SignalEvent) error {
	firedAt := time.Now().UTC()
	if _, err := tx.Exec(ctx,
		`UPDATE signal_subscriptions
		 SET status = 'fired', fired_event_id = $2, fired_at = $3
		 WHERE id = $1 AND status = 'pending'`,
		sub.ID, ev.ID, firedAt,
	); err != nil {
		return fmt.Errorf("postgres: DeliverSignalEvent: fire subscription %s: %w", sub.ID, err)
	}
	sub.Status = SignalSubscriptionFired
	sub.FiredEventID = ev.ID
	sub.FiredAt = firedAt

	if _, err := tx.Exec(ctx,
		`UPDATE work_items
		 SET available_at = now(), state = 'ready', state_version = state_version + 1, updated_at = now()
		 WHERE node_run_id = $1 AND state = 'waiting'`,
		sub.NodeRunID,
	); err != nil {
		return fmt.Errorf("postgres: DeliverSignalEvent: make work item available for node run %s: %w", sub.NodeRunID, err)
	}

	return appendRunEventTx(ctx, tx, sub.NamespaceID, sub.RunID, TypeSignalResumed, map[string]any{
		"run_id":          sub.RunID,
		"node_run_id":     sub.NodeRunID,
		"subscription_id": sub.ID,
		"event_id":        ev.ID,
		"event_name":      ev.Name,
		"emitter":         ev.Emitter,
	})
}

// sortedRunIDs returns the distinct run ids of the matched subscriptions in
// sorted order — the advisory-lock acquisition order two concurrent
// deliveries must share.
func sortedRunIDs(subs []SignalSubscription) []string {
	seen := make(map[string]struct{}, len(subs))
	var ids []string
	for _, sub := range subs {
		if _, ok := seen[sub.RunID]; ok {
			continue
		}
		seen[sub.RunID] = struct{}{}
		ids = append(ids, sub.RunID)
	}
	sort.Strings(ids)
	return ids
}
