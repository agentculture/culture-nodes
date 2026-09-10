package worker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/repair"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// Lane liveness at the dispatch site (plan loop-closure t10; spec c4, c23,
// c26, c33, honesty h13/h20; decision c43).
//
// The acceptance these carry, end to end against a real PostgreSQL, a real
// DBRegistry over real actors rows, and two real HTTP actors:
//
//  1. fresh false row + registered fallback -> the lease goes to the
//     fallback, the primary is never invoked, the ledger has the derived
//     routing record naming both actors, and the attempt is attributed to
//     the fallback;
//  2. fresh false row without a fallback -> the lease proceeds and a
//     warning-shaped record exists;
//  3. stale CHECK-mode false row -> proceeds, no record;
//  4. a locked row of ANY age refuses the lane;
//  5. resume alone does not reopen the lane; resume followed by a healthy
//     collector write does;
//  6. a failed attempt with class credential_spent writes the locked row;
//  7. a worker process started AFTER the row was written still refuses from
//     the persisted row — nothing about the decision lives in memory.

// fallbackActor is a second real HTTP actor that counts its own
// invocations, so a test can tell which lane the lease went to.
type fallbackActor struct {
	server *httptest.Server
	hits   atomic.Int32
}

func newFallbackActor(t *testing.T) *fallbackActor {
	t.Helper()
	fb := &fallbackActor{}
	fb.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.hits.Add(1)
		writeSyncResult(w, "completed", `{"score":0.5,"summary":"from the fallback lane"}`)
	}))
	t.Cleanup(fb.server.Close)
	return fb
}

// livenessLane is what withLivenessLanes registered, for the tests to read.
type livenessLane struct {
	gateActorID   string
	primaryRowID  string
	fallbackRowID string
}

// withLivenessLanes replaces the harness's StaticRegistry with the REAL
// DBRegistry over two REAL actors rows: company/analyzer (the node's own
// lane, pointing at the harness actor server) carrying the metadata a test
// wants, and — when fallbackURL is non-empty — company/fallback pointing at
// the second actor. The router's producer identity is registered too, the
// same way clarifygate_test.go's withRegisteredActor does.
func withLivenessLanes(t *testing.T, primaryMetadata, fallbackURL string, out *livenessLane) harnessOption {
	t.Helper()
	return func(o *worker.Options) {
		base, ok := o.Registry.(worker.StaticRegistry)
		if !ok {
			t.Fatalf("harness registry is %T, want a StaticRegistry", o.Registry)
		}
		ctx := context.Background()
		store, err := storepg.NewEngineStore(testStore, o.NamespaceID)
		if err != nil {
			t.Fatalf("NewEngineStore: %v", err)
		}
		primary, err := store.RegisterActor(ctx, storepg.RegisterActorParams{
			ActorKey: "company/analyzer", Kind: "agent", Protocol: "http",
			EndpointRef: base["actor://company/analyzer"].URL,
			Metadata:    json.RawMessage(primaryMetadata),
		})
		if err != nil {
			t.Fatalf("RegisterActor(primary): %v", err)
		}
		out.primaryRowID = primary.ID
		if fallbackURL != "" {
			fb, err := store.RegisterActor(ctx, storepg.RegisterActorParams{
				ActorKey: "company/fallback", Kind: "agent", Protocol: "http", EndpointRef: fallbackURL,
			})
			if err != nil {
				t.Fatalf("RegisterActor(fallback): %v", err)
			}
			out.fallbackRowID = fb.ID
		}
		registry, err := worker.NewDBRegistry(testStore, o.NamespaceID)
		if err != nil {
			t.Fatalf("NewDBRegistry: %v", err)
		}
		o.Registry = registry

		gateActorID := "engine-dispatch-gate-" + idstore.NewULID()
		if _, err := testStore.Pool().Exec(ctx, `
			INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
			VALUES ($1, $2, $3, 1, 'engine', 'internal')
		`, gateActorID, o.NamespaceID, gateActorID); err != nil {
			t.Fatalf("register the router producer: %v", err)
		}
		o.DispatchGateActorID = gateActorID
		out.gateActorID = gateActorID
	}
}

const withFallbackMetadata = `{"fallback_actor":"company/fallback"}`

// observeNotLive writes what the mesh collector would have persisted for a
// bridge reporting session_ok=false at checkedAt.
func observeNotLive(t *testing.T, h *harness, actorKey string, checkedAt time.Time) {
	t.Helper()
	observeNotLiveInMode(t, h, actorKey, checkedAt, storepg.LivenessModeLock)
}

// observeNotLiveInMode is observeNotLive with the bridge's detection mode
// spelled out, because the mode decides whether checkedAt's age means
// anything (postgres.ActorLiveness.Live).
func observeNotLiveInMode(t *testing.T, h *harness, actorKey string, checkedAt time.Time, mode string) {
	t.Helper()
	notOK := false
	if _, err := h.store.RecordActorLiveness(h.ctx, storepg.RecordActorLivenessInput{
		NamespaceID: h.ns.ID, ActorKey: actorKey, SessionOK: &notOK, Reason: "refresh_token_spent", Mode: mode,
		CheckedAt: checkedAt, Source: storepg.LivenessSourceCollector,
	}); err != nil {
		t.Fatalf("RecordActorLiveness: %v", err)
	}
}

func observeLive(t *testing.T, h *harness, actorKey string) {
	t.Helper()
	ok := true
	if _, err := h.store.RecordActorLiveness(h.ctx, storepg.RecordActorLivenessInput{
		NamespaceID: h.ns.ID, ActorKey: actorKey, SessionOK: &ok, Reason: "ok", Mode: "LOCK",
		CheckedAt: time.Now().UTC(), Source: storepg.LivenessSourceCollector,
	}); err != nil {
		t.Fatalf("RecordActorLiveness: %v", err)
	}
}

// livenessRoutings reads the run's lane_liveness routing records.
func livenessRoutings(t *testing.T, h *harness, runID string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range ledgerRecordsOfType(t, h, runID, ledger.RecordDecision) {
		data, err := rec.DataMap()
		if err != nil {
			t.Fatal(err)
		}
		if data["router"] == repair.LivenessRouterMethod {
			if rec.Authority != ledger.AuthorityDerived || rec.Origin.Kind != ledger.OriginValidator {
				t.Errorf("routing record is %s/%s, want derived/validator", rec.Authority, rec.Origin.Kind)
			}
			out = append(out, data)
		}
	}
	return out
}

func TestFreshFalseRowWithAFallbackRoutesTheLeaseToTheFallbackAndRecordsIt(t *testing.T) {
	fb := newFallbackActor(t)
	var lanes livenessLane
	h := newHarness(t, completesSynchronously, withLivenessLanes(t, withFallbackMetadata, fb.server.URL, &lanes))
	observeNotLive(t, h, "company/analyzer", time.Now().UTC().Add(-30*time.Second))

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if got := len(h.invocations()); got != 0 {
		t.Fatalf("the primary lane was invoked %d times, want 0: its session cannot start", got)
	}
	if got := fb.hits.Load(); got != 1 {
		t.Fatalf("the fallback lane was invoked %d times, want 1", got)
	}
	if state := h.run(run.ID).State; state != engine.RunCompleted {
		t.Fatalf("run state = %s, want completed: the fallback answered the node's own contract", state)
	}
	if got := attemptActorID(t, h, run.ID, "analyze"); got == nil || *got != lanes.fallbackRowID {
		t.Errorf("attempt attributed to %v, want the fallback's row %q", got, lanes.fallbackRowID)
	}

	routings := livenessRoutings(t, h, run.ID)
	if len(routings) != 1 {
		t.Fatalf("lane_liveness routing records = %d, want 1", len(routings))
	}
	rec := routings[0]
	if rec["selected"] != "fallback" || rec["reason"] != string(repair.ReasonLaneNotLive) {
		t.Errorf("selected/reason = %v/%v, want fallback/lane_not_live", rec["selected"], rec["reason"])
	}
	if rec["lane_actor_key"] != "company/analyzer" || rec["fallback_actor_key"] != "company/fallback" {
		t.Errorf("record names %v -> %v, want both actors", rec["lane_actor_key"], rec["fallback_actor_key"])
	}
	types := runEventTypes(t, h, run.ID)
	if !hasEvent(types, worker.TypeLaneRouted) {
		t.Errorf("run events %v carry no %s", types, worker.TypeLaneRouted)
	}
}

func TestFreshFalseRowWithoutAFallbackProceedsWithAWarningRecord(t *testing.T) {
	var lanes livenessLane
	h := newHarness(t, completesSynchronously, withLivenessLanes(t, `{}`, "", &lanes))
	observeNotLive(t, h, "company/analyzer", time.Now().UTC())

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if got := len(h.invocations()); got != 1 {
		t.Fatalf("the primary lane was invoked %d times, want 1: with no fallback the dispatch proceeds", got)
	}
	routings := livenessRoutings(t, h, run.ID)
	if len(routings) != 1 {
		t.Fatalf("lane_liveness records = %d, want 1 warning (worker errors: %v)", len(routings), h.workerErrors())
	}
	if routings[0]["selected"] != "proceed" || routings[0]["severity"] != "warning" {
		t.Errorf("record = %v, want selected=proceed severity=warning", routings[0])
	}
	if _, named := routings[0]["fallback_actor_key"]; named {
		t.Error("warning names a fallback the registration does not have")
	}
}

// A CHECK-mode bridge re-measures on every probe, so a false fact older
// than the window is one nobody re-measured and proceeds. (A LOCK-mode
// false fact does not age out — see liveness_routing_test.go.)
func TestStaleCheckModeFalseRowProceedsWithoutARecord(t *testing.T) {
	fb := newFallbackActor(t)
	var lanes livenessLane
	h := newHarness(t, completesSynchronously, withLivenessLanes(t, withFallbackMetadata, fb.server.URL, &lanes))
	observeNotLiveInMode(t, h, "company/analyzer", time.Now().UTC().Add(-storepg.LivenessFreshness-time.Minute), storepg.LivenessModeCheck)

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if got := len(h.invocations()); got != 1 {
		t.Fatalf("primary invoked %d times, want 1: a stale fact never refuses", got)
	}
	if got := fb.hits.Load(); got != 0 {
		t.Fatalf("fallback invoked %d times on a stale fact, want 0", got)
	}
	if routings := livenessRoutings(t, h, run.ID); len(routings) != 0 {
		t.Fatalf("a stale row produced routing records %v, want none", routings)
	}
}

func TestALockedRowOfAnyAgeRefusesTheLane(t *testing.T) {
	fb := newFallbackActor(t)
	var lanes livenessLane
	h := newHarness(t, completesSynchronously, withLivenessLanes(t, withFallbackMetadata, fb.server.URL, &lanes))
	if _, err := h.store.LockActorLiveness(h.ctx, storepg.LockActorLivenessInput{
		NamespaceID: h.ns.ID, ActorKey: "company/analyzer", Reason: worker.LivenessLockReason,
		CheckedAt: time.Now().UTC().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if got := len(h.invocations()); got != 0 {
		t.Fatalf("primary invoked %d times through a two-day-old lock, want 0: a lock does not expire by time", got)
	}
	if got := fb.hits.Load(); got != 1 {
		t.Fatalf("fallback invoked %d times, want 1", got)
	}
	routings := livenessRoutings(t, h, run.ID)
	if len(routings) != 1 {
		t.Fatalf("routing records = %d, want 1", len(routings))
	}
	if liveness, _ := routings[0]["liveness"].(map[string]any); liveness["locked"] != true {
		t.Errorf("record liveness = %v, want locked=true", liveness)
	}
}

func TestResumeAloneDoesNotReopenTheLaneButAHealthyFactAfterItDoes(t *testing.T) {
	fb := newFallbackActor(t)
	var lanes livenessLane
	h := newHarness(t, completesSynchronously, withLivenessLanes(t, withFallbackMetadata, fb.server.URL, &lanes))
	if _, err := h.store.LockActorLiveness(h.ctx, storepg.LockActorLivenessInput{
		NamespaceID: h.ns.ID, ActorKey: "company/analyzer", Reason: worker.LivenessLockReason, CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// What POST /v1alpha1/actors/{id}/resume does to the row.
	es, err := storepg.NewEngineStore(h.store, h.ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := es.UnlockActorLiveness(h.ctx, "company/analyzer"); err != nil || !ok {
		t.Fatalf("UnlockActorLiveness = (%v, %v)", ok, err)
	}

	first := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(first.ID).State.Terminal() })
	if got := len(h.invocations()); got != 0 {
		t.Fatalf("primary invoked %d times after resume alone, want 0: the last fact is still a fresh session_ok=false", got)
	}
	if got := fb.hits.Load(); got != 1 {
		t.Fatalf("fallback invoked %d times, want 1", got)
	}

	// The bridge's next probe says the session is fine; the collector
	// persists it; the lane is live again.
	observeLive(t, h, "company/analyzer")
	second := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(second.ID).State.Terminal() })
	if got := len(h.invocations()); got != 1 {
		t.Fatalf("primary invoked %d times after resume + healthy fact, want 1", got)
	}
	if got := fb.hits.Load(); got != 1 {
		t.Fatalf("fallback invoked %d times in total, want still 1", got)
	}
	if routings := livenessRoutings(t, h, second.ID); len(routings) != 0 {
		t.Fatalf("a live lane produced routing records %v", routings)
	}
}

// credentialSpent answers the §13.5 body-declared class the bridge sends
// for a revoked refresh token (issue #308). The status is an ordinary 500:
// the class comes from the body alone.
func credentialSpent(_ *harness, w http.ResponseWriter, _ actors.InvocationRequest) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"error":"Your access token could not be refreshed because your refresh token was revoked","class":"credential_spent"}`))
}

func TestACredentialSpentAttemptLocksTheLane(t *testing.T) {
	h := newHarness(t, credentialSpent)
	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	h.runUntil(20*time.Second, func() bool { return h.run(run.ID).State.Terminal() })

	if got := len(h.invocations()); got != 1 {
		t.Fatalf("actor invoked %d times, want 1 (maxAttempts 1, class not retryable)", got)
	}
	status, result := attemptRecord(t, h, run.ID, "analyze")
	if engine.TechStatus(status) != engine.StatusFailed {
		t.Errorf("attempt status = %q, want failed", status)
	}
	var diag struct {
		Error struct {
			Class string `json:"class"`
		} `json:"error"`
	}
	_ = json.Unmarshal(result, &diag)
	if diag.Error.Class != string(actors.ClassCredentialSpent) {
		t.Errorf("attempt class = %q, want credential_spent (result %s)", diag.Error.Class, result)
	}

	row, ok, err := h.store.ActorLiveness(h.ctx, h.ns.ID, "company/analyzer")
	if err != nil || !ok {
		t.Fatalf("ActorLiveness = (%v, %v), want the locked row", ok, err)
	}
	if !row.Locked || row.SessionOK == nil || *row.SessionOK || row.Reason != worker.LivenessLockReason || row.Source != storepg.LivenessSourceWorker {
		t.Fatalf("row = %+v, want locked session_ok=false reason=refresh_token_spent source=worker", row)
	}
	if row.LockedByRunID != run.ID {
		t.Errorf("locked_by_run_id = %q, want %s", row.LockedByRunID, run.ID)
	}
	if row.Live(time.Now().UTC()) {
		t.Error("a freshly locked row reads live")
	}
	if !hasEvent(runEventTypes(t, h, run.ID), worker.TypeLaneLocked) {
		t.Errorf("run events carry no %s", worker.TypeLaneLocked)
	}
}

// A worker started after the row was written knows nothing that the row
// does not say: the decision is read from PostgreSQL on every dispatch, so
// a restart between the probe and the lease changes nothing.
func TestAWorkerStartedAfterTheRowWasWrittenStillRefusesFromIt(t *testing.T) {
	fb := newFallbackActor(t)
	var lanes livenessLane
	h := newHarness(t, completesSynchronously, withLivenessLanes(t, withFallbackMetadata, fb.server.URL, &lanes))
	observeNotLive(t, h, "company/analyzer", time.Now().UTC())

	// A second, fresh worker process over the same store and namespace,
	// built after the row landed and sharing nothing with h.worker but the
	// database.
	registry, err := worker.NewDBRegistry(h.store, h.ns.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := worker.New(h.store, h.engine, worker.Options{
		WorkerID: "fresh-" + t.Name(), NamespaceID: h.ns.ID, ClaimBatch: 4,
		LeaseDuration: 30 * time.Second, HeartbeatInterval: 200 * time.Millisecond, PollInterval: 20 * time.Millisecond,
		Registry: registry, Signer: h.signer, CallbackBaseURL: h.callbackServer.URL,
		DispatchGateActorID: lanes.gateActorID,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}

	run := h.createRun("sync.workflow.yaml", `{"subject":"widget"}`)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !h.run(run.ID).State.Terminal() {
		if _, err := fresh.Tick(h.ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !h.run(run.ID).State.Terminal() {
		t.Fatal("run did not finish")
	}
	if got := len(h.invocations()); got != 0 {
		t.Fatalf("primary invoked %d times by a fresh worker, want 0", got)
	}
	if got := fb.hits.Load(); got != 1 {
		t.Fatalf("fallback invoked %d times by a fresh worker, want 1", got)
	}
	if routings := livenessRoutings(t, h, run.ID); len(routings) != 1 || routings[0]["selected"] != "fallback" {
		t.Fatalf("routing records = %v, want one fallback routing", routings)
	}
}
