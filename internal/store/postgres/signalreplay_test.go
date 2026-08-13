package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Store-level tests for replay / catch-up (issue #43 task t21, design D12):
// a subscriber that subscribes AFTER its event fired still resumes, exactly
// once per fact, never on history from before its run existed.
//
// Every test here drives Store.ReplaySignalEvent directly rather than the
// worker, because what is under test is the cursor arithmetic — the floor
// (review R6) and the maximum across a run's fired subscriptions (review R5)
// — and those are decided in one SQL statement. The end-to-end walk (a real
// dispatch that finds a backlogged fact and completes instead of parking)
// lives in internal/worker/wait_test.go.

// runCreatedAt reads a run's creation timestamp — the R6 floor the replay
// probe compares against.
func runCreatedAt(t *testing.T, s *postgres.Store, runID string) time.Time {
	t.Helper()
	var at time.Time
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT created_at FROM runs WHERE id = $1`, runID).Scan(&at); err != nil {
		t.Fatalf("read run %s created_at: %v", runID, err)
	}
	return at
}

// insertSignalEventAt appends one signal_events fact with an explicit
// created_at, which is the only way to test the floor and the ordering
// deterministically: DeliverSignalEvent stamps now() and cannot place a fact
// before its run.
func insertSignalEventAt(t *testing.T, s *postgres.Store, namespaceID, runID, name string, at time.Time) string {
	t.Helper()
	id := store.NewULID()
	var scopedRun any
	if runID != "" {
		scopedRun = runID
	}
	if _, err := s.Pool().Exec(context.Background(), `
		INSERT INTO signal_events (id, namespace_id, run_id, name, payload, emitter, created_at)
		VALUES ($1, $2, $3, $4, '{"seen":true}'::jsonb, 'test', $5)`,
		id, namespaceID, scopedRun, name, at,
	); err != nil {
		t.Fatalf("insert signal event: %v", err)
	}
	return id
}

// replayFor asks the catch-up probe for one node run of a run.
func replayFor(t *testing.T, s *postgres.Store, runID, nodeRunID, name string) (postgres.SignalEvent, bool) {
	t.Helper()
	_, ev, replayed, err := s.ReplaySignalEvent(context.Background(), postgres.ReplaySignalEventInput{
		RunID:          runID,
		NodeRunID:      nodeRunID,
		NodeID:         "pause",
		AttemptID:      "att_" + store.NewULID(),
		SubscriptionID: "signal-" + nodeRunID,
		EventName:      name,
	})
	if err != nil {
		t.Fatalf("ReplaySignalEvent: %v", err)
	}
	return ev, replayed
}

// TestReplayResumesASubscriberThatSubscribedAfterTheEvent is the acceptance
// h42's replay half asks for: the 0016 gap, closed.
func TestReplayResumesASubscriberThatSubscribedAfterTheEvent(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "signal-replay")
	runID, nodeRunID := mustRunAndNodeRun(t, s, ns.ID)

	// The event fires while nothing is waiting. Under the pre-t21 semantics
	// this fact was unreachable for every later subscriber of this run.
	delivery, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID, Name: "green-light", Emitter: "ci",
	})
	if err != nil {
		t.Fatalf("DeliverSignalEvent: %v", err)
	}
	ev := delivery.Event

	replayedEvent, replayed := replayFor(t, s, runID, nodeRunID, "green-light")
	if !replayed {
		t.Fatalf("ReplaySignalEvent replayed = false, want the backlogged fact to resume the late subscriber")
	}
	if replayedEvent.ID != ev.ID {
		t.Errorf("replayed event = %s, want %s", replayedEvent.ID, ev.ID)
	}

	// The subscription is committed already fired, so the dispatch that asked
	// completes through the ordinary §12.5 transaction rather than parking.
	stored, found, err := s.SignalSubscriptionByID(ctx, "signal-"+nodeRunID)
	if err != nil || !found {
		t.Fatalf("SignalSubscriptionByID = (found=%v, err=%v), want the armed row", found, err)
	}
	if stored.Status != postgres.SignalSubscriptionFired || stored.FiredEventID != ev.ID {
		t.Errorf("subscription = (%s, %s), want (fired, %s)", stored.Status, stored.FiredEventID, ev.ID)
	}

	// Catch-up is distinguishable from live delivery in the audit trail.
	if n := runEventCount(t, s, runID, postgres.TypeSignalReplayed); n != 1 {
		t.Errorf("signal.replayed events = %d, want 1", n)
	}
	if n := runEventCount(t, s, runID, postgres.TypeSignalResumed); n != 0 {
		t.Errorf("signal.resumed events = %d, want 0 (nothing was parked to resume)", n)
	}
}

// TestReplayIgnoresFactsOlderThanTheRun pins the floor: a run catches up on
// its own lifetime, never on history it was never part of. Without it, any
// run subscribing to a busy name would instantly consume months-old facts.
func TestReplayIgnoresFactsOlderThanTheRun(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "signal-replay-floor")
	runID, nodeRunID := mustRunAndNodeRun(t, s, ns.ID)

	insertSignalEventAt(t, s, ns.ID, "", "ancient", runCreatedAt(t, s, runID).Add(-time.Hour))

	if _, replayed := replayFor(t, s, runID, nodeRunID, "ancient"); replayed {
		t.Fatal("a fact from before the run was created was replayed into it")
	}
}

// TestReplayAdmitsAFactFromTheRunsOwnInstant is review finding R6, made
// executable: the floor is `>=`, so a fact delivered in the same instant the
// run was created is NOT silently excluded. The two timestamps do not even
// come from the same clock (the engine's for the run, the database's for the
// fact), so a strict floor would drop events the run genuinely should see.
func TestReplayAdmitsAFactFromTheRunsOwnInstant(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "signal-replay-instant")
	runID, nodeRunID := mustRunAndNodeRun(t, s, ns.ID)

	born := runCreatedAt(t, s, runID)
	id := insertSignalEventAt(t, s, ns.ID, "", "born-together", born)

	ev, replayed := replayFor(t, s, runID, nodeRunID, "born-together")
	if !replayed {
		t.Fatal("a fact delivered in the same instant as run creation was excluded; the floor must be >= (review R6)")
	}
	if ev.ID != id {
		t.Errorf("replayed event = %s, want %s", ev.ID, id)
	}
}

// TestReplayCursorIsTheMaximumAcrossEveryFiredSubscription is review finding
// R5. signal_subscriptions deliberately carries no uniqueness over (run_id,
// event_name), so one run may hold several subscriptions to one name. The
// cursor must be the NEWEST fact any of them consumed — a cursor that read
// whichever fired row came first would hand this late subscriber a fact the
// run has already seen.
func TestReplayCursorIsTheMaximumAcrossEveryFiredSubscription(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "signal-replay-cursor")
	runID, first := mustRunAndNodeRun(t, s, ns.ID)
	second := mustNodeRunForRun(t, s, ns.ID, runID)
	third := mustNodeRunForRun(t, s, ns.ID, runID)

	born := runCreatedAt(t, s, runID)
	older := insertSignalEventAt(t, s, ns.ID, "", "tick", born.Add(time.Second))
	newer := insertSignalEventAt(t, s, ns.ID, "", "tick", born.Add(2*time.Second))
	newest := insertSignalEventAt(t, s, ns.ID, "", "tick", born.Add(3*time.Second))

	// Two subscriptions of the same run consume the first two facts.
	if ev, replayed := replayFor(t, s, runID, first, "tick"); !replayed || ev.ID != older {
		t.Fatalf("first catch-up = (%v, %s), want the oldest fact %s", replayed, ev.ID, older)
	}
	if ev, replayed := replayFor(t, s, runID, second, "tick"); !replayed || ev.ID != newer {
		t.Fatalf("second catch-up = (%v, %s), want %s", replayed, ev.ID, newer)
	}

	// The third must land on the newest, which is only true if the cursor is
	// MAX over both fired rows rather than either one of them.
	ev, replayed := replayFor(t, s, runID, third, "tick")
	if !replayed {
		t.Fatal("third late subscriber found nothing to catch up on")
	}
	if ev.ID == newer {
		t.Fatalf("third catch-up re-consumed %s; the cursor read one fired subscription, not the maximum (review R5)", newer)
	}
	if ev.ID != newest {
		t.Errorf("third catch-up = %s, want %s", ev.ID, newest)
	}
}

// TestReplayIsMonotonicPerRunAndName pins D12's queue semantics: a loop that
// re-parks on one name consumes the backlog one fact per iteration, oldest
// first, never the same fact twice. Without the cursor this is a hot loop
// terminated only by maxVisitsPerNode.
func TestReplayIsMonotonicPerRunAndName(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "signal-replay-monotonic")
	runID, _ := mustRunAndNodeRun(t, s, ns.ID)

	born := runCreatedAt(t, s, runID)
	want := []string{
		insertSignalEventAt(t, s, ns.ID, "", "iter", born.Add(time.Second)),
		insertSignalEventAt(t, s, ns.ID, "", "iter", born.Add(2*time.Second)),
		insertSignalEventAt(t, s, ns.ID, "", "iter", born.Add(3*time.Second)),
	}

	for i, expected := range want {
		nodeRunID := mustNodeRunForRun(t, s, ns.ID, runID)
		ev, replayed := replayFor(t, s, runID, nodeRunID, "iter")
		if !replayed {
			t.Fatalf("iteration %d found nothing to catch up on", i)
		}
		if ev.ID != expected {
			t.Fatalf("iteration %d consumed %s, want %s (oldest unseen first)", i, ev.ID, expected)
		}
	}

	// The backlog is exhausted; the next iteration parks, as it must.
	nodeRunID := mustNodeRunForRun(t, s, ns.ID, runID)
	if ev, replayed := replayFor(t, s, runID, nodeRunID, "iter"); replayed {
		t.Fatalf("a fourth iteration replayed %s from an exhausted backlog", ev.ID)
	}
}

// TestReplayHonoursTheEventsRunScope keeps the fact table's optional scope
// meaningful during catch-up: a fact addressed to another run is not this
// run's to consume, while a namespace-wide fact is.
func TestReplayHonoursTheEventsRunScope(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "signal-replay-scope")
	mine, mineNodeRun := mustRunAndNodeRun(t, s, ns.ID)
	theirs, _ := mustRunAndNodeRun(t, s, ns.ID)

	born := runCreatedAt(t, s, mine)
	insertSignalEventAt(t, s, ns.ID, theirs, "scoped", born.Add(time.Second))

	if ev, replayed := replayFor(t, s, mine, mineNodeRun, "scoped"); replayed {
		t.Fatalf("replayed %s, a fact scoped to run %s, into run %s", ev.ID, theirs, mine)
	}

	wide := insertSignalEventAt(t, s, ns.ID, "", "scoped", born.Add(2*time.Second))
	other := mustNodeRunForRun(t, s, ns.ID, mine)
	if ev, replayed := replayFor(t, s, mine, other, "scoped"); !replayed || ev.ID != wide {
		t.Fatalf("namespace-wide catch-up = (%v, %s), want %s", replayed, ev.ID, wide)
	}
}

// TestReplayFindsNothingWithAnEmptyBacklog is the ordinary answer: nothing to
// catch up on, so the caller parks exactly as it always did.
func TestReplayFindsNothingWithAnEmptyBacklog(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "signal-replay-empty")
	runID, nodeRunID := mustRunAndNodeRun(t, s, ns.ID)

	if _, replayed := replayFor(t, s, runID, nodeRunID, "never-delivered"); replayed {
		t.Fatal("an empty backlog produced a replay")
	}
	if _, found, err := s.SignalSubscriptionByID(context.Background(), "signal-"+nodeRunID); err != nil || found {
		t.Fatalf("a refused catch-up armed a subscription (found=%v, err=%v)", found, err)
	}
}

// TestReplayAdoptsAnAlreadyArmedSubscription covers the concurrent-dispatch
// race: two dispatches of one node run race for a single primary key, and the
// loser must adopt the winner's fact rather than consume a second one — the
// cursor is per (run, name), and double-spending it would skip a fact nobody
// ever saw.
func TestReplayAdoptsAnAlreadyArmedSubscription(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "signal-replay-adopt")
	runID, nodeRunID := mustRunAndNodeRun(t, s, ns.ID)

	born := runCreatedAt(t, s, runID)
	first := insertSignalEventAt(t, s, ns.ID, "", "twice", born.Add(time.Second))
	second := insertSignalEventAt(t, s, ns.ID, "", "twice", born.Add(2*time.Second))

	if ev, replayed := replayFor(t, s, runID, nodeRunID, "twice"); !replayed || ev.ID != first {
		t.Fatalf("first catch-up = (%v, %s), want %s", replayed, ev.ID, first)
	}
	// The same node run asking again adopts its own armed answer.
	ev, replayed := replayFor(t, s, runID, nodeRunID, "twice")
	if !replayed {
		t.Fatal("a re-ask of an armed node run reported nothing to replay")
	}
	if ev.ID != first {
		t.Errorf("re-ask consumed %s, want the already-adopted %s (%s must stay unconsumed)", ev.ID, first, second)
	}
}
