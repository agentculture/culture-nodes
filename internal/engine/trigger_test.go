package engine_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/ticketreport"
)

// Task t15 (spec c31/h16): "at most one active run per originating Jira
// issue -- a second state change or comment while a flow is mid-flight must
// resume or queue against the existing run, never spawn a parallel run on
// the same subject". Measuring internal/engine/trigger.go's TriggerEvent at
// HEAD found it created one run per matching event unconditionally -- no
// subject/correlation concept existed anywhere on the inbound event path
// (internal/compiler's trigger struct, internal/engine's PickupEvent, or
// internal/store/postgres's SignalEvent). These tests exercise the fix
// through the exact path a real deliverer uses: Store.DeliverSignalEvent
// with Trigger set to a real *engine.Engine, the same call
// internal/api/signalevents.go's handleDeliverEvent makes.

// publishFixtureWorkflow makes f's compiled workflow a trigger CANDIDATE:
// runEventTriggers (internal/store/postgres/eventtriggers.go) only offers a
// delivered fact to workflow_versions rows that already exist, and
// EnsureWorkflowVersion is normally reached by creating a run
// (engine.Engine.CreateRun) or by POST /v1alpha1/workflows
// (handlePublishWorkflow) — neither of which this test wants to go through
// just to make the definition visible. Calling EnsureWorkflowVersion
// directly, the way handlePublishWorkflow does, publishes the SAME row
// without creating a run or a token.
func publishFixtureWorkflow(t *testing.T, f *fixture) {
	t.Helper()
	es, err := storepg.NewEngineStore(f.store, f.ns.ID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	if _, err := es.EnsureWorkflowVersion(f.ctx, engine.WorkflowVersionInput{
		WorkflowKey:   f.cw.Name,
		SourceFormat:  string(f.cw.Format),
		Source:        string(f.cw.Source),
		NormalizedIR:  f.cw.Normalized,
		ContentDigest: f.cw.Digest,
	}); err != nil {
		t.Fatalf("publish fixture workflow: %v", err)
	}
}

// TestTicketDerivedSubIntervalRunPostsStartThenFinishThroughBridge proves the
// report path is engine lifecycle -> transactional outbox -> registered Jira
// actor. Both lifecycle transitions commit before one dispatcher pass, the
// timing that sweep-based reporting would lose.
func TestTicketDerivedSubIntervalRunPostsStartThenFinishThroughBridge(t *testing.T) {
	t.Setenv("NODES_UI_BASE_URL", "https://nodes.example/")
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	var mu sync.Mutex
	var comments []string
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var invocation struct {
			Input struct{ Verb, Issue, Comment string } `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&invocation); err != nil {
			t.Errorf("decode invocation: %v", err)
		}
		if invocation.Input.Verb != "post_comment" || invocation.Input.Issue != "SCRUM-203" {
			t.Errorf("narrow bridge input = %+v", invocation.Input)
		}
		mu.Lock()
		comments = append(comments, invocation.Input.Comment)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"outcome":"comment_posted","output":{},"ledger_delta":{"records":[]}}`))
	}))
	defer bridge.Close()
	if _, err := f.store.Pool().Exec(f.ctx, `INSERT INTO actors
		(id,namespace_id,actor_key,revision,kind,protocol,endpoint_ref)
		VALUES ($1,$2,$3,1,'bridge','actor-protocol',$4)`, store.NewULID(), f.ns.ID, storepg.JiraTicketReporterActorKey, bridge.URL); err != nil {
		t.Fatalf("register Jira bridge: %v", err)
	}

	delivery := deliverSubjectEvent(t, f, "SCRUM-203", "jira:team.example.com:SCRUM-203:status")
	runID := delivery.Triggered[0].RunID
	var startRows int
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM jira_ticket_report_outbox WHERE run_id=$1 AND phase='start'`, runID).Scan(&startRows); err != nil || startRows != 1 {
		t.Fatalf("engine pickup start report before any scheduler pass = %d (err=%v), want 1", startRows, err)
	}
	if err := f.engine.Store().InTx(f.ctx, func(ctx context.Context, tx engine.Tx) error {
		if err := tx.UpdateRunState(ctx, runID, engine.RunCompleted, nil); err != nil {
			return err
		}
		_, err := tx.AppendEvent(ctx, runID, engine.EventInput{Type: engine.TypeRunCompleted, Data: json.RawMessage(`{"run_id":"` + runID + `"}`)})
		return err
	}); err != nil {
		t.Fatalf("finish run before report pass: %v", err)
	}

	if err := ticketreport.New(f.store, nil).Run(f.ctx); err != nil {
		t.Fatalf("dispatch reports: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(comments) != 3 {
		t.Fatalf("bridge comments = %d, want start, page link, and finish", len(comments))
	}
	if !strings.Contains(comments[0], "started run") || !strings.Contains(comments[0], runID) || !strings.Contains(comments[0], delivery.Event.ID) {
		t.Errorf("start comment = %q", comments[0])
	}
	if comments[1] != "culture-nodes page: https://nodes.example/tickets/SCRUM-203 [culture-nodes:ticket-page-link]" {
		t.Errorf("page-link comment = %q", comments[1])
	}
	if !strings.Contains(comments[2], "finished run") || !strings.Contains(comments[2], "completed") {
		t.Errorf("finish comment = %q", comments[2])
	}
}

func TestTwoTicketStartReportsEnqueueOnePageLink(t *testing.T) {
	t.Setenv("NODES_UI_BASE_URL", "")
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	first := deliverSubjectEvent(t, f, "SCRUM-203", "jira:team.example.com:SCRUM-203:status:1")
	firstRunID := first.Triggered[0].RunID
	if err := f.engine.Store().InTx(f.ctx, func(ctx context.Context, tx engine.Tx) error {
		if err := tx.UpdateRunState(ctx, firstRunID, engine.RunCompleted, nil); err != nil {
			return err
		}
		_, err := tx.AppendEvent(ctx, firstRunID, engine.EventInput{Type: engine.TypeRunCompleted, Data: json.RawMessage(`{}`)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	deliverSubjectEvent(t, f, "SCRUM-203", "jira:team.example.com:SCRUM-203:status:2")

	var starts, links int
	var runIDIsNull bool
	var payload []byte
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT
		count(*) FILTER (WHERE phase='start'), count(*) FILTER (WHERE phase='page-link')
		FROM jira_ticket_report_outbox WHERE namespace_id=$1 AND issue_key='SCRUM-203'`, f.ns.ID).Scan(&starts, &links); err != nil {
		t.Fatal(err)
	}
	if starts != 2 || links != 1 {
		t.Fatalf("outbox start/page-link rows = %d/%d, want 2/1", starts, links)
	}
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT run_id IS NULL,payload FROM jira_ticket_report_outbox
		WHERE namespace_id=$1 AND issue_key='SCRUM-203' AND phase='page-link'`, f.ns.ID).Scan(&runIDIsNull, &payload); err != nil {
		t.Fatal(err)
	}
	var intent struct {
		Verb, Issue, Comment, Phase string
	}
	if err := json.Unmarshal(payload, &intent); err != nil {
		t.Fatal(err)
	}
	if !runIDIsNull || intent.Verb != "post_comment" || intent.Issue != "SCRUM-203" || intent.Phase != "page-link" ||
		intent.Comment != "culture-nodes page: /tickets/SCRUM-203 [culture-nodes:ticket-page-link]" {
		t.Fatalf("page-link run_id_null/payload = %v/%+v", runIDIsNull, intent)
	}
}

func deliverSubjectEvent(t *testing.T, f *fixture, subject, sourceKey string) storepg.SignalDelivery {
	t.Helper()
	delivery, err := f.store.DeliverSignalEvent(f.ctx, storepg.DeliverSignalEventInput{
		NamespaceID: f.ns.ID,
		Name:        "test.subject-event",
		Payload:     json.RawMessage(`{}`),
		Emitter:     "test",
		Subject:     subject,
		// A distinct SourceKey/Watermark per call: that pair's idempotency is
		// for an emitter's exact redelivery of the SAME fact, which is not
		// what this test is about -- these are meant to be recorded as two
		// different, genuinely new facts.
		SourceKey: sourceKey,
		Watermark: json.RawMessage(`{"seq":"` + sourceKey + `"}`),
		Trigger:   f.engine,
	})
	if err != nil {
		t.Fatalf("DeliverSignalEvent(subject=%s, sourceKey=%s): %v", subject, sourceKey, err)
	}
	return delivery
}

// TestTriggerEventAttachesSecondEventToExistingRunForSameSubject is the
// acceptance test for h16: two events on one subject during a single flight
// yield exactly one active run, and the second event's effect is visible on
// that run rather than a sibling.
func TestTriggerEventAttachesSecondEventToExistingRunForSameSubject(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	first := deliverSubjectEvent(t, f, "SCRUM-1", "state-change")
	if len(first.Triggered) != 1 {
		t.Fatalf("first delivery: want 1 triggered entry, got %d (%+v)", len(first.Triggered), first.Triggered)
	}
	if first.Triggered[0].Attached {
		t.Fatalf("first delivery for a fresh subject must create a NEW run, not attach: %+v", first.Triggered[0])
	}
	runID := first.Triggered[0].RunID
	if runID == "" {
		t.Fatal("first delivery reported an empty run id")
	}

	second := deliverSubjectEvent(t, f, "SCRUM-1", "comment")
	if len(second.Triggered) != 1 {
		t.Fatalf("second delivery: want 1 triggered entry, got %d (%+v)", len(second.Triggered), second.Triggered)
	}
	if !second.Triggered[0].Attached {
		t.Fatalf("second delivery on the SAME subject during the SAME flight must attach, not create a sibling: %+v", second.Triggered[0])
	}
	if second.Triggered[0].RunID != runID {
		t.Fatalf("second delivery attached to a different run: got %s, want the first run %s", second.Triggered[0].RunID, runID)
	}

	// Acceptance: "Two events on one issue during a single flight yield
	// exactly one active run in the run list."
	runCount := f.countScalar(`SELECT COUNT(*)::int FROM runs WHERE namespace_id = $1 AND subject = $2`, f.ns.ID, "SCRUM-1")
	if runCount != 1 {
		t.Fatalf("want exactly 1 run recorded for subject SCRUM-1, got %d", runCount)
	}

	run := f.run(runID)
	if run.State.Terminal() {
		t.Fatalf("the run the second event should have attached to is unexpectedly terminal: %s", run.State)
	}
	if run.Subject != "SCRUM-1" {
		t.Fatalf("run.Subject = %q, want SCRUM-1", run.Subject)
	}

	// Acceptance: "The second event's effect is visible on the existing run,
	// not a sibling." Confirmed two ways -- the run count above (no sibling
	// exists to be visible on), and here: the SECOND event's own audit trace
	// lands on THIS run's own stream, in order beside everything else that
	// happened to it.
	types := f.eventTypes(runID)
	var attachedCount int
	for _, typ := range types {
		if typ == engine.TypeTriggerEventAttached {
			attachedCount++
		}
	}
	if attachedCount != 1 {
		t.Fatalf("want exactly 1 %s event on run %s, got %d (stream: %v)", engine.TypeTriggerEventAttached, runID, attachedCount, types)
	}
}

// TestTriggerEventDoesNotDedupAcrossDifferentSubjects proves the guard is
// scoped to the subject, not a blanket "one run per workflow": two DIFFERENT
// Jira issues must still get two independent runs.
func TestTriggerEventDoesNotDedupAcrossDifferentSubjects(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	a := deliverSubjectEvent(t, f, "SCRUM-1", "a")
	b := deliverSubjectEvent(t, f, "SCRUM-2", "b")

	if len(a.Triggered) != 1 || len(b.Triggered) != 1 {
		t.Fatalf("want 1 triggered entry each, got %d and %d", len(a.Triggered), len(b.Triggered))
	}
	if a.Triggered[0].Attached || b.Triggered[0].Attached {
		t.Fatalf("different subjects must each create their own run: %+v %+v", a.Triggered[0], b.Triggered[0])
	}
	if a.Triggered[0].RunID == b.Triggered[0].RunID {
		t.Fatalf("different subjects produced the SAME run id %s", a.Triggered[0].RunID)
	}
}

// TestTriggerEventCreatesNewRunOnceSubjectRunIsTerminal proves the guard is
// a FLIGHT dedup, not a permanent one: once the earlier run for a subject has
// finished, a later matching event is free to open a new one.
func TestTriggerEventCreatesNewRunOnceSubjectRunIsTerminal(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	first := deliverSubjectEvent(t, f, "SCRUM-1", "a")
	runID := first.Triggered[0].RunID

	err := f.engine.Store().InTx(f.ctx, func(ctx context.Context, tx engine.Tx) error {
		return tx.UpdateRunState(ctx, runID, engine.RunCancelled, nil)
	})
	if err != nil {
		t.Fatalf("cancel run %s: %v", runID, err)
	}

	second := deliverSubjectEvent(t, f, "SCRUM-1", "b")
	if len(second.Triggered) != 1 {
		t.Fatalf("want 1 triggered entry, got %d (%+v)", len(second.Triggered), second.Triggered)
	}
	if second.Triggered[0].Attached {
		t.Fatalf("a terminal run must not receive an attach: %+v", second.Triggered[0])
	}
	if second.Triggered[0].RunID == runID {
		t.Fatalf("expected a brand-new run once the earlier one for this subject was terminal")
	}
}

// TestTriggerEventWithNoSubjectAlwaysCreatesANewRun is the compatibility
// floor: a caller that never sets subject (every caller that predates task
// t15) gets exactly the pre-existing behavior back -- a run every time.
func TestTriggerEventWithNoSubjectAlwaysCreatesANewRun(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	deliver := func(sourceKey string) storepg.SignalDelivery {
		t.Helper()
		d, err := f.store.DeliverSignalEvent(f.ctx, storepg.DeliverSignalEventInput{
			NamespaceID: f.ns.ID,
			Name:        "test.subject-event",
			Payload:     json.RawMessage(`{}`),
			Emitter:     "test",
			SourceKey:   sourceKey,
			Watermark:   json.RawMessage(`{"seq":"` + sourceKey + `"}`),
			Trigger:     f.engine,
		})
		if err != nil {
			t.Fatalf("DeliverSignalEvent: %v", err)
		}
		return d
	}

	first := deliver("a")
	second := deliver("b")
	if first.Triggered[0].Attached || second.Triggered[0].Attached {
		t.Fatalf("no subject means no dedup: %+v %+v", first.Triggered[0], second.Triggered[0])
	}
	if first.Triggered[0].RunID == second.Triggered[0].RunID {
		t.Fatalf("subject-less deliveries must not be deduped against each other")
	}
}

// TestFailingTicketReportBacksOffAndDoesNotBlockLaterReports is the
// regression test for the PR #208 head-of-line finding: the dispatcher
// drains the globally oldest eligible row, so a report whose bridge
// dispatch fails must be recorded (attempts+1, future available_at,
// status='failed' once the attempt budget is spent) rather than left as the
// eligible head where it would block every later report on each pass.
func TestFailingTicketReportBacksOffAndDoesNotBlockLaterReports(t *testing.T) {
	f := newFixture(t, "trigger-subject.workflow.yaml")
	publishFixtureWorkflow(t, f)

	var mu sync.Mutex
	var posted []string
	var badCalls int
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var invocation struct {
			Input struct{ Verb, Issue, Comment string } `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&invocation); err != nil {
			t.Errorf("decode invocation: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if invocation.Input.Issue == "SCRUM-BAD" {
			badCalls++
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		posted = append(posted, invocation.Input.Issue)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"outcome":"comment_posted","output":{},"ledger_delta":{"records":[]}}`))
	}))
	defer bridge.Close()
	if _, err := f.store.Pool().Exec(f.ctx, `INSERT INTO actors
		(id,namespace_id,actor_key,revision,kind,protocol,endpoint_ref)
		VALUES ($1,$2,$3,1,'bridge','actor-protocol',$4)`, store.NewULID(), f.ns.ID, storepg.JiraTicketReporterActorKey, bridge.URL); err != nil {
		t.Fatalf("register Jira bridge: %v", err)
	}

	deliverSubjectEvent(t, f, "SCRUM-BAD", "jira:team.example.com:SCRUM-BAD:status")
	deliverSubjectEvent(t, f, "SCRUM-OK", "jira:team.example.com:SCRUM-OK:status")
	// Pin the poison row to the head of the ORDER BY id drain so the test
	// exercises exactly the blocked-queue shape, whatever the ULIDs drew.
	if _, err := f.store.Pool().Exec(f.ctx, `UPDATE jira_ticket_report_outbox
		SET id='00000000000000000000000000' WHERE issue_key='SCRUM-BAD'`); err != nil {
		t.Fatalf("pin poison row: %v", err)
	}

	if err := ticketreport.New(f.store, nil).Run(f.ctx); err == nil {
		t.Fatal("Run with a failing report returned nil, want the recorded per-row failure joined in")
	}
	mu.Lock()
	if len(posted) != 1 || posted[0] != "SCRUM-OK" {
		t.Fatalf("posted = %v, want the later SCRUM-OK report to drain past the failing head", posted)
	}
	mu.Unlock()
	var attempts int
	var status string
	var backedOff bool
	if err := f.store.Pool().QueryRow(f.ctx, `SELECT attempts,status,available_at>now()
		FROM jira_ticket_report_outbox WHERE issue_key='SCRUM-BAD'`).Scan(&attempts, &status, &backedOff); err != nil {
		t.Fatalf("read poison row: %v", err)
	}
	if attempts != 1 || status != "pending" || !backedOff {
		t.Fatalf("poison row after one failure = (attempts=%d, status=%q, backedOff=%v), want (1, pending, true)", attempts, status, backedOff)
	}

	// Burn through the remaining budget: each pass re-fails and backs off,
	// and the row dead-letters once its attempts are spent.
	for status == "pending" {
		if attempts > 10 {
			t.Fatalf("poison row never dead-lettered: attempts=%d status=%q", attempts, status)
		}
		if _, err := f.store.Pool().Exec(f.ctx, `UPDATE jira_ticket_report_outbox
			SET available_at=now()-interval '1 second' WHERE issue_key='SCRUM-BAD'`); err != nil {
			t.Fatalf("re-arm poison row: %v", err)
		}
		_ = ticketreport.New(f.store, nil).Run(f.ctx)
		if err := f.store.Pool().QueryRow(f.ctx, `SELECT attempts,status FROM jira_ticket_report_outbox
			WHERE issue_key='SCRUM-BAD'`).Scan(&attempts, &status); err != nil {
			t.Fatalf("read poison row: %v", err)
		}
	}
	if status != "failed" || attempts != 5 {
		t.Fatalf("poison row terminal state = (attempts=%d, status=%q), want (5, failed)", attempts, status)
	}
	mu.Lock()
	callsAtDeadLetter := badCalls
	mu.Unlock()

	// A dead-lettered row is out of the queue for good: another pass makes
	// no further bridge call for it and has nothing left to report.
	if err := ticketreport.New(f.store, nil).Run(f.ctx); err != nil {
		t.Fatalf("Run after dead-letter: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if badCalls != callsAtDeadLetter {
		t.Fatalf("dead-lettered report was re-dispatched: %d bridge calls after, %d at dead-letter", badCalls, callsAtDeadLetter)
	}
}
