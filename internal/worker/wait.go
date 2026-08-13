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
// node's until.duration / until.timestamp becomes a durable timer, never a
// held lease or a sleeping goroutine.
//
// The life of a wait, end to end:
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
// until.signal is explicitly refused: delivering a named external signal
// needs the first-class event surface (emit/subscribe records and an
// authenticated inbound delivery route) that a follow-up task builds — see
// the build plan's t10 (docs/plans/2026-08-13-attempts-evidence-humans-loops.md).
// Refusing loudly is deliberate: a wait that silently completed (or silently
// never armed) because its signal surface does not exist yet would be the
// same claims-must-be-earned failure mode seams.go documents.

// waitOutcome is the kind-implied domain outcome every wait node declares
// (internal/compiler/vocabulary.go's impliedOutcomes[KindWait]).
const waitOutcome = "completed"

// waitTimerID derives the durable wait timer's id from the node run it
// wakes. Deterministic on purpose: a crashed-and-redispatched arm re-adopts
// the same timer (StartDurableWait's ON CONFLICT DO NOTHING keeps the
// original fire_at anchor), and the resumed dispatch after the fire finds
// the fired row under the same id without a search.
func waitTimerID(nodeRunID string) string { return "wait-" + nodeRunID }

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

// DispatchWait implements WaitDispatcher for until.duration and
// until.timestamp, and refuses until.signal. See the file doc comment for
// the full arm → park → fire → resume walk-through.
func (t *TimerWaitDispatcher) DispatchWait(ctx context.Context, dc DispatchContext, until json.RawMessage) (SeamResult, error) {
	spec, err := decodeUntil(until)
	if err != nil {
		return SeamResult{}, err
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

// decodeUntil validates the until block down to exactly one supported
// resume condition, refusing until.signal with the follow-up named.
func decodeUntil(until json.RawMessage) (untilSpec, error) {
	if len(until) == 0 {
		return untilSpec{}, errors.New("wait node carries no until block; the compiler requires one, so this pinned definition is corrupt")
	}
	var spec untilSpec
	if err := json.Unmarshal(until, &spec); err != nil {
		return untilSpec{}, fmt.Errorf("until block could not be decoded: %w", err)
	}
	if spec.Signal != "" {
		return untilSpec{}, fmt.Errorf(
			"until.signal %q is not supported yet: delivering a named signal needs the first-class event surface "+
				"(emit/subscribe records and an authenticated inbound event delivery route), which a follow-up task builds "+
				"(build plan t10, issue #39); use until.duration or until.timestamp until it lands", spec.Signal)
	}
	declared := 0
	if spec.Duration != "" {
		declared++
	}
	if spec.Timestamp != "" {
		declared++
	}
	if declared != 1 {
		return untilSpec{}, fmt.Errorf(
			"until block must declare exactly one resume condition, got %d of duration/timestamp; "+
				"declare one of them (until.signal is a separate, not-yet-supported condition)", declared)
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
