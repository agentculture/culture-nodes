package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The production WaitDispatcher (issue #39, PRD §9.2, §12.7): a `wait`
// node's until.duration / until.timestamp becomes a durable timer, and its
// until.signal a durable event subscription (task t10, spec decision c35) —
// never a held lease or a sleeping goroutine.
//
// The life of a timer wait, end to end:
//
//  1. First dispatch: the fire time is computed from the until block. If it
//     is already past, the node completes immediately with its kind-implied
//     `completed` outcome (internal/compiler/vocabulary.go's impliedOutcomes)
//     and the run routes onward — no park for a wait that is already over.
//  2. Otherwise the dispatch parks: Store.StartDurableWait releases the
//     lease (fenced, exactly like an async actor's §12.6 park), marks the
//     node run waiting_external, and persists a §12.7 wait timer bound to
//     the node run. The worker then holds NOTHING — no lease, no goroutine —
//     and may exit; the run is visibly parked (waiting_external on the run
//     detail surface) and the pending timer is what will wake it.
//  3. The scheduler fires the timer (internal/scheduler.applyEffect's
//     TimerKindWait effect): the work item returns to 'ready' in the same
//     transaction that marks the timer fired.
//  4. A worker claims the item again and re-dispatches the node. The
//     dispatcher finds the fired timer and completes the attempt with the
//     `completed` outcome through the engine's ordinary §12.5 completion —
//     which is what re-enters planTransition and keeps the §9.7 loop bounds
//     (maxTransitions, maxVisitsPerNode, maxDuration) enforced across the
//     park: a wait inside a cycle is bounded exactly like any other node.
//
// A signal wait is the same walk with the timer swapped for a first-class
// event subscription and the scheduler swapped for the inbound delivery
// route:
//
//  1. First dispatch parks: Store.StartDurableSignalWait — the identical
//     fenced release and waiting_external node run, with a pending
//     signal_subscriptions row (keyed deterministically off the node run,
//     like waitTimerID) where the timer would be, and deliberately NO
//     deadline: an undelivered signal leaves the run parked and
//     inspectable, never timed out by a dispatch default.
//  2. POST /v1alpha1/events (authenticated, internal/api) delivers a named
//     event: Store.DeliverSignalEvent appends the event fact, marks every
//     matching pending subscription fired, and returns the parked work
//     items to 'ready' in one transaction — the exact effect a timer fire
//     applies, performed by the delivery transaction instead of the
//     scheduler (see signal.go's doc comment for the single-writer
//     reasoning).
//  3. The re-claimed dispatch finds the fired subscription and completes
//     with the `completed` outcome through the same §12.5 completion as a
//     fired timer — planTransition, loop bounds and all. The resuming
//     event (id, name, emitter, payload) is folded into the node's output,
//     so downstream bindings can read what actually woke the run.
//
// An event with no waiting subscription is still appended — a fact, not an
// error — and since task t21 a subscription created AFTER its event no longer
// stays parked forever: step 1 first asks Store.ReplaySignalEvent whether the
// run has an unconsumed backlogged fact for this name (design D12's per-run,
// per-name cursor), and resumes immediately from it instead of arming a wait
// for an event that has been and gone. Only when there is nothing to catch up
// on does the dispatch park. The cursor is what keeps catch-up monotonic: a
// loop re-parking on one name consumes the backlog one fact per iteration,
// oldest first, never the same fact twice. See
// internal/store/postgres/signalreplay.go for the floor, the cursor, and the
// broadcast-versus-catch-up asymmetry they create.

// waitOutcome is the kind-implied domain outcome every wait node declares
// (internal/compiler/vocabulary.go's impliedOutcomes[KindWait]).
const waitOutcome = "completed"

// waitTimerID derives the durable wait timer's id from the node run it
// wakes. Deterministic on purpose: a crashed-and-redispatched arm re-adopts
// the same timer (StartDurableWait's ON CONFLICT DO NOTHING keeps the
// original fire_at anchor), and the resumed dispatch after the fire finds
// the fired row under the same id without a search.
func waitTimerID(nodeRunID string) string { return "wait-" + nodeRunID }

// signalSubscriptionID derives the durable signal subscription's id from
// the node run it wakes — waitTimerID's signal-kind twin, deterministic for
// the same two reasons: a crashed-and-redispatched arm re-adopts the same
// subscription (StartDurableSignalWait's ON CONFLICT DO NOTHING), and the
// resumed dispatch finds the fired row under the same id without a search.
func signalSubscriptionID(nodeRunID string) string { return "signal-" + nodeRunID }

// untilSpec is the worker's decoded view of a wait node's until block,
// mirroring internal/compiler's identically-shaped `until` type (model.go).
type untilSpec struct {
	Duration  string `json:"duration"`
	Timestamp string `json:"timestamp"`
	Signal    string `json:"signal"`
}

// TimerWaitDispatcher is the timer-backed WaitDispatcher every worker gets
// by default (worker.New wires one when Options.Waiter is nil). It holds a
// store handle and a clock and nothing else: the fencing-guarded park itself
// stays in the worker (parkWait), which is the only place the claim's
// fencing tuple legitimately lives.
type TimerWaitDispatcher struct {
	db  *postgres.Store
	now func() time.Time
}

// NewTimerWaitDispatcher returns the production wait dispatcher. now
// defaults to the UTC wall clock when nil.
func NewTimerWaitDispatcher(db *postgres.Store, now func() time.Time) *TimerWaitDispatcher {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &TimerWaitDispatcher{db: db, now: now}
}

// DispatchWait implements WaitDispatcher for until.duration,
// until.timestamp, and until.signal. See the file doc comment for the full
// arm → park → deliver/fire → resume walk-through of both wait kinds.
func (t *TimerWaitDispatcher) DispatchWait(ctx context.Context, dc DispatchContext, until json.RawMessage) (SeamResult, error) {
	spec, err := decodeUntil(until)
	if err != nil {
		return SeamResult{}, err
	}
	if spec.Signal != "" {
		return t.dispatchSignalWait(ctx, dc, until, spec.Signal)
	}

	now := t.now().UTC()
	timerID := waitTimerID(dc.NodeRunID)

	// The candidate fire time is computed — and the until block's syntax
	// therefore fully validated — before the timer lookup, so a malformed
	// duration or timestamp is diagnosed identically on every dispatch. It
	// is only the ANSWER for a wait that has not been armed yet; once a
	// timer row exists, its recorded fire_at is the anchor (see below).
	fireAt, err := waitFireAt(spec, now)
	if err != nil {
		return SeamResult{}, err
	}

	// The persisted timer — not a recomputation — is the authority on
	// whether the wait has elapsed. This matters twice over: a
	// until.duration recomputed from `now` on every dispatch would re-arm
	// forever, and a fired timer must complete the resume even if this
	// worker's clock runs behind the scheduler's (a skewed re-park over an
	// already-fired timer would wedge the run — nothing would ever make the
	// work item ready again).
	timer, found, err := t.db.TimerByID(ctx, timerID)
	if err != nil {
		return SeamResult{}, fmt.Errorf("load wait timer %s: %w", timerID, err)
	}
	if found {
		switch timer.Status {
		case postgres.TimerStatusFired:
			return waitCompleted(until, now), nil
		case postgres.TimerStatusCanceled:
			// Run cancellation retires the timer AND the work item in one
			// transaction, so a canceled timer under a still-claimable work
			// item is a state this system never writes. Refuse rather than
			// guess: completing would invent an outcome for a wait that was
			// told to stop, and re-parking would arm a wait nothing will fire.
			return SeamResult{}, fmt.Errorf(
				"wait timer %s is canceled but the work item was still dispatched; "+
					"run cancellation retires both together, so this work item is stale — refusing to guess an outcome", timerID)
		default:
			// Pending: the work item came back before its timer fired — an
			// anomalous early wake (nothing in this codebase produces one, but
			// a hand-poked row can). If the recorded fire time has passed,
			// complete: the eventual fire against a completed item is the
			// scheduler's documented no-op. Otherwise re-park on the original
			// anchor.
			if !timer.FireAt.After(now) {
				return waitCompleted(until, now), nil
			}
			return SeamResult{Async: true, AsyncRef: timerID, AsyncDeadline: timer.FireAt}, nil
		}
	}

	if !fireAt.After(now) {
		return waitCompleted(until, now), nil
	}
	return SeamResult{Async: true, AsyncRef: timerID, AsyncDeadline: fireAt}, nil
}

// dispatchSignalWait is DispatchWait's until.signal half. The persisted
// subscription — not any recomputation — is the authority on whether the
// signal has arrived, for the same reason the timer row is on the timer
// path: a fired subscription must complete the resume, and a pending one
// must re-park on the ORIGINAL subscription rather than arming a second.
func (t *TimerWaitDispatcher) dispatchSignalWait(ctx context.Context, dc DispatchContext, until json.RawMessage, signalName string) (SeamResult, error) {
	subID := signalSubscriptionID(dc.NodeRunID)
	sub, found, err := t.db.SignalSubscriptionByID(ctx, subID)
	if err != nil {
		return SeamResult{}, fmt.Errorf("load signal subscription %s: %w", subID, err)
	}
	if !found {
		// First dispatch. Before parking, ask whether the fact this wait is
		// waiting for has ALREADY been delivered — issue #43's replay half
		// (design D12, internal/store/postgres/signalreplay.go). Store
		// .ReplaySignalEvent commits an already-`fired` subscription when the
		// run has an unconsumed backlogged fact for this name, and this
		// dispatch then completes immediately through the ordinary §12.5
		// transaction instead of parking on an event that has been and gone.
		sub, ev, replayed, err := t.db.ReplaySignalEvent(ctx, postgres.ReplaySignalEventInput{
			RunID:          dc.RunID,
			NodeRunID:      dc.NodeRunID,
			NodeID:         dc.NodeID,
			AttemptID:      dc.AttemptID,
			SubscriptionID: subID,
			EventName:      signalName,
		})
		if err != nil {
			return SeamResult{}, fmt.Errorf("replay signal %q for node run %s: %w", signalName, dc.NodeRunID, err)
		}
		if replayed {
			return t.completedFromEvent(until, sub, ev), nil
		}
		// Nothing to catch up on: park. parkWait routes an AsyncSignal answer
		// to StartDurableSignalWait, which persists the subscription under the
		// fencing tuple only the worker legitimately holds.
		return SeamResult{Async: true, AsyncRef: subID, AsyncSignal: signalName}, nil
	}

	switch sub.Status {
	case postgres.SignalSubscriptionFired:
		return t.signalWaitCompleted(ctx, until, sub)
	case postgres.SignalSubscriptionCanceled:
		// Run cancellation retires the subscription AND the work item in one
		// transaction (the API's cancelRun REAP step), so — exactly like a
		// canceled timer above — a canceled subscription under a
		// still-claimable work item is a state this system never writes.
		return SeamResult{}, fmt.Errorf(
			"signal subscription %s is canceled but the work item was still dispatched; "+
				"run cancellation retires both together, so this work item is stale — refusing to guess an outcome", subID)
	default:
		// Pending: an anomalous early wake (nothing in this codebase
		// produces one, but a hand-poked row can). Re-park on the original
		// subscription — the eventual delivery is what completes it.
		return SeamResult{Async: true, AsyncRef: subID, AsyncSignal: signalName}, nil
	}
}

// signalWaitCompleted builds the completion for a signal wait whose
// subscription has fired: the kind-implied `completed` outcome, with the
// resuming event folded into the output so downstream bindings
// (/nodes/<id>/output) can read what actually woke the run.
func (t *TimerWaitDispatcher) signalWaitCompleted(ctx context.Context, until json.RawMessage, sub postgres.SignalSubscription) (SeamResult, error) {
	ev, found, err := t.db.SignalEventByID(ctx, sub.FiredEventID)
	if err != nil {
		return SeamResult{}, fmt.Errorf("load signal event %s: %w", sub.FiredEventID, err)
	}
	if !found {
		// A fired subscription always records the event that fired it in the
		// same transaction (DeliverSignalEvent), and signal_events is
		// append-only — so a missing row is corruption, not a race.
		return SeamResult{}, fmt.Errorf(
			"signal subscription %s is fired by event %s, but no such event exists; "+
				"refusing to complete a wait on evidence that cannot be read", sub.ID, sub.FiredEventID)
	}

	return t.completedFromEvent(until, sub, ev), nil
}

// completedFromEvent builds the `completed` SeamResult for a signal wait that
// a named event satisfied — whether the event arrived while the run was
// parked (live delivery) or was already on the table when the wait was first
// dispatched (replay, design D12). The output is identical either way on
// purpose: a downstream binding reads what woke the run, not how the control
// plane happened to notice.
func (t *TimerWaitDispatcher) completedFromEvent(until json.RawMessage, sub postgres.SignalSubscription, ev postgres.SignalEvent) SeamResult {
	payload := struct {
		Until       json.RawMessage `json:"until"`
		CompletedAt string          `json:"completed_at"`
		Event       signalEventOut  `json:"event"`
	}{
		Until:       until,
		CompletedAt: sub.FiredAt.UTC().Format(time.RFC3339Nano),
		Event: signalEventOut{
			ID:      ev.ID,
			Name:    ev.Name,
			Emitter: ev.Emitter,
			Payload: ev.Payload,
		},
	}
	// A struct of raw JSON and strings never fails to marshal.
	output, _ := json.Marshal(payload)
	return SeamResult{TechStatus: engine.StatusSucceeded, Outcome: waitOutcome, Output: output}
}

// signalEventOut is the resuming event's shape inside a signal wait's
// output payload.
type signalEventOut struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Emitter string          `json:"emitter"`
	Payload json.RawMessage `json:"payload"`
}

// decodeUntil validates the until block down to exactly one supported
// resume condition: duration, timestamp, or signal.
func decodeUntil(until json.RawMessage) (untilSpec, error) {
	if len(until) == 0 {
		return untilSpec{}, errors.New("wait node carries no until block; the compiler requires one, so this pinned definition is corrupt")
	}
	var spec untilSpec
	if err := json.Unmarshal(until, &spec); err != nil {
		return untilSpec{}, fmt.Errorf("until block could not be decoded: %w", err)
	}
	declared := 0
	if spec.Duration != "" {
		declared++
	}
	if spec.Timestamp != "" {
		declared++
	}
	if spec.Signal != "" {
		declared++
	}
	if declared != 1 {
		return untilSpec{}, fmt.Errorf(
			"until block must declare exactly one resume condition, got %d of duration/timestamp/signal; "+
				"declare exactly one of them", declared)
	}
	return spec, nil
}

// waitFireAt computes the absolute fire time a fresh (not-yet-armed) wait
// resolves to: now + duration, or the declared timestamp.
func waitFireAt(spec untilSpec, now time.Time) (time.Time, error) {
	if spec.Duration != "" {
		d, err := time.ParseDuration(spec.Duration)
		if err != nil {
			return time.Time{}, fmt.Errorf("until.duration %q is not a duration: %w", spec.Duration, err)
		}
		return now.Add(d), nil
	}
	ts, err := time.Parse(time.RFC3339, spec.Timestamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("until.timestamp %q is not an RFC 3339 timestamp: %w", spec.Timestamp, err)
	}
	return ts.UTC(), nil
}

// waitCompleted is the SeamResult a wait that has elapsed completes with:
// the kind-implied `completed` outcome, and an output that records what was
// waited for and when it resolved — the honest content of a wait node's
// /nodes/<id>/output binding.
func waitCompleted(until json.RawMessage, now time.Time) SeamResult {
	payload := struct {
		Until       json.RawMessage `json:"until"`
		CompletedAt string          `json:"completed_at"`
	}{
		Until:       until,
		CompletedAt: now.Format(time.RFC3339Nano),
	}
	// A struct of raw JSON and a string never fails to marshal.
	output, _ := json.Marshal(payload)
	return SeamResult{TechStatus: engine.StatusSucceeded, Outcome: waitOutcome, Output: output}
}

// parkWait commits a wait seam's Async answer: park the claimed work item on
// the durable wait timer the seam computed, holding no lease afterwards. It
// is the wait-kind counterpart of park (dispatch.go) — same fenced release,
// same waiting_external node run — with a timer where the actor-invocation
// record would be, and deliberately NO deadline timer: a wait node's whole
// point is to be away for its declared wall-clock time, and the worker's
// default dispatch timeout must not fail a wait for lasting longer than an
// actor call is allowed to.
func (w *Worker) parkWait(ctx context.Context, claimed postgres.ClaimedWork, d postgres.Dispatch, dc DispatchContext, result SeamResult) error {
	if result.AsyncSignal != "" {
		return w.parkSignalWait(ctx, claimed, d, dc, result)
	}
	if result.AsyncDeadline.IsZero() {
		return w.failAttempt(ctx, claimed, "", engine.StatusFailed, kindWait,
			fmt.Sprintf("node %q wait dispatch answered async with no fire time; the wait cannot be armed", dc.NodeID))
	}
	err := w.db.StartDurableWait(ctx, postgres.StartDurableWaitInput{
		WorkID:       claimed.ID,
		WorkerID:     w.opts.WorkerID,
		FencingToken: claimed.FencingToken,
		Attempt:      int(claimed.Attempt),
		NamespaceID:  d.NamespaceID,
		RunID:        dc.RunID,
		NodeRunID:    dc.NodeRunID,
		NodeID:       dc.NodeID,
		AttemptID:    dc.AttemptID,
		TimerID:      result.AsyncRef,
		FireAt:       result.AsyncDeadline,
	})
	if err != nil {
		if isStale(err) {
			// The claim went while the wait was being armed. Nothing was
			// written; whoever holds the item now will arm it again.
			return nil
		}
		return err
	}
	return nil
}

// parkSignalWait is parkWait's until.signal half: the identical fenced
// release and waiting_external node run, with a pending signal subscription
// (Store.StartDurableSignalWait) where the timer would be, and deliberately
// NO deadline — an undelivered signal leaves the run parked and
// inspectable, never timed out by a dispatch default. The inbound event
// delivery route (POST /v1alpha1/events, internal/api) is what returns the
// parked work item to 'ready', the way the scheduler's timer fire does for
// a timer wait.
func (w *Worker) parkSignalWait(ctx context.Context, claimed postgres.ClaimedWork, d postgres.Dispatch, dc DispatchContext, result SeamResult) error {
	err := w.db.StartDurableSignalWait(ctx, postgres.StartDurableSignalWaitInput{
		WorkID:         claimed.ID,
		WorkerID:       w.opts.WorkerID,
		FencingToken:   claimed.FencingToken,
		Attempt:        int(claimed.Attempt),
		NamespaceID:    d.NamespaceID,
		RunID:          dc.RunID,
		NodeRunID:      dc.NodeRunID,
		NodeID:         dc.NodeID,
		AttemptID:      dc.AttemptID,
		SubscriptionID: result.AsyncRef,
		EventName:      result.AsyncSignal,
	})
	if err != nil && !isStale(err) {
		// A stale claim means the lease went while the park was being armed:
		// nothing was written, and whoever holds the item now arms it again —
		// the same tolerance parkWait's timer half applies.
		return err
	}
	return nil
}
