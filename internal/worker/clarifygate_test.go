package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/preflight"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// The clarify-then-commit gate at the dispatch site (issue #67, task t14).
//
// The acceptance these tests carry:
//
//  1. a derived preflight record exists as a ledger record BEFORE the first
//     billable turn — proven by the actor never being invoked while it does;
//  2. a dispatch whose preflight was never acknowledged does not proceed —
//     it is refused when the window closes, and the actor is still never
//     invoked;
//  3. an acknowledged preflight lets exactly one dispatch through and is
//     spent doing it;
//  4. an actor that does not configure the gate dispatches exactly as it did
//     before this existed.

// withRegisteredActor replaces the harness's StaticRegistry with the REAL
// DBRegistry over a REAL actors row, carrying the capabilities and metadata
// a test wants to try. That is deliberate rather than convenient: the gate's
// configuration comes from the actors table through
// DBRegistry.PreflightConfig, and a hand-rolled fake registry would test the
// gate against a seam no deployment uses.
//
// The endpoint comes out of the StaticRegistry the harness already built, so
// the registered row points at the same test actor server.
func withRegisteredActor(t *testing.T, capabilities, metadata string) harnessOption {
	t.Helper()
	return func(o *worker.Options) {
		base, ok := o.Registry.(worker.StaticRegistry)
		if !ok {
			t.Fatalf("harness registry is %T, want a StaticRegistry to read the actor endpoint from", o.Registry)
		}
		endpoint := base["actor://company/analyzer"].URL

		store, err := storepg.NewEngineStore(testStore, o.NamespaceID)
		if err != nil {
			t.Fatalf("NewEngineStore: %v", err)
		}
		if _, err := store.RegisterActor(context.Background(), storepg.RegisterActorParams{
			ActorKey:     "company/analyzer",
			Kind:         "agent",
			Protocol:     "http",
			EndpointRef:  endpoint,
			Capabilities: json.RawMessage(capabilities),
			Metadata:     json.RawMessage(metadata),
		}); err != nil {
			t.Fatalf("RegisterActor: %v", err)
		}

		registry, err := worker.NewDBRegistry(testStore, o.NamespaceID)
		if err != nil {
			t.Fatalf("NewDBRegistry: %v", err)
		}
		o.Registry = registry

		// The gate's own producer identity. ledger_records.origin_actor_id
		// references actors(id), so even a deterministic producer writes as
		// a registered identity — a real deployment has the same obligation
		// (see Options.DispatchGateActorID). actors.id is a global primary
		// key, so each harness registers its own.
		gateActorID := "engine-dispatch-gate-" + idstore.NewULID()
		if _, err := testStore.Pool().Exec(context.Background(), `
			INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
			VALUES ($1, $2, $3, 1, 'engine', 'internal')
		`, gateActorID, o.NamespaceID, gateActorID); err != nil {
			t.Fatalf("register the dispatch gate producer: %v", err)
		}
		o.DispatchGateActorID = gateActorID
	}
}

const testHostCapabilities = `{"preflight":{"protocol_version":"1.0","host":{` +
	`"hostname":"test-host",` +
	`"sandbox_modes":["read-only"],` +
	`"commit_policy":"harvest: the session never runs git commit"` +
	`}}}`

// withGatedActor registers the harness's actor with the gate enabled.
func withGatedActor(t *testing.T) harnessOption {
	return withRegisteredActor(t, testHostCapabilities, `{"preflight_gate":{"enabled":true}}`)
}

// completesSynchronously is the actor behaviour every test in this file
// wants: the gate is about what happens BEFORE the invocation, so what the
// actor would have answered is deliberately uninteresting.
func completesSynchronously(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
	writeSyncResult(w, "completed", `{"score":0.9,"summary":"done"}`)
}

// releaseDeferred brings a deferred work item forward, so a test does not
// have to wait out the recheck interval to observe the next decision.
func releaseDeferred(t *testing.T, h *harness, runID, nodeKey string) {
	t.Helper()
	if _, err := h.store.Pool().Exec(h.ctx,
		`UPDATE work_items SET available_at = now() WHERE id = $1`, workItemOf(t, h, runID, nodeKey)); err != nil {
		t.Fatalf("bring the deferred item forward: %v", err)
	}
}

// preflightRows reads the gate's durable rows for a run.
func preflightRows(t *testing.T, h *harness, runID string) []storepg.Preflight {
	t.Helper()
	rows, err := h.store.Pool().Query(h.ctx, `
		SELECT id FROM dispatch_preflights WHERE namespace_id = $1 AND run_id = $2 ORDER BY issued_at
	`, h.ns.ID, runID)
	if err != nil {
		t.Fatalf("read dispatch_preflights: %v", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan preflight id: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	out := make([]storepg.Preflight, 0, len(ids))
	for _, id := range ids {
		p, err := h.store.Preflight(h.ctx, h.ns.ID, id)
		if err != nil {
			t.Fatalf("read preflight %s: %v", id, err)
		}
		out = append(out, p)
	}
	return out
}

// ledgerRecordsOfType reads a run's ledger records of one type.
func ledgerRecordsOfType(t *testing.T, h *harness, runID string, recordType ledger.RecordType) []ledger.Record {
	t.Helper()
	l, err := storepg.NewLedger(h.store, h.ns.ID)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	all, err := l.Records(h.ctx, runID)
	if err != nil {
		t.Fatalf("read ledger records: %v", err)
	}
	var out []ledger.Record
	for _, rec := range all {
		if rec.RecordType == recordType {
			out = append(out, rec)
		}
	}
	return out
}

// acknowledge does what POST /v1alpha1/preflights/{id}/acknowledge does: it
// appends the actor's proposed claim and then marks the row. The API handler
// and this helper build the record through the same builder
// (preflight.NewAcknowledgementRecord), so there is one definition of the
// record's shape and this test is not a second implementation of it.
func acknowledge(t *testing.T, h *harness, row storepg.Preflight) ledger.Record {
	t.Helper()
	l, err := storepg.NewLedger(h.store, h.ns.ID)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	rec, err := preflight.NewAcknowledgementRecord(preflight.AcknowledgementInput{
		RunID:             row.RunID,
		NodeRunID:         row.NodeRunID,
		PreflightRecordID: row.RecordID,
		PreflightDigest:   row.RecordDigest,
		OriginKind:        ledger.OriginAgent,
		OriginActorID:     row.ActorID,
		AcknowledgedBy:    row.ActorID,
		Note:              "read the host capabilities and the expected terminal shape",
	})
	if err != nil {
		t.Fatalf("NewAcknowledgementRecord: %v", err)
	}
	appended, err := l.Append(h.ctx, rec)
	if err != nil {
		t.Fatalf("append acknowledgement: %v", err)
	}
	if _, err := h.store.AcknowledgePreflight(h.ctx, storepg.AcknowledgePreflightInput{
		NamespaceID:             h.ns.ID,
		ID:                      row.ID,
		AcknowledgedBy:          row.ActorID,
		AcknowledgementRecordID: appended.ID,
		Now:                     time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AcknowledgePreflight: %v", err)
	}
	return appended
}

// TestAGatedDispatchIsBriefedBeforeItIsBilled is acceptance criterion 1: the
// derived preflight record exists, and the actor has not been invoked.
func TestAGatedDispatchIsBriefedBeforeItIsBilled(t *testing.T) {
	h := newHarness(t, completesSynchronously, withGatedActor(t))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	for i := 0; i < 3; i++ {
		if _, err := h.worker.Tick(h.ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
	}

	if got := len(h.invocations()); got != 0 {
		t.Fatalf("actor was invoked %d times before acknowledging its preflight, want 0", got)
	}

	records := ledgerRecordsOfType(t, h, run.ID, ledger.RecordDispatchPreflight)
	if len(records) != 1 {
		t.Fatalf("run has %d dispatch_preflight records, want exactly 1", len(records))
	}
	rec := records[0]
	if rec.Authority != ledger.AuthorityDerived || rec.Origin.Kind != ledger.OriginEngine {
		t.Errorf("preflight record origin/authority = %s/%s, want engine/derived",
			rec.Origin.Kind, rec.Authority)
	}

	// The document states the facts the actor's task depends on, as the
	// bridge advertised them.
	var doc preflight.Document
	if err := json.Unmarshal(rec.Data, &doc); err != nil {
		t.Fatalf("decode preflight document: %v", err)
	}
	if doc.Verdict != preflight.VerdictHold {
		t.Errorf("verdict = %q, want hold", doc.Verdict)
	}
	if doc.Task.NodeID != "analyze" || doc.Task.ActorKey != "company/analyzer" {
		t.Errorf("task declaration = %+v, want the dispatch being briefed", doc.Task)
	}
	var host map[string]any
	if err := json.Unmarshal(doc.HostCapabilities, &host); err != nil {
		t.Fatalf("decode host capabilities: %v", err)
	}
	if host["hostname"] != "test-host" || host["commit_policy"] == nil {
		t.Errorf("host capabilities = %v, want the advertised block verbatim", host)
	}

	// No acknowledgement exists yet — nothing has claimed to understand it.
	if acks := ledgerRecordsOfType(t, h, run.ID, ledger.RecordDispatchAcknowledgement); len(acks) != 0 {
		t.Errorf("run has %d acknowledgements before anyone acknowledged, want 0", len(acks))
	}

	// The work item is DEFERRED, not failed and not parked: the run is still
	// live, and the deferral gave back the dispatch counter because nothing
	// was dispatched.
	state, availableAt, attempt := workItemAvailability(t, h, run.ID, "analyze")
	if state != "ready" {
		t.Errorf("work item state = %q, want ready: a gated dispatch is deferred, not parked or failed", state)
	}
	if !availableAt.After(time.Now()) {
		t.Errorf("available_at = %s, want pushed forward until the acknowledgement can arrive", availableAt)
	}
	if attempt != 0 {
		t.Errorf("work item attempt = %d, want 0: waiting for an acknowledgement is not a dispatch", attempt)
	}
	if h.run(run.ID).State.Terminal() {
		t.Error("the run ended while waiting for an acknowledgement; a gate holds work, it does not kill it")
	}

	// And it is explainable from the run's own event stream.
	if types := runEventTypes(t, h, run.ID); !hasEvent(types, worker.TypePreflightIssued) {
		t.Errorf("run events = %v, want %s recorded", types, worker.TypePreflightIssued)
	}
	issued := runEventData(t, h, run.ID, worker.TypePreflightIssued)
	if issued["actor_key"] != "company/analyzer" || issued["preflight_id"] == nil || issued["expires_at"] == nil {
		t.Errorf("%s payload = %v, want the actor, the preflight and its window", worker.TypePreflightIssued, issued)
	}
}

// TestAnAcknowledgedPreflightLetsExactlyOneDispatchThrough is criterion 1's
// other half plus the single-use property: the acknowledgement is a proposed
// record by the actor, and it is spent by the dispatch it authorized.
func TestAnAcknowledgedPreflightLetsExactlyOneDispatchThrough(t *testing.T) {
	h := newHarness(t, completesSynchronously, withGatedActor(t))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	rows := preflightRows(t, h, run.ID)
	if len(rows) != 1 {
		t.Fatalf("run has %d preflight rows, want 1", len(rows))
	}
	ack := acknowledge(t, h, rows[0])
	if ack.Authority != ledger.AuthorityProposed || ack.Origin.Kind != ledger.OriginAgent {
		t.Errorf("acknowledgement origin/authority = %s/%s, want agent/proposed — an actor saying it "+
			"understood is a claim, not evidence", ack.Origin.Kind, ack.Authority)
	}

	// Bring the deferred item forward rather than waiting out the recheck
	// interval, then let the run finish.
	releaseDeferred(t, h, run.ID, "analyze")
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if got := len(h.invocations()); got != 1 {
		t.Fatalf("actor was invoked %d times, want exactly 1", got)
	}

	spent := preflightRows(t, h, run.ID)[0]
	if !spent.Consumed() {
		t.Error("the acknowledgement was not consumed by the dispatch it authorized")
	}
	if spent.ConsumedByAttemptID == "" {
		t.Error("the consumed row does not name the attempt that rode it")
	}
	if spent.AcknowledgementRecordID != ack.ID {
		t.Errorf("row points at acknowledgement %q, want %q", spent.AcknowledgementRecordID, ack.ID)
	}
}

// TestADispatchWhosePreflightWasNeverAcknowledgedDoesNotProceed is
// acceptance criterion 2. The window closing is what turns "not yet" into a
// refusal, so a run cannot sit forever against a bridge that never answers.
func TestADispatchWhosePreflightWasNeverAcknowledgedDoesNotProceed(t *testing.T) {
	h := newHarness(t, completesSynchronously, withGatedActor(t))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	if _, err := h.worker.Tick(h.ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	rows := preflightRows(t, h, run.ID)
	if len(rows) != 1 {
		t.Fatalf("run has %d preflight rows, want 1", len(rows))
	}

	// Close the window without anyone acknowledging. Waiting out the real
	// default would mean a fifteen-minute test; expiring the row is the one
	// thing simulated here.
	if _, err := h.store.Pool().Exec(h.ctx,
		`UPDATE dispatch_preflights SET expires_at = now() - interval '1 second' WHERE id = $1`,
		rows[0].ID); err != nil {
		t.Fatalf("expire the preflight: %v", err)
	}
	releaseDeferred(t, h, run.ID, "analyze")
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if got := len(h.invocations()); got != 0 {
		t.Fatalf("actor was invoked %d times for an unacknowledged dispatch, want 0", got)
	}

	status, result := attemptRecord(t, h, run.ID, "analyze")
	if engine.TechStatus(status) != engine.StatusPolicyDenied {
		t.Errorf("attempt status = %q, want policy_denied: a declared policy refused the dispatch", status)
	}
	if !bytes.Contains(result, []byte(worker.PreflightRefusalClass)) {
		t.Errorf("attempt result = %s, want it to name the refusal class %q", result, worker.PreflightRefusalClass)
	}
	if types := runEventTypes(t, h, run.ID); !hasEvent(types, worker.TypeDispatchRefused) {
		t.Errorf("run events = %v, want %s recorded", types, worker.TypeDispatchRefused)
	}
	// Still exactly one briefing and no acknowledgement: a refused window
	// does not silently re-brief, and nothing ever claimed to understand.
	if records := ledgerRecordsOfType(t, h, run.ID, ledger.RecordDispatchPreflight); len(records) != 1 {
		t.Errorf("run has %d preflight records after the refusal, want 1", len(records))
	}
	if acks := ledgerRecordsOfType(t, h, run.ID, ledger.RecordDispatchAcknowledgement); len(acks) != 0 {
		t.Errorf("run has %d acknowledgements, want 0", len(acks))
	}
}

// TestAnUngatedActorDispatchesUnchanged is acceptance criterion 3's second
// half at the dispatch site: an actor whose registration says nothing about
// the gate — every actor registered before this shipped — is untouched.
func TestAnUngatedActorDispatchesUnchanged(t *testing.T) {
	// A registration that says nothing about the gate — the shape all ten
	// actors registered today have.
	h := newHarness(t, completesSynchronously, withRegisteredActor(t, `{}`, `{}`))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if got := len(h.invocations()); got != 1 {
		t.Fatalf("actor was invoked %d times, want 1: an ungated actor dispatches exactly as before", got)
	}
	if rows := preflightRows(t, h, run.ID); len(rows) != 0 {
		t.Errorf("an ungated dispatch issued %d preflights, want 0", len(rows))
	}
	if records := ledgerRecordsOfType(t, h, run.ID, ledger.RecordDispatchPreflight); len(records) != 0 {
		t.Errorf("an ungated dispatch wrote %d preflight records, want 0", len(records))
	}
}
