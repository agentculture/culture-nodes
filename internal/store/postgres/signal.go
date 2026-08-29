package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
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
// Delivery semantics. DeliverSignalEvent fires the subscriptions that are
// pending at append time and deliberately never scans event history: live
// delivery is a broadcast over what is waiting NOW, and keeping it that way
// is what makes it one indexed scan. Migration 0016 documented the gap that
// left — event-then-subscription stayed parked forever — and task t21 closed
// it from the OTHER side rather than by widening this scan: a wait that is
// about to park first asks signalreplay.go's ReplaySignalEvent whether the
// run has an unconsumed backlogged fact for that name (design D12's per-run,
// per-name cursor). Delivery therefore stays a broadcast, catch-up stays a
// per-subscriber cursor, and the append-only fact table is what lets the two
// coexist without a schema change to this pair of tables.
//
// Route pickup (eventroutes.go, design D9) rides the same transaction:
// after firing subscriptions, a delivery may also create tokens at the
// target nodes of active event_routes. It still completes nothing — the
// tokens it creates are new claimable work, exactly like CreateRun's.

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

	// Subject is the caller-supplied correlation key (task t15, spec
	// c31/h16), carried from DeliverSignalEventInput.Subject through this one
	// delivery call so runEventTriggers can build a PickupEvent with it. It
	// is DELIBERATELY not a signal_events column: the durable record of a
	// subject that mattered lives on the run it created or attached to
	// (runs.subject, migrations/0038) and on that run's own event stream
	// (TypeRunCreated / TypeTriggerEventAttached), not duplicated onto the
	// immutable fact row. A row read back from the database (SignalEventByID,
	// the duplicate-watermark path) therefore always reads Subject == "".
	Subject string
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
	// that run's matching subscriptions and routes; empty, it resumes every
	// matching subscription and route in the namespace. The caller is
	// responsible for having verified a non-empty RunID exists in this
	// namespace (the API handler does) — an unknown id would only fail the FK
	// on insert.
	RunID string
	// SourceKey and Watermark make discovery delivery idempotent. When both
	// equal the durable cursor from an earlier delivery, the existing event is
	// returned and no subscription, route, or trigger is run again.
	SourceKey string
	Watermark json.RawMessage

	// Subject is an optional correlation key (task t15, spec c31/h16) — e.g.
	// a Jira issue key. It has nothing to do with SourceKey/Watermark's
	// exact-redelivery idempotency: two DIFFERENT real events on the same
	// subject (a state change, then a comment) are two different facts with
	// two different watermarks, and both are meant to be appended. What
	// Subject changes is trigger creation: TriggerEvent attaches a
	// subject-bearing event to an already-ACTIVE run for the same
	// (workflow key, subject) instead of creating a sibling. Empty (the
	// default) reproduces the pre-task-t15 behavior exactly — every trigger
	// match creates a new run.
	Subject string

	// Pickup performs event-route pickup (issue #43, design D9) inside this
	// delivery's transaction. Nil disables route pickup entirely, which is
	// what every caller that only cares about signal waits wants — and what
	// keeps a delivery from silently depending on an engine it was not given.
	Pickup engine.EventPickupRunner
	// Trigger creates runs from matching handlers in the newest published
	// workflow versions, in this same delivery transaction.
	Trigger engine.EventTriggerRunner
}

// JiraHistoryWatermark is the cumulative cursor carried by each fact from
// the history-aware Jira emitter. Jira ids remain decimal strings on the
// wire; comparison below uses arbitrary precision integers.
type JiraHistoryWatermark struct {
	ChangelogID string `json:"changelog_id"`
	CommentID   string `json:"comment_id"`
}

// AdoptJiraHistoryHead records Jira's observed current head without
// appending a signal event. Migration 0041 cannot fill this value because
// Jira, not PostgreSQL, owns it. The host-side cutover-adopt command calls
// this before the history-aware sweep is allowed to resume.
func (s *Store) AdoptJiraHistoryHead(ctx context.Context, namespaceID, issueSourceKey string, watermark json.RawMessage) error {
	if namespaceID == "" || !jiraIssueSourceKey(issueSourceKey) {
		return errors.New("postgres: AdoptJiraHistoryHead requires namespace and jira:<site>:<issue> source key")
	}
	parsed, err := parseJiraHistoryWatermark(watermark)
	if err != nil {
		return fmt.Errorf("postgres: AdoptJiraHistoryHead: %w", err)
	}
	encoded, _ := json.Marshal(parsed)
	_, err = s.pool.Exec(ctx, `INSERT INTO jira_history_watermark_cutovers
		(namespace_id, issue_source_key, watermark, adopted_at)
		VALUES ($1,$2,$3,now())
		ON CONFLICT (namespace_id, issue_source_key) DO UPDATE SET
		watermark=EXCLUDED.watermark, adopted_at=EXCLUDED.adopted_at`, namespaceID, issueSourceKey, encoded)
	if err != nil {
		return fmt.Errorf("postgres: AdoptJiraHistoryHead: %w", err)
	}
	return nil
}

// PendingJiraHistoryCutover is one migration-seeded issue whose Jira head
// has not yet been adopted by the deployment one-shot.
type PendingJiraHistoryCutover struct {
	NamespaceID    string
	IssueSourceKey string
}

func (s *Store) ListPendingJiraHistoryCutovers(ctx context.Context) ([]PendingJiraHistoryCutover, error) {
	rows, err := s.pool.Query(ctx, `SELECT namespace_id, issue_source_key
		FROM jira_history_watermark_cutovers WHERE adopted_at IS NULL
		ORDER BY namespace_id, issue_source_key`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list pending Jira history cutovers: %w", err)
	}
	defer rows.Close()
	var pending []PendingJiraHistoryCutover
	for rows.Next() {
		var row PendingJiraHistoryCutover
		if err := rows.Scan(&row.NamespaceID, &row.IssueSourceKey); err != nil {
			return nil, fmt.Errorf("postgres: scan pending Jira history cutover: %w", err)
		}
		pending = append(pending, row)
	}
	return pending, rows.Err()
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
func (s *Store) DeliverSignalEvent(ctx context.Context, in DeliverSignalEventInput) (SignalDelivery, error) {
	if err := in.validate(); err != nil {
		return SignalDelivery{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SignalDelivery{}, fmt.Errorf("postgres: DeliverSignalEvent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	delivery, err := s.deliverSignalEventTx(ctx, tx, in)
	if err != nil {
		return SignalDelivery{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SignalDelivery{}, fmt.Errorf("postgres: DeliverSignalEvent: commit: %w", err)
	}
	return delivery, nil
}

func (in DeliverSignalEventInput) validate() error {
	switch {
	case in.NamespaceID == "":
		return errors.New("postgres: DeliverSignalEvent: namespaceID is required")
	case in.Name == "":
		return errors.New("postgres: DeliverSignalEvent: name is required")
	case in.Emitter == "":
		return errors.New("postgres: DeliverSignalEvent: emitter is required")
	case (in.SourceKey == "") != (len(in.Watermark) == 0):
		return errors.New("postgres: DeliverSignalEvent: sourceKey and watermark must be supplied together")
	}
	return nil
}

// deliverSignalEventTx is DeliverSignalEvent's whole body minus Begin and
// Commit, so a caller that is ALREADY inside a transaction can append a fact
// (and fire its waits, routes, and triggers) atomically with its own work.
// FireSchedule is that caller: acceptance criterion 2 of task t33 requires
// that a control plane which dies between deciding a schedule is due and
// creating the run can tell afterwards which of those happened, and the only
// way to make that answerable is to put the event, the run the trigger
// creates from it, and the schedule's own advanced cursor in ONE transaction.
// Splitting the seam here rather than duplicating the delivery is deliberate:
// there is one set of delivery semantics in this system and this keeps it
// that way.
//
// It does not commit and does not roll back: the caller owns tx's lifetime,
// including the rollback that undoes everything this wrote.
func (s *Store) deliverSignalEventTx(ctx context.Context, tx pgx.Tx, in DeliverSignalEventInput) (SignalDelivery, error) {
	if issueKey, ok := jiraHistoryIssueSourceKey(in.SourceKey); ok {
		suppressed, err := suppressPreCutoverJiraHistory(ctx, tx, in.NamespaceID, issueKey, in.Watermark)
		if err != nil {
			return SignalDelivery{}, err
		}
		if suppressed {
			return SignalDelivery{Duplicate: true, Suppressed: true}, nil
		}
	}
	if in.SourceKey != "" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			"signal-watermark:"+in.NamespaceID+":"+in.SourceKey); err != nil {
			return SignalDelivery{}, fmt.Errorf("postgres: DeliverSignalEvent: lock watermark: %w", err)
		}
		var eventID string
		err := tx.QueryRow(ctx, `SELECT event_id FROM signal_event_watermarks
			WHERE namespace_id = $1 AND source_key = $2 AND watermark = $3::jsonb
			FOR UPDATE`, in.NamespaceID, in.SourceKey, jsonOrEmptyObject(in.Watermark)).Scan(&eventID)
		if err == nil {
			ev, err := signalEventByIDTx(ctx, tx, eventID)
			if err != nil {
				return SignalDelivery{}, err
			}
			return SignalDelivery{Event: ev, Duplicate: true}, nil
		}
		if !isNoRows(err) {
			return SignalDelivery{}, fmt.Errorf("postgres: DeliverSignalEvent: read watermark: %w", err)
		}
	}

	fired, routes, err := matchUnderRunLocks(ctx, tx, in)
	if err != nil {
		return SignalDelivery{}, err
	}

	ev := SignalEvent{
		ID:          store.NewULID(),
		NamespaceID: in.NamespaceID,
		RunID:       in.RunID,
		Name:        in.Name,
		Payload:     jsonOrEmptyObject(in.Payload),
		Emitter:     in.Emitter,
		// In-memory only — see the field's doc comment for why this never
		// reaches the signal_events INSERT below.
		Subject: in.Subject,
	}
	var createdAt pgtype.Timestamptz
	if err := tx.QueryRow(ctx,
		`INSERT INTO signal_events (id, namespace_id, run_id, name, payload, emitter, source_key)
		 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7,''))
		 RETURNING created_at`,
		ev.ID, ev.NamespaceID, textOrNull(ev.RunID), ev.Name, ev.Payload, ev.Emitter, in.SourceKey,
	).Scan(&createdAt); err != nil {
		return SignalDelivery{}, fmt.Errorf("postgres: DeliverSignalEvent: append event: %w", err)
	}
	ev.CreatedAt = tsValue(createdAt)
	// A merged-PR fact is also the durable freeze transition for the ticket
	// projection. It is applied in the fact transaction, so consumers never
	// observe the merge without the corresponding frozen page.
	if in.Name == "pr.merged" {
		var merged struct {
			IssueKey string `json:"issue_key"`
		}
		if err := json.Unmarshal(ev.Payload, &merged); err != nil {
			return SignalDelivery{}, fmt.Errorf("postgres: DeliverSignalEvent: decode pr.merged: %w", err)
		}
		if merged.IssueKey == "" {
			return SignalDelivery{}, errors.New("postgres: DeliverSignalEvent: pr.merged issue_key is required")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ticket_freezes(namespace_id,ticket_id,merged_pr,frozen_by,signal_event_id)
			VALUES($1,$2,$3,'pr.merged',$4) ON CONFLICT(namespace_id,ticket_id) DO UPDATE SET
			merged_pr=EXCLUDED.merged_pr,frozen_by=EXCLUDED.frozen_by,signal_event_id=EXCLUDED.signal_event_id`, in.NamespaceID, merged.IssueKey, ev.Payload, ev.ID); err != nil {
			return SignalDelivery{}, fmt.Errorf("postgres: DeliverSignalEvent: freeze ticket: %w", err)
		}
	}
	if in.SourceKey != "" {
		if len(in.Watermark) == 0 {
			return SignalDelivery{}, errors.New("postgres: DeliverSignalEvent: watermark is required with sourceKey")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO signal_event_watermarks
			(namespace_id, source_key, watermark, event_id) VALUES ($1,$2,$3,$4)
			ON CONFLICT (namespace_id, source_key) DO UPDATE SET
			watermark = EXCLUDED.watermark, event_id = EXCLUDED.event_id, updated_at = now()`,
			in.NamespaceID, in.SourceKey, in.Watermark, ev.ID); err != nil {
			return SignalDelivery{}, fmt.Errorf("postgres: DeliverSignalEvent: advance watermark: %w", err)
		}
	}

	for i := range fired {
		if err := fireSubscription(ctx, tx, &fired[i], ev); err != nil {
			return SignalDelivery{}, err
		}
	}

	// Route pickup runs AFTER the fact exists, because a pickup token names
	// the event it came from (tokens.origin_event_id) and that is a real
	// foreign key, not a label.
	pickups, err := runEventRoutePickup(ctx, tx, in.NamespaceID, in.Pickup, routes, ev)
	if err != nil {
		return SignalDelivery{}, err
	}
	triggered, err := runEventTriggers(ctx, tx, in.NamespaceID, in.Trigger, ev)
	if err != nil {
		return SignalDelivery{}, err
	}

	// One outbox row for the delivery itself, whether or not anything was
	// waiting — the same transactional audit discipline every other state
	// change in this store follows (appendRunEventTx, the API's cancelRun).
	// subject reads back nil (JSON null) rather than "" when the delivery
	// carried none — omitting a `subject` key entirely would leave a reader
	// unable to tell "no subject" from "field predates this version of the
	// payload".
	var subject any
	if ev.Subject != "" {
		subject = ev.Subject
	}
	deliveredPayload, _ := json.Marshal(map[string]any{
		"event_id":  ev.ID,
		"name":      ev.Name,
		"emitter":   ev.Emitter,
		"run_id":    ev.RunID,
		"subject":   subject,
		"resumed":   len(fired),
		"picked_up": admittedPickups(pickups),
		"triggered": len(triggered),
	})
	if _, err := tx.Exec(ctx,
		`INSERT INTO outbox (id, namespace_id, topic, payload, status, available_at)
		 VALUES ($1, $2, $3, $4, 'pending', now())`,
		store.NewULID(), in.NamespaceID, TypeSignalDelivered, deliveredPayload,
	); err != nil {
		return SignalDelivery{}, fmt.Errorf("postgres: DeliverSignalEvent: outbox: %w", err)
	}

	return SignalDelivery{Event: ev, Fired: fired, Pickups: pickups, Triggered: triggered}, nil
}

// SignalDelivery is what one committed delivery did: the fact it appended,
// the parked subscriptions it fired, and the event routes it offered the fact
// to. Pickups includes REFUSED routes as well as admitted ones — a guard that
// declined and a bound that had no headroom are both answers the caller is
// entitled to see, and design D13 makes the refusal the only trace a
// bound-refused pickup leaves.
type SignalDelivery struct {
	Event     SignalEvent
	Fired     []SignalSubscription
	Pickups   []engine.EventPickupResult
	Triggered []engine.TriggeredRun
	Duplicate bool
	// Suppressed is a pre-cutover history fact adopted without emission,
	// rather than an exact redelivery of an already-appended event.
	Suppressed bool
}

func jiraIssueSourceKey(key string) bool {
	parts := strings.Split(key, ":")
	return len(parts) == 3 && parts[0] == "jira" && parts[1] != "" && parts[2] != ""
}

func jiraHistoryIssueSourceKey(key string) (string, bool) {
	parts := strings.Split(key, ":")
	if len(parts) != 6 || parts[0] != "jira" || parts[1] == "" || parts[2] == "" || parts[3] != "history" || (parts[4] != "changelog" && parts[4] != "comment") || parts[5] == "" {
		return "", false
	}
	return strings.Join(parts[:3], ":"), true
}

func parseJiraHistoryWatermark(raw json.RawMessage) (JiraHistoryWatermark, error) {
	var watermark JiraHistoryWatermark
	if err := json.Unmarshal(raw, &watermark); err != nil {
		return watermark, fmt.Errorf("invalid watermark: %w", err)
	}
	for _, value := range []string{watermark.ChangelogID, watermark.CommentID} {
		if value != "" {
			if _, ok := new(big.Int).SetString(value, 10); !ok {
				return watermark, errors.New("watermark ids must be empty or decimal strings")
			}
		}
	}
	return watermark, nil
}

func suppressPreCutoverJiraHistory(ctx context.Context, tx pgx.Tx, namespaceID, issueSourceKey string, raw json.RawMessage) (bool, error) {
	incoming, err := parseJiraHistoryWatermark(raw)
	if err != nil {
		return false, fmt.Errorf("postgres: DeliverSignalEvent: jira history watermark: %w", err)
	}
	var adoptedRaw []byte
	var isAdopted bool
	err = tx.QueryRow(ctx, `SELECT watermark, adopted_at IS NOT NULL FROM jira_history_watermark_cutovers
		WHERE namespace_id=$1 AND issue_source_key=$2`, namespaceID, issueSourceKey).Scan(&adoptedRaw, &isAdopted)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("postgres: DeliverSignalEvent: read jira cutover: %w", err)
	}
	// Fail closed: a pending marker means deployment resumed Jira history
	// delivery before the credential-isolated adopter completed. Refusing the
	// transaction makes the ordering violation loud and prevents replayed
	// history from creating billable runs; silently delivering or discarding
	// it would conceal an unsafe rollout.
	if !isAdopted {
		return false, errors.New("postgres: DeliverSignalEvent: Jira history cutover is pending; run nodes cutover-adopt before resuming sweeps")
	}
	adopted, err := parseJiraHistoryWatermark(adoptedRaw)
	if err != nil {
		return false, fmt.Errorf("postgres: DeliverSignalEvent: stored jira cutover: %w", err)
	}
	return decimalAtOrBefore(incoming.ChangelogID, adopted.ChangelogID) && decimalAtOrBefore(incoming.CommentID, adopted.CommentID), nil
}

func decimalAtOrBefore(value, head string) bool {
	if value == "" {
		return true
	}
	if head == "" {
		return false
	}
	v, _ := new(big.Int).SetString(value, 10)
	h, _ := new(big.Int).SetString(head, 10)
	return v.Cmp(h) <= 0
}

func signalEventByIDTx(ctx context.Context, tx pgx.Tx, id string) (SignalEvent, error) {
	var ev SignalEvent
	var runID pgtype.Text
	var createdAt pgtype.Timestamptz
	var payload []byte
	err := tx.QueryRow(ctx, `SELECT id, namespace_id, run_id, name, payload, emitter, created_at FROM signal_events WHERE id = $1`, id).
		Scan(&ev.ID, &ev.NamespaceID, &runID, &ev.Name, &payload, &ev.Emitter, &createdAt)
	if err != nil {
		return SignalEvent{}, fmt.Errorf("postgres: DeliverSignalEvent: load duplicate event: %w", err)
	}
	ev.RunID, ev.Payload, ev.CreatedAt = textOrEmpty(runID), jsonOrEmptyObject(payload), tsValue(createdAt)
	return ev, nil
}

func admittedPickups(pickups []engine.EventPickupResult) int {
	n := 0
	for _, p := range pickups {
		if p.Admitted {
			n++
		}
	}
	return n
}

// matchUnderRunLocks resolves everything this delivery will touch: the
// pending subscriptions it fires and the active event routes it offers the
// fact to. Both halves follow one two-phase discipline — an unlocked
// candidate read to learn which runs are involved, the per-run advisory locks
// in sorted order (the same ledger.RunLockKey every other writer of those
// runs' waiting state and audit timeline takes — appendRunEventTx's MAX+1
// sequence is only safe under it), then the authoritative re-reads restricted
// to the locked runs.
//
// The two candidate sets are UNIONED before any lock is taken, and that is
// the whole reason this function exists rather than two independent ones. A
// delivery that locked the subscription runs, then discovered a route in a
// third run and locked that too, would be acquiring locks in an order no
// other writer shares — the lock-order inversion selectLockedSubscriptionsSQL's
// doc comment explains is structurally impossible today. One sorted union
// keeps it impossible.
func matchUnderRunLocks(ctx context.Context, tx pgx.Tx, in DeliverSignalEventInput) ([]SignalSubscription, []engine.EventRoute, error) {
	candidates, err := querySubscriptions(ctx, tx, selectCandidateSubscriptionsSQL,
		in.NamespaceID, in.Name, textOrNull(in.RunID))
	if err != nil {
		return nil, nil, err
	}
	var routeRuns []string
	if in.Pickup != nil {
		if routeRuns, err = candidateEventRouteRuns(ctx, tx, in); err != nil {
			return nil, nil, err
		}
	}

	runIDs := sortedRunIDs(candidates, routeRuns)
	if len(runIDs) == 0 {
		return nil, nil, nil
	}
	for _, runID := range runIDs {
		if _, err := tx.Exec(ctx, advisoryXactLockSQL, ledger.RunLockKey(runID)); err != nil {
			return nil, nil, fmt.Errorf("postgres: DeliverSignalEvent: lock run %s: %w", runID, err)
		}
	}

	fired, err := querySubscriptions(ctx, tx, selectLockedSubscriptionsSQL,
		in.NamespaceID, in.Name, textOrNull(in.RunID), runIDs)
	if err != nil {
		return nil, nil, err
	}
	var routes []engine.EventRoute
	if in.Pickup != nil {
		if routes, err = lockedEventRoutes(ctx, tx, in, runIDs); err != nil {
			return nil, nil, err
		}
	}
	return fired, routes, nil
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

// sortedRunIDs returns the distinct run ids a delivery will touch — the
// subscriptions' runs and the routes' runs together — in sorted order, which
// is the advisory-lock acquisition order two concurrent deliveries must
// share.
func sortedRunIDs(subs []SignalSubscription, extra []string) []string {
	seen := make(map[string]struct{}, len(subs)+len(extra))
	var ids []string
	add := func(runID string) {
		if runID == "" {
			return
		}
		if _, ok := seen[runID]; ok {
			return
		}
		seen[runID] = struct{}{}
		ids = append(ids, runID)
	}
	for _, sub := range subs {
		add(sub.RunID)
	}
	for _, runID := range extra {
		add(runID)
	}
	sort.Strings(ids)
	return ids
}
