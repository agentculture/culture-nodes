package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// Replay / catch-up for signal waits (issue #43 task t21; the parallel-tokens
// design §6.3, decision D12). This file closes the gap migration 0016
// documented and signal.go's doc comment restates: subscription-then-event
// resumes, event-then-subscription stays parked forever.
//
// The fix is a per-run, per-name CURSOR over the append-only fact table
// rather than a new cursor column anywhere: the run's own already-`fired`
// subscriptions ARE the cursor. At park time, before a subscription is armed,
// the dispatcher asks for the OLDEST matching fact that satisfies all three
// of:
//
//  1. namespace and name match, and the event's optional run_id scope admits
//     this run (NULL, or equal);
//  2. the fact is not older than the run itself — a run catches up on its own
//     lifetime, never on months of history it was never part of;
//  3. the fact is newer than the newest fact already fired to THIS run for
//     THIS name.
//
// Two review findings shape (2) and (3), and both are honoured literally:
//
//   - R5 — a run may hold SEVERAL subscriptions to one name (0016 deliberately
//     put no uniqueness on (run_id, event_name)), so the cursor is the maximum
//     across ALL of that run's fired subscriptions for the name, not whichever
//     one happens to be read first. cursorSQL below is that maximum, ordered
//     over the total (created_at, id) order rather than created_at alone so
//     two facts appended in the same instant still have one answer.
//   - R6 — the run-creation floor is `>=`, not `>`. An event delivered in the
//     same instant the run was created is admitted. The exclusion `>` would
//     buy is a fact that predates the run by less than the clock's resolution
//     — and the two timestamps do not even come from the same clock
//     (runs.created_at is the engine's Go clock, signal_events.created_at is
//     the database's now()), so a strict floor would silently drop events the
//     run genuinely should see. Admitting the tie is the honest direction: the
//     worst case is one extra fact from the instant of birth, where the worst
//     case of `>` is a permanently missed resume.
//
// Why a cursor at all, and what asymmetry it buys (design §6.3, stated so it
// is not discovered later as a surprise): live delivery is BROADCAST — one
// event fires every pending subscription — while replay is per-subscriber
// CATCH-UP that advances the run's cursor. Two waiters in one run subscribing
// late to the same name therefore consume two DIFFERENT backlogged facts,
// where subscribing early would have had one fact fire both. The alternative,
// replaying the newest matching fact to every late subscriber, restores the
// symmetry but makes a loop that re-parks on the same name each iteration
// re-fire on the same old event forever — a hot loop bounded only by
// maxVisitsPerNode. Monotonic catch-up is the choice; open item O6 records
// that it should be revalidated against the first real multi-waiter workflow.
//
// Event ROUTES (eventroutes.go) deliberately do not replay: a route exists
// only from run creation onward, so "start observing" never means "re-execute
// history".

// TypeSignalReplayed is the audit event a catch-up resume appends. It is
// deliberately distinct from TypeSignalResumed: an operator reading a run's
// timeline should be able to tell "an event arrived while we waited" from
// "we subscribed late and consumed a fact that was already on the table".
const TypeSignalReplayed = "dev.culture.nodes.signal.replayed"

// ReplaySignalEventInput asks whether a wait that is about to park can
// instead resume immediately from a fact that already exists. It carries the
// same identity StartDurableSignalWaitInput does, minus the fencing tuple:
// nothing here parks or releases a work item, so there is no claim to fence.
// The subscription id is derived from the node run (the worker's
// signalSubscriptionID), which is what makes the insert below safe against a
// concurrent duplicate dispatch — two workers racing on the same node run
// race for one primary key, and the loser adopts the winner's answer instead
// of consuming a second fact.
//
// The namespace and the run-creation floor are NOT parameters: both are read
// from the run row inside the transaction, because both are the run's own
// facts and a caller-supplied copy could only ever agree or be wrong.
type ReplaySignalEventInput struct {
	RunID     string
	NodeRunID string
	NodeID    string
	AttemptID string

	SubscriptionID string
	EventName      string
}

// cursorSQL is R5's maximum: the newest fact already fired to this run for
// this name, across EVERY fired subscription the run holds for it. Ordered by
// (created_at, id) rather than created_at alone so the answer is total even
// when two facts share a timestamp.
const cursorSQL = `
	SELECT fe.created_at AS ts, fe.id AS eid
	FROM signal_subscriptions AS ss
	JOIN signal_events AS fe ON fe.id = ss.fired_event_id
	WHERE ss.run_id = $2 AND ss.event_name = $3 AND ss.status = 'fired'
	ORDER BY fe.created_at DESC, fe.id DESC
	LIMIT 1
`

// selectReplayableEventSQL is the catch-up probe: the oldest fact this run
// has not yet consumed for this name. signal_events_replay_idx
// (migrations/0021) serves the (namespace_id, name, created_at) range. $4 is
// the run-creation floor, read from the run row in the same transaction, and
// the comparison is `>=` — review finding R6, argued in the file doc above.
const selectReplayableEventSQL = `
WITH cur AS (` + cursorSQL + `)
SELECT e.id, e.namespace_id, e.run_id, e.name, e.payload, e.emitter, e.created_at
FROM signal_events AS e
WHERE e.namespace_id = $1
  AND e.name = $3
  AND (e.run_id IS NULL OR e.run_id = $2)
  AND e.created_at >= $4
  AND (
        NOT EXISTS (SELECT 1 FROM cur)
        OR (e.created_at, e.id) > ((SELECT ts FROM cur), (SELECT eid FROM cur))
      )
ORDER BY e.created_at, e.id
LIMIT 1
`

// insertFiredSubscriptionSQL arms a subscription that is already satisfied.
// ON CONFLICT DO NOTHING for insertSignalSubscriptionSQL's reason turned
// inside out: if a row already exists under this id, another dispatch of the
// same node run got here first and ITS answer is the one that counts —
// consuming a second fact for one node run would double-spend the cursor.
const insertFiredSubscriptionSQL = `
INSERT INTO signal_subscriptions (id, namespace_id, run_id, node_run_id, event_name, status, fired_event_id, fired_at)
VALUES ($1, $2, $3, $4, $5, 'fired', $6, $7)
ON CONFLICT (id) DO NOTHING
RETURNING id
`

// ReplaySignalEvent resolves a signal wait against facts that already exist.
//
// It reports (subscription, event, true, nil) when a backlogged fact was
// claimed: the subscription row is committed already `fired`, so the dispatch
// that called this completes the node run through the ordinary §12.5
// transaction — outcome `completed`, the event folded into the output, §9.7
// loop bounds enforced by the completion exactly as for a live delivery.
// Nothing is parked and no work item is touched, so the caller keeps the
// claim it already holds.
//
// It reports (zero, zero, false, nil) when there is nothing to catch up on,
// which is the ordinary answer: the caller parks as it always did.
//
// Committing the fired subscription in its own transaction rather than inside
// the park is safe and deliberate. The claim is untouched, so there is no
// fencing tuple to honour; a crash between this commit and the completion
// leaves a fired subscription that the re-dispatch finds and completes with
// the same event (dispatchSignalWait's existing `fired` branch), which is
// idempotent; and the run's advisory lock — the same one delivery and every
// completion take — serialises this against a concurrent live delivery, so a
// fact can never be both broadcast to this subscription and replayed into it.
func (s *Store) ReplaySignalEvent(ctx context.Context, in ReplaySignalEventInput) (SignalSubscription, SignalEvent, bool, error) {
	switch {
	case in.RunID == "" || in.NodeRunID == "":
		return SignalSubscription{}, SignalEvent{}, false, errors.New("postgres: ReplaySignalEvent: runID and nodeRunID are required")
	case in.SubscriptionID == "":
		return SignalSubscription{}, SignalEvent{}, false, errors.New("postgres: ReplaySignalEvent: subscriptionID is required")
	case in.EventName == "":
		return SignalSubscription{}, SignalEvent{}, false, errors.New("postgres: ReplaySignalEvent: eventName is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SignalSubscription{}, SignalEvent{}, false, fmt.Errorf("postgres: ReplaySignalEvent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	// The same per-run advisory lock delivery, the park, and every completion
	// take, so a catch-up cannot interleave with a live delivery of the same
	// name into the same run.
	if _, err := tx.Exec(ctx, advisoryXactLockSQL, ledger.RunLockKey(in.RunID)); err != nil {
		return SignalSubscription{}, SignalEvent{}, false,
			fmt.Errorf("postgres: ReplaySignalEvent: lock run %s: %w", in.RunID, err)
	}

	// The run row carries both facts the probe needs: which namespace it
	// belongs to and when it began (the R6 floor).
	var (
		namespaceID  string
		runCreatedAt pgtype.Timestamptz
	)
	err = tx.QueryRow(ctx, `SELECT namespace_id, created_at FROM runs WHERE id = $1`, in.RunID).
		Scan(&namespaceID, &runCreatedAt)
	if err != nil {
		if isNoRows(err) {
			// No run, nothing to catch up on. The caller's park will fail on
			// its own foreign keys if this is genuinely a bad id.
			return SignalSubscription{}, SignalEvent{}, false, nil
		}
		return SignalSubscription{}, SignalEvent{}, false,
			fmt.Errorf("postgres: ReplaySignalEvent: read run %s: %w", in.RunID, err)
	}

	ev, found, err := scanReplayCandidate(ctx, tx, in, namespaceID, runCreatedAt)
	if err != nil || !found {
		return SignalSubscription{}, SignalEvent{}, false, err
	}

	firedAt := time.Now().UTC()
	var claimedID string
	err = tx.QueryRow(ctx, insertFiredSubscriptionSQL,
		in.SubscriptionID, namespaceID, in.RunID, in.NodeRunID, in.EventName, ev.ID, firedAt,
	).Scan(&claimedID)
	switch {
	case isNoRows(err):
		// Another dispatch of this node run armed the subscription first.
		// Adopt whatever it decided rather than consuming a second fact.
		return adoptExistingSubscription(ctx, tx, in)
	case err != nil:
		return SignalSubscription{}, SignalEvent{}, false,
			fmt.Errorf("postgres: ReplaySignalEvent: arm fired subscription: %w", err)
	}

	sub := SignalSubscription{
		ID:           in.SubscriptionID,
		NamespaceID:  namespaceID,
		RunID:        in.RunID,
		NodeRunID:    in.NodeRunID,
		EventName:    in.EventName,
		Status:       SignalSubscriptionFired,
		FiredEventID: ev.ID,
		CreatedAt:    firedAt,
		FiredAt:      firedAt,
	}

	if err := appendRunEventTx(ctx, tx, namespaceID, in.RunID, TypeSignalReplayed, map[string]any{
		"run_id":          in.RunID,
		"node_run_id":     in.NodeRunID,
		"node_id":         in.NodeID,
		"attempt_id":      in.AttemptID,
		"subscription_id": in.SubscriptionID,
		"event_id":        ev.ID,
		"event_name":      ev.Name,
		"emitter":         ev.Emitter,
		// The fact predates the subscription — that is the whole point of a
		// catch-up, and an operator reading the timeline should see it stated
		// rather than infer it from timestamps.
		"event_created_at": ev.CreatedAt.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return SignalSubscription{}, SignalEvent{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return SignalSubscription{}, SignalEvent{}, false, fmt.Errorf("postgres: ReplaySignalEvent: commit: %w", err)
	}
	return sub, ev, true, nil
}

// scanReplayCandidate runs the catch-up probe for one (run, name), floored at
// the run's own creation.
func scanReplayCandidate(
	ctx context.Context, tx pgx.Tx, in ReplaySignalEventInput,
	namespaceID string, runCreatedAt pgtype.Timestamptz,
) (SignalEvent, bool, error) {
	var (
		ev        SignalEvent
		runID     pgtype.Text
		payload   []byte
		createdAt pgtype.Timestamptz
	)
	err := tx.QueryRow(ctx, selectReplayableEventSQL, namespaceID, in.RunID, in.EventName, runCreatedAt).
		Scan(&ev.ID, &ev.NamespaceID, &runID, &ev.Name, &payload, &ev.Emitter, &createdAt)
	if err != nil {
		if isNoRows(err) {
			return SignalEvent{}, false, nil
		}
		return SignalEvent{}, false, fmt.Errorf("postgres: ReplaySignalEvent: scan backlog: %w", err)
	}
	ev.RunID = textOrEmpty(runID)
	ev.Payload = jsonOrEmptyObject(payload)
	ev.CreatedAt = tsValue(createdAt)
	return ev, true, nil
}

// adoptExistingSubscription resolves the insert-conflict case: a subscription
// under this id already exists, so this dispatch adopts it instead of
// claiming a fact of its own. A row that is already `fired` is returned as a
// replay (the caller completes with the event that fired it, which is exactly
// what the winning dispatch will do too); anything else answers "nothing to
// replay", and the caller parks — where the existing pending row is the one
// that will be fired.
func adoptExistingSubscription(ctx context.Context, tx pgx.Tx, in ReplaySignalEventInput) (SignalSubscription, SignalEvent, bool, error) {
	sub, err := scanSignalSubscription(tx.QueryRow(ctx,
		`SELECT `+signalSubscriptionColumns+` FROM signal_subscriptions WHERE id = $1`, in.SubscriptionID))
	if err != nil {
		if isNoRows(err) {
			// The conflicting row vanished, which nothing in this codebase
			// does; answer "park" rather than invent a resume.
			return SignalSubscription{}, SignalEvent{}, false, nil
		}
		return SignalSubscription{}, SignalEvent{}, false,
			fmt.Errorf("postgres: ReplaySignalEvent: read existing subscription: %w", err)
	}
	if sub.Status != SignalSubscriptionFired || sub.FiredEventID == "" {
		return SignalSubscription{}, SignalEvent{}, false, nil
	}

	var (
		ev        SignalEvent
		runID     pgtype.Text
		payload   []byte
		createdAt pgtype.Timestamptz
	)
	err = tx.QueryRow(ctx,
		`SELECT id, namespace_id, run_id, name, payload, emitter, created_at FROM signal_events WHERE id = $1`,
		sub.FiredEventID,
	).Scan(&ev.ID, &ev.NamespaceID, &runID, &ev.Name, &payload, &ev.Emitter, &createdAt)
	if err != nil {
		return SignalSubscription{}, SignalEvent{}, false,
			fmt.Errorf("postgres: ReplaySignalEvent: read adopted event %s: %w", sub.FiredEventID, err)
	}
	ev.RunID = textOrEmpty(runID)
	ev.Payload = jsonOrEmptyObject(payload)
	ev.CreatedAt = tsValue(createdAt)

	if err := tx.Commit(ctx); err != nil {
		return SignalSubscription{}, SignalEvent{}, false, fmt.Errorf("postgres: ReplaySignalEvent: commit: %w", err)
	}
	return sub, ev, true, nil
}
