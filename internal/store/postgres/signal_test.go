package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Store-level tests for the signal half of the durable wait surface (task
// t10, issue #39, spec decision c35): StartDurableSignalWait parks a leased
// work item on a pending signal_subscriptions row the way StartDurableWait
// parks it on a timer, and DeliverSignalEvent — the inbound event route's
// single transaction — appends the event fact and fires matching pending
// subscriptions. The full worker-driven walk (real dispatch parks, real
// authenticated POST resumes, run completes through its edges) lives in
// internal/worker/wait_test.go; these tests pin the store contract itself.

// claimOne claims the ready work item for nodeRunID through the real
// claiming path, failing the test if it is not claimable. Namespaces here
// are per-test, so ClaimWork over the test's own namespace only ever sees
// its own rows.
func claimOne(t *testing.T, s *postgres.Store, namespaceID, nodeRunID string) postgres.ClaimedWork {
	t.Helper()
	claimed, err := s.ClaimWork(context.Background(), namespaceID, "signal-test-worker", 2*time.Minute, 20)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}
	for i := range claimed {
		if claimed[i].NodeRunID == nodeRunID {
			return claimed[i]
		}
	}
	t.Fatalf("no claimable work item for node run %s", nodeRunID)
	return postgres.ClaimedWork{}
}

// parkOnSignal enqueues, claims, and parks nodeRunID's work item on a
// signal subscription — the exact sequence the worker's dispatchSignalWait
// + parkSignalWait perform — returning the claim and the subscription id.
func parkOnSignal(t *testing.T, s *postgres.Store, namespaceID, runID, nodeRunID, eventName string) (postgres.ClaimedWork, string) {
	t.Helper()
	mustEnqueued(t, s, namespaceID, nodeRunID)
	claimed := claimOne(t, s, namespaceID, nodeRunID)
	subID := "signal-" + nodeRunID
	if err := s.StartDurableSignalWait(context.Background(), postgres.StartDurableSignalWaitInput{
		WorkID:         claimed.ID,
		WorkerID:       "signal-test-worker",
		FencingToken:   claimed.FencingToken,
		Attempt:        int(claimed.Attempt),
		NamespaceID:    namespaceID,
		RunID:          runID,
		NodeRunID:      nodeRunID,
		NodeID:         "pause",
		AttemptID:      "att_" + store.NewULID(),
		SubscriptionID: subID,
		EventName:      eventName,
	}); err != nil {
		t.Fatalf("StartDurableSignalWait: %v", err)
	}
	return claimed, subID
}

func nodeRunStatus(t *testing.T, s *postgres.Store, nodeRunID string) string {
	t.Helper()
	var status string
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT status FROM node_runs WHERE id = $1`, nodeRunID).Scan(&status); err != nil {
		t.Fatalf("read node run %s: %v", nodeRunID, err)
	}
	return status
}

func runEventCount(t *testing.T, s *postgres.Store, runID, eventType string) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE aggregate_id = $1 AND event_type = $2`,
		runID, eventType).Scan(&n); err != nil {
		t.Fatalf("count %s events: %v", eventType, err)
	}
	return n
}

func TestStartDurableSignalWaitParksWorkItemOnPendingSubscription(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "signal-park")
	runID, nodeRunID := mustRunAndNodeRun(t, s, ns.ID)

	claimed, subID := parkOnSignal(t, s, ns.ID, runID, nodeRunID, "green-light")

	// The park: leaseless waiting work item, waiting_external node run,
	// pending subscription — the timer park's exact discipline.
	if state, _ := workItemState(t, s, claimed.ID); state != "waiting" {
		t.Errorf("work item state after park = %q, want waiting", state)
	}
	if got := nodeRunStatus(t, s, nodeRunID); got != "waiting_external" {
		t.Errorf("node run status after park = %q, want waiting_external", got)
	}
	sub, found, err := s.SignalSubscriptionByID(ctx, subID)
	if err != nil || !found {
		t.Fatalf("SignalSubscriptionByID(%s) = (found=%v, err=%v), want a row", subID, found, err)
	}
	if sub.Status != postgres.SignalSubscriptionPending || sub.EventName != "green-light" {
		t.Errorf("subscription = (%s, %q), want (pending, green-light)", sub.Status, sub.EventName)
	}
	if sub.RunID != runID || sub.NodeRunID != nodeRunID {
		t.Errorf("subscription scope = (%s, %s), want (%s, %s)", sub.RunID, sub.NodeRunID, runID, nodeRunID)
	}

	// The audit event names the wait and the signal.
	if n := runEventCount(t, s, runID, postgres.TypeAttemptWaitingSignal); n != 1 {
		t.Errorf("attempt.waiting-signal events = %d, want 1", n)
	}
}

// TestJiraHistoryCutoverAdoptsHeadWithoutReplayingRecordedHistory is the
// prod-shaped cutover replay: legacy status/comment rows identify a known
// issue, the first history pass adopts Jira's observed cumulative head, and
// every older per-position fact is suppressed without appending anything.
func TestJiraHistoryCutoverAdoptsHeadWithoutReplayingRecordedHistory(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "jira-history-cutover")
	legacyEvent := store.NewULID()
	if _, err := s.Pool().Exec(ctx, `INSERT INTO signal_events
		(id, namespace_id, name, payload, emitter) VALUES ($1,$2,'legacy','{}','recorded-prod')`, legacyEvent, ns.ID); err != nil {
		t.Fatalf("seed legacy event: %v", err)
	}
	for key, watermark := range map[string]string{
		"jira:team.example.com:SCRUM-3:status":  `{"status":"In Progress"}`,
		"jira:team.example.com:SCRUM-3:comment": `{"updated_at":"2026-08-19T12:00:00Z","newest_comment_at":"2026-08-19T11:59:00Z"}`,
	} {
		if _, err := s.Pool().Exec(ctx, `INSERT INTO signal_event_watermarks
			(namespace_id,source_key,watermark,event_id) VALUES ($1,$2,$3,$4)`, ns.ID, key, watermark, legacyEvent); err != nil {
			t.Fatalf("seed recorded watermark %s: %v", key, err)
		}
	}
	// This is migration 0041's data statement, rerun after the recorded rows
	// are installed so the test exercises its exact translation.
	if _, err := s.Pool().Exec(ctx, `INSERT INTO jira_history_watermark_cutovers (namespace_id, issue_source_key)
		SELECT DISTINCT namespace_id, regexp_replace(source_key, ':(status|comment)$', '')
		FROM signal_event_watermarks WHERE namespace_id=$1
		AND source_key ~ '^jira:[^:]+:[^:]+:(status|comment)$' ON CONFLICT DO NOTHING`, ns.ID); err != nil {
		t.Fatalf("translate legacy rows: %v", err)
	}
	if err := s.AdoptJiraHistoryHead(ctx, ns.ID, "jira:team.example.com:SCRUM-3", json.RawMessage(`{"changelog_id":"10180","comment_id":"10118"}`)); err != nil {
		t.Fatalf("AdoptJiraHistoryHead: %v", err)
	}

	var before int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM signal_events WHERE namespace_id=$1`, ns.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, fact := range []postgres.DeliverSignalEventInput{
		{NamespaceID: ns.ID, Name: "pr-upkeep.jira.transitioned.to-do", Emitter: "recorded-replay", SourceKey: "jira:team.example.com:SCRUM-3:history:changelog:10179", Watermark: json.RawMessage(`{"changelog_id":"10179","comment_id":""}`)},
		{NamespaceID: ns.ID, Name: "pr-upkeep.jira.comment", Emitter: "recorded-replay", SourceKey: "jira:team.example.com:SCRUM-3:history:comment:10118", Watermark: json.RawMessage(`{"changelog_id":"10180","comment_id":"10118"}`)},
	} {
		delivery, err := s.DeliverSignalEvent(ctx, fact)
		if err != nil {
			t.Fatalf("replay %s: %v", fact.SourceKey, err)
		}
		if !delivery.Suppressed || !delivery.Duplicate {
			t.Errorf("replay %s = %+v, want adopted suppression", fact.SourceKey, delivery)
		}
	}
	var after int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM signal_events WHERE namespace_id=$1`, ns.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("first post-cutover replay appended %d facts, want zero", after-before)
	}
}

func TestStartDurableSignalWaitRefusesStaleFencing(t *testing.T) {
	s := requireStore(t)
	ns := mustNamespace(t, s, "signal-stale")
	runID, nodeRunID := mustRunAndNodeRun(t, s, ns.ID)
	mustEnqueued(t, s, ns.ID, nodeRunID)
	claimed := claimOne(t, s, ns.ID, nodeRunID)

	err := s.StartDurableSignalWait(context.Background(), postgres.StartDurableSignalWaitInput{
		WorkID:         claimed.ID,
		WorkerID:       "signal-test-worker",
		FencingToken:   claimed.FencingToken + 1, // not the claim's token
		Attempt:        int(claimed.Attempt),
		NamespaceID:    ns.ID,
		RunID:          runID,
		NodeRunID:      nodeRunID,
		NodeID:         "pause",
		AttemptID:      "att_" + store.NewULID(),
		SubscriptionID: "signal-" + nodeRunID,
		EventName:      "green-light",
	})
	if !errors.Is(err, engine.ErrStaleClaim) {
		t.Fatalf("err = %v, want engine.ErrStaleClaim", err)
	}
	// Nothing was written: the item is still leased, no subscription exists.
	if state, _ := workItemState(t, s, claimed.ID); state != "leased" {
		t.Errorf("work item state = %q, want leased (a stale park must write nothing)", state)
	}
	if _, found, _ := s.SignalSubscriptionByID(context.Background(), "signal-"+nodeRunID); found {
		t.Error("a stale park persisted a subscription")
	}
}

func TestDeliverSignalEventWithNoSubscriptionIsAFactNotAnError(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "signal-fact")

	delivery, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID,
		Name:        "nobody-is-listening",
		Payload:     json.RawMessage(`{"n":1}`),
		Emitter:     "external",
	})
	if err != nil {
		t.Fatalf("DeliverSignalEvent: %v", err)
	}
	ev, fired := delivery.Event, delivery.Fired
	if len(fired) != 0 {
		t.Fatalf("fired %d subscriptions, want 0", len(fired))
	}
	got, found, err := s.SignalEventByID(ctx, ev.ID)
	if err != nil || !found {
		t.Fatalf("SignalEventByID(%s) = (found=%v, err=%v), want the appended fact", ev.ID, found, err)
	}
	if got.Name != "nobody-is-listening" || got.Emitter != "external" {
		t.Errorf("event = (%q, %q), want (nobody-is-listening, external)", got.Name, got.Emitter)
	}
}

func TestDeliverSignalEventWatermarkSuppressesRestartDuplicate(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "signal-watermark")
	in := postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID, Name: "pr-upkeep.pr", Emitter: "sweep",
		SourceKey: "github:owner/repo:pr:17",
		Watermark: json.RawMessage(`{"head_sha":"abc","newest_comment_at":"2026-08-15T10:00:00Z"}`),
	}
	first, err := s.DeliverSignalEvent(ctx, in)
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	second, err := s.DeliverSignalEvent(ctx, in)
	if err != nil {
		t.Fatalf("restart delivery: %v", err)
	}
	if !second.Duplicate {
		t.Fatal("restart delivery was not identified as duplicate")
	}
	if second.Event.ID != first.Event.ID {
		t.Fatalf("duplicate event id = %s, want original %s", second.Event.ID, first.Event.ID)
	}
	var count int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM signal_events WHERE namespace_id=$1 AND name=$2`, ns.ID, in.Name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("signal_events count = %d, want 1", count)
	}
}

// TestAnswerDeliveryRollsBackWatermarkAndEventTogether is t11's restart
// fixture. A database failure after signal_events has been appended but while
// its answer watermark is advancing must leave NEITHER half committed. The
// next sweep pass then emits exactly once: no skipped answer, no duplicate.
func TestAnswerDeliveryRollsBackWatermarkAndEventTogether(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "jira-answer-transaction")
	sourceKey := "jira:team.example.com:EX-17:comment-answer-transaction"
	in := postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID, Name: "pr-upkeep.jira.comment", Emitter: "pr-upkeep/sweep",
		Payload:   json.RawMessage(`{"originating_question_id":"q-17","answer":{"comment_id":"1009"}}`),
		SourceKey: sourceKey,
		Watermark: json.RawMessage(`{"updated_at":"2026-08-17T00:00:00Z","newest_comment_at":"2026-08-17T00:00:00Z"}`),
	}

	// Conditional failure keeps the fixture isolated even when database tests
	// share one PostgreSQL instance.
	if _, err := s.Pool().Exec(ctx, `
		CREATE OR REPLACE FUNCTION fail_t11_answer_watermark() RETURNS trigger AS $$
		BEGIN
			IF NEW.source_key = 'jira:team.example.com:EX-17:comment-answer-transaction' THEN
				RAISE EXCEPTION 'fixture crash between answer append and watermark advance';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER fail_t11_answer_watermark
		BEFORE INSERT OR UPDATE ON signal_event_watermarks
		FOR EACH ROW EXECUTE FUNCTION fail_t11_answer_watermark();
	`); err != nil {
		t.Fatalf("install failure fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.Pool().Exec(context.Background(), `DROP TRIGGER IF EXISTS fail_t11_answer_watermark ON signal_event_watermarks`)
		_, _ = s.Pool().Exec(context.Background(), `DROP FUNCTION IF EXISTS fail_t11_answer_watermark()`)
	})

	if _, err := s.DeliverSignalEvent(ctx, in); err == nil {
		t.Fatal("failure fixture unexpectedly committed the answer")
	}
	var events, watermarks int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM signal_events WHERE namespace_id=$1 AND name=$2`, ns.ID, in.Name).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM signal_event_watermarks WHERE namespace_id=$1 AND source_key=$2`, ns.ID, sourceKey).Scan(&watermarks); err != nil {
		t.Fatal(err)
	}
	if events != 0 || watermarks != 0 {
		t.Fatalf("failed pass left (events=%d, watermarks=%d), want (0, 0)", events, watermarks)
	}

	if _, err := s.Pool().Exec(ctx, `DROP TRIGGER fail_t11_answer_watermark ON signal_event_watermarks`); err != nil {
		t.Fatalf("remove failure fixture: %v", err)
	}
	first, err := s.DeliverSignalEvent(ctx, in)
	if err != nil {
		t.Fatalf("restart delivery: %v", err)
	}
	second, err := s.DeliverSignalEvent(ctx, in)
	if err != nil {
		t.Fatalf("next sweep delivery: %v", err)
	}
	if !second.Duplicate || second.Event.ID != first.Event.ID {
		t.Fatalf("next pass = (duplicate=%v, event=%s), want original %s", second.Duplicate, second.Event.ID, first.Event.ID)
	}
}

func TestDeliverSignalEventFiresPendingSubscription(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "signal-fire")
	runID, nodeRunID := mustRunAndNodeRun(t, s, ns.ID)
	claimed, subID := parkOnSignal(t, s, ns.ID, runID, nodeRunID, "green-light")

	delivery, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID,
		Name:        "green-light",
		Payload:     json.RawMessage(`{"go":true}`),
		Emitter:     "ops@example",
	})
	if err != nil {
		t.Fatalf("DeliverSignalEvent: %v", err)
	}
	ev, fired := delivery.Event, delivery.Fired
	if len(fired) != 1 || fired[0].ID != subID {
		t.Fatalf("fired = %+v, want exactly the parked subscription %s", fired, subID)
	}

	sub, _, err := s.SignalSubscriptionByID(ctx, subID)
	if err != nil {
		t.Fatalf("SignalSubscriptionByID: %v", err)
	}
	if sub.Status != postgres.SignalSubscriptionFired || sub.FiredEventID != ev.ID {
		t.Errorf("subscription = (%s, fired by %q), want (fired, %q)", sub.Status, sub.FiredEventID, ev.ID)
	}
	if sub.FiredAt.IsZero() {
		t.Error("fired subscription has no fired_at")
	}

	// The delivery's resume effect: the parked work item is claimable again.
	if state, _ := workItemState(t, s, claimed.ID); state != "ready" {
		t.Errorf("work item state after delivery = %q, want ready", state)
	}
	if n := runEventCount(t, s, runID, postgres.TypeSignalResumed); n != 1 {
		t.Errorf("signal.resumed events = %d, want 1", n)
	}
}

func TestDeliverSignalEventDoesNotFireWrongNameOrOtherRunScope(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "signal-scope")
	runA, nodeRunA := mustRunAndNodeRun(t, s, ns.ID)
	runB, nodeRunB := mustRunAndNodeRun(t, s, ns.ID)
	claimedA, subA := parkOnSignal(t, s, ns.ID, runA, nodeRunA, "green-light")
	claimedB, subB := parkOnSignal(t, s, ns.ID, runB, nodeRunB, "green-light")

	// A different name resumes nothing.
	if d, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID, Name: "red-light", Emitter: "external",
	}); err != nil || len(d.Fired) != 0 {
		t.Fatalf("wrong-name delivery = (fired=%d, err=%v), want (0, nil)", len(d.Fired), err)
	}

	// A run-scoped delivery resumes only that run's subscription.
	delivery, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID, Name: "green-light", Emitter: "external", RunID: runA,
	})
	if err != nil {
		t.Fatalf("DeliverSignalEvent(run-scoped): %v", err)
	}
	fired := delivery.Fired
	if len(fired) != 1 || fired[0].ID != subA {
		t.Fatalf("run-scoped delivery fired %+v, want exactly %s", fired, subA)
	}
	if state, _ := workItemState(t, s, claimedA.ID); state != "ready" {
		t.Errorf("run A's work item = %q, want ready", state)
	}
	if state, _ := workItemState(t, s, claimedB.ID); state != "waiting" {
		t.Errorf("run B's work item = %q, want waiting (a run-scoped event must not resume it)", state)
	}

	// A namespace-wide delivery then resumes the remaining waiter.
	delivery, err = s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID, Name: "green-light", Emitter: "external",
	})
	if err != nil {
		t.Fatalf("DeliverSignalEvent(namespace-wide): %v", err)
	}
	fired = delivery.Fired
	if len(fired) != 1 || fired[0].ID != subB {
		t.Fatalf("namespace-wide delivery fired %+v, want exactly %s", fired, subB)
	}
}

// TestDeliverSignalEventDoesNotRetroactivelyFireLaterSubscriber pins this
// pass's documented delivery semantics (issue #43): subscription-then-event
// resumes; event-then-subscription stays parked. The event is still a fact
// in signal_events either way.
func TestDeliverSignalEventDoesNotRetroactivelyFireLaterSubscriber(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "signal-late")
	runID, nodeRunID := mustRunAndNodeRun(t, s, ns.ID)

	// The event arrives first — appended, resuming nothing.
	delivery, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID, Name: "green-light", Emitter: "external",
	})
	if err != nil || len(delivery.Fired) != 0 {
		t.Fatalf("early delivery = (fired=%d, err=%v), want (0, nil)", len(delivery.Fired), err)
	}
	ev := delivery.Event

	// The subscription arrives second: it stays parked. Only a NEW delivery
	// may resume it.
	claimed, subID := parkOnSignal(t, s, ns.ID, runID, nodeRunID, "green-light")
	sub, _, err := s.SignalSubscriptionByID(ctx, subID)
	if err != nil {
		t.Fatalf("SignalSubscriptionByID: %v", err)
	}
	if sub.Status != postgres.SignalSubscriptionPending {
		t.Errorf("late subscription = %s, want pending (no retroactive fire on %s)", sub.Status, ev.ID)
	}
	if state, _ := workItemState(t, s, claimed.ID); state != "waiting" {
		t.Errorf("work item = %q, want waiting", state)
	}
	if _, found, _ := s.SignalEventByID(ctx, ev.ID); !found {
		t.Error("the early event is not readable back; it must remain an appended fact")
	}
}

// TestDeliverSignalEventLeavesCancelledWorkItemDead pins the resume
// UPDATE's state guard: a delivery may only return a work item that is
// actually parked ('waiting') to 'ready', never resurrect a 'cancelled'
// one. The API's cancelRun REAP (internal/api's cancel path, task t10)
// retires the pending subscription together with the work item under the
// run's advisory lock, so in the normal order a delivery finds no pending
// row at all — this test simulates the subscription OUTLIVING that reap
// (a pre-t10 database, or any future writer that cancels the work item
// without retiring the subscription) and asserts the delivery acks the
// event by retiring the leftover subscription without re-enabling the
// dead run's work.
func TestDeliverSignalEventLeavesCancelledWorkItemDead(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "signal-dead")
	runID, nodeRunID := mustRunAndNodeRun(t, s, ns.ID)
	claimed, subID := parkOnSignal(t, s, ns.ID, runID, nodeRunID, "green-light")

	// Cancel the run's rows exactly as cancelRun writes them — but leave
	// the subscription pending, simulating it outliving the REAP.
	for _, stmt := range []struct{ sql, arg string }{
		{`UPDATE runs SET status = 'cancelled', updated_at = now(), completed_at = now() WHERE id = $1`, runID},
		{`UPDATE node_runs SET status = 'cancelled', updated_at = now(), completed_at = now() WHERE id = $1`, nodeRunID},
		{`UPDATE work_items SET state = 'cancelled', updated_at = now() WHERE node_run_id = $1`, nodeRunID},
	} {
		if _, err := s.Pool().Exec(ctx, stmt.sql, stmt.arg); err != nil {
			t.Fatalf("cancel setup: %v", err)
		}
	}

	delivery, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID, Name: "green-light", Emitter: "external",
	})
	if err != nil {
		t.Fatalf("DeliverSignalEvent: %v", err)
	}
	fired := delivery.Fired

	// The event is acked: the leftover subscription is retired (fired), so
	// it can never match a later delivery either.
	if len(fired) != 1 || fired[0].ID != subID {
		t.Fatalf("fired = %+v, want exactly the leftover subscription %s", fired, subID)
	}
	sub, _, err := s.SignalSubscriptionByID(ctx, subID)
	if err != nil {
		t.Fatalf("SignalSubscriptionByID: %v", err)
	}
	if sub.Status != postgres.SignalSubscriptionFired {
		t.Errorf("leftover subscription after delivery = %s, want fired (acked, retired)", sub.Status)
	}

	// ...but nothing about the dead run came back to life.
	if state, _ := workItemState(t, s, claimed.ID); state != "cancelled" {
		t.Errorf("work item after delivery = %q, want cancelled (a delivery must never resurrect a cancelled run's work)", state)
	}
	var runStatus string
	if err := s.Pool().QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if runStatus != "cancelled" {
		t.Errorf("run after delivery = %q, want cancelled", runStatus)
	}
}

// TestStartDurableSignalWaitReparkAdoptsOriginalSubscription pins the
// ON CONFLICT DO NOTHING contract: a re-park under the same deterministic
// subscription id must not reset a subscription a delivery already fired.
func TestStartDurableSignalWaitReparkAdoptsOriginalSubscription(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "signal-repark")
	runID, nodeRunID := mustRunAndNodeRun(t, s, ns.ID)
	_, subID := parkOnSignal(t, s, ns.ID, runID, nodeRunID, "green-light")

	delivery, err := s.DeliverSignalEvent(ctx, postgres.DeliverSignalEventInput{
		NamespaceID: ns.ID, Name: "green-light", Emitter: "external",
	})
	if err != nil {
		t.Fatalf("DeliverSignalEvent: %v", err)
	}
	ev := delivery.Event

	// The anomalous early re-park: claim the now-ready item and park again
	// under the same subscription id.
	claimed := claimOne(t, s, ns.ID, nodeRunID)
	if err := s.StartDurableSignalWait(ctx, postgres.StartDurableSignalWaitInput{
		WorkID:         claimed.ID,
		WorkerID:       "signal-test-worker",
		FencingToken:   claimed.FencingToken,
		Attempt:        int(claimed.Attempt),
		NamespaceID:    ns.ID,
		RunID:          runID,
		NodeRunID:      nodeRunID,
		NodeID:         "pause",
		AttemptID:      "att_" + store.NewULID(),
		SubscriptionID: subID,
		EventName:      "green-light",
	}); err != nil {
		t.Fatalf("re-park: %v", err)
	}

	sub, _, err := s.SignalSubscriptionByID(ctx, subID)
	if err != nil {
		t.Fatalf("SignalSubscriptionByID: %v", err)
	}
	if sub.Status != postgres.SignalSubscriptionFired || sub.FiredEventID != ev.ID {
		t.Errorf("subscription after re-park = (%s, %q), want the ORIGINAL fired row (fired, %q)",
			sub.Status, sub.FiredEventID, ev.ID)
	}
}
