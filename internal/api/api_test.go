package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Local request-body shapes. internal/api's own request types
// (workflowSourceRequest, createRunRequest, ...) are unexported — this
// package encodes what api/openapi/openapi.yaml documents, the same way
// any real client would, rather than reaching for internal/api's private
// wire types.

type workflowSourceReq struct {
	Format string `json:"format"`
	Source string `json:"source"`
}

type createRunReq struct {
	WorkflowDigest string          `json:"workflow_digest"`
	Input          json.RawMessage `json:"input"`
}

type createReviewReq struct {
	RecordIDs       []string `json:"record_ids"`
	LedgerVersion   int64    `json:"ledger_version"`
	ReviewerActorID string   `json:"reviewer_actor_id"`
}

type commitReviewReq struct {
	Decisions             map[string]string `json:"decisions"`
	ExpectedLedgerVersion int64             `json:"expected_ledger_version"`
}

type validationResp struct {
	Valid       bool `json:"valid"`
	Digest      string
	Diagnostics []struct {
		Level   string `json:"level"`
		Path    string `json:"path"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Hint    string `json:"hint"`
	} `json:"diagnostics"`
}

// TestWorkflowLifecycle exercises validate and publish end to end,
// including the domain-outcome-not-HTTP-error distinction (PRD §3.4): an
// invalid document is a 200 with valid:false, never a 4xx.
func TestWorkflowLifecycle(t *testing.T) {
	f := newFixture(t)
	source := readFixtureWorkflow(t, "edge-order-ordered.workflow.yaml")

	// Validate: valid document.
	var validation validationResp
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows/validate"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &validation)
	requireStatus(t, resp, body, http.StatusOK)
	if !validation.Valid {
		t.Fatalf("valid = false for a compiling document; diagnostics = %+v", validation.Diagnostics)
	}
	if validation.Digest == "" {
		t.Fatal("digest is empty for a compiling document")
	}

	// Validate: invalid document is still a 200, domain outcome only.
	invalidSource := readFixtureWorkflow(t, "err-unknown-entry.workflow.yaml")
	var invalidValidation validationResp
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows/validate"),
		workflowSourceReq{Format: "yaml", Source: string(invalidSource)}, &invalidValidation)
	requireStatus(t, resp, body, http.StatusOK)
	if invalidValidation.Valid {
		t.Fatal("valid = true for a document with an unknown entry node")
	}
	if len(invalidValidation.Diagnostics) == 0 {
		t.Fatal("an invalid document reported no diagnostics")
	}

	// Publish: invalid document is refused with 422 in the documented
	// Error shape.
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(invalidSource)}, nil)
	requireStatus(t, resp, body, http.StatusUnprocessableEntity)
	decodeAPIError(t, body)

	// Publish: first time creates (201).
	var published apipkg.WorkflowVersionOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)
	if published.Digest != validation.Digest {
		t.Fatalf("published digest = %q, want %q", published.Digest, validation.Digest)
	}
	if published.WorkflowKey != "edge-order" {
		t.Fatalf("workflow_key = %q, want %q", published.WorkflowKey, "edge-order")
	}

	// Publish: same content again is idempotent (200, not a new version).
	var republished apipkg.WorkflowVersionOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &republished)
	requireStatus(t, resp, body, http.StatusOK)
	if republished.ID != published.ID {
		t.Fatalf("republishing the same content produced a different version: %s vs %s", republished.ID, published.ID)
	}

	// List includes it.
	var list apipkg.WorkflowVersionListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/workflows?workflow_key=edge-order"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	found := false
	for _, v := range list.Items {
		if v.Digest == published.Digest {
			found = true
		}
	}
	if !found {
		t.Fatalf("published workflow %s not present in list %+v", published.Digest, list.Items)
	}

	// Get by digest.
	var fetched apipkg.WorkflowVersionOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/workflows/"+published.Digest), nil, &fetched)
	requireStatus(t, resp, body, http.StatusOK)
	if fetched.ID != published.ID {
		t.Fatalf("fetched id = %q, want %q", fetched.ID, published.ID)
	}

	// Get an unknown digest: 404 in the documented shape.
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/workflows/sha256:does-not-exist"), nil, nil)
	requireStatus(t, resp, body, http.StatusNotFound)
	decodeAPIError(t, body)
}

// TestRunLifecycleEventsLedgerAndReviews is this task's centerpiece: publish
// a workflow, create a run, drive it forward by hand (claim+CompleteAttempt,
// the same real claiming path internal/engine's own tests use) while an SSE
// client collects committed events across a disconnect and reconnect by
// Last-Event-ID, then exercises ledger projections and a stale review
// commit.
func TestRunLifecycleEventsLedgerAndReviews(t *testing.T) {
	f := newFixture(t)
	source := readFixtureWorkflow(t, "edge-order-ordered.workflow.yaml")

	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	// --- create run ---
	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{"subject":"t19"}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)
	if run.State != string(engine.RunRunning) {
		t.Fatalf("run state = %q, want %q", run.State, engine.RunRunning)
	}
	if run.WorkflowDigest != published.Digest {
		t.Fatalf("run workflow_digest = %q, want %q", run.WorkflowDigest, published.Digest)
	}

	// list / get.
	var runList apipkg.RunListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs?state=running"), nil, &runList)
	requireStatus(t, resp, body, http.StatusOK)
	if !containsRunID(runList.Items, run.ID) {
		t.Fatalf("run %s not present in ?state=running list", run.ID)
	}

	var view apipkg.RunViewOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID), nil, &view)
	requireStatus(t, resp, body, http.StatusOK)
	if len(view.Tokens) != 1 || view.Tokens[0].State != "active" {
		t.Fatalf("expected exactly one active token right after creation, got %+v", view.Tokens)
	}
	if len(view.NodeRuns) != 1 || view.NodeRuns[0].NodeID != "start" || view.NodeRuns[0].State != "ready" {
		t.Fatalf("expected exactly one ready node run at 'start', got %+v", view.NodeRuns)
	}
	startNodeRunID := view.NodeRuns[0].ID

	// --- SSE, phase 1: connect from the beginning, read run.created + node-run.ready, then disconnect ---
	resp1 := f.openSSE(t, run.ID, "")
	ch1 := make(chan sseEvent)
	go streamSSEEvents(resp1.Body, ch1)

	var firstBatch []sseEvent
	for i := 0; i < 2; i++ {
		select {
		case ev, ok := <-ch1:
			if !ok {
				t.Fatalf("SSE stream closed after only %d events", len(firstBatch))
			}
			firstBatch = append(firstBatch, ev)
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for event %d", i+1)
		}
	}
	resp1.Body.Close() // simulate a client disconnect

	assertEventTypes(t, firstBatch, engine.TypeRunCreated, engine.TypeNodeRunReady)
	lastSeenID := firstBatch[len(firstBatch)-1].ID

	// --- SSE, phase 2: reconnect with Last-Event-ID, then drive the run to completion ---
	resp2 := f.openSSE(t, run.ID, strconv.FormatInt(lastSeenID, 10))
	ch2 := make(chan sseEvent)
	go streamSSEEvents(resp2.Body, ch2)

	// Step 1: start.completed -> middle.
	claimed := f.claim("worker-1", startNodeRunID)
	completeStep(t, f.api, claimed, "worker-1")

	// Step 2: middle.completed -> finish, which ends the run.
	view = getRunView(t, f, run.ID)
	middleNodeRunID := onlyReadyNodeRun(t, view).ID
	claimed = f.claim("worker-1", middleNodeRunID)
	completeStep(t, f.api, claimed, "worker-1")

	secondBatch := drainClosed(t, ch2, 15*time.Second)
	resp2.Body.Close()

	assertEventTypes(t, secondBatch,
		engine.TypeAttemptCompleted, engine.TypeTokenTransitioned, engine.TypeNodeRunReady,
		engine.TypeAttemptCompleted, engine.TypeTokenTransitioned, engine.TypeRunCompleted,
	)

	// No gaps, no duplicates, across the disconnect: the two batches'
	// sequence numbers concatenate into exactly 1..8.
	all := append(append([]sseEvent{}, firstBatch...), secondBatch...)
	for i, ev := range all {
		want := int64(i + 1)
		if ev.ID != want {
			t.Fatalf("event %d has sequence %d, want %d (gap or duplicate across reconnect); full sequence = %v",
				i, ev.ID, want, sequenceNumbers(all))
		}
	}

	// The run is durably completed.
	var finalRun apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID), nil, &view)
	_ = finalRun
	requireStatus(t, resp, body, http.StatusOK)
	if view.Run.State != string(engine.RunCompleted) {
		t.Fatalf("final run state = %q, want %q", view.Run.State, engine.RunCompleted)
	}

	// --- ledger: append two records directly (bypassing the node's
	// declared propose permissions on purpose — that check belongs to
	// internal/engine's CompleteAttempt path, not to ledger.Append itself)
	// and read them back through the API. ---
	actorID := f.insertActor("worker")
	firstRecord, err := f.api.Ledger.Append(t.Context(), ledger.Record{
		RecordType: ledger.RecordAnnouncement,
		RunID:      run.ID,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: actorID},
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(`{"statement":"deliver edge-order"}`),
	})
	if err != nil {
		t.Fatalf("append announcement: %v", err)
	}

	var records apipkg.LedgerRecordsOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID+"/ledger"), nil, &records)
	requireStatus(t, resp, body, http.StatusOK)
	if records.LedgerVersion != 1 {
		t.Fatalf("ledger_version = %d, want 1", records.LedgerVersion)
	}
	if len(records.Items) != 1 || records.Items[0].ID != firstRecord.ID {
		t.Fatalf("ledger records = %+v, want exactly [%s]", records.Items, firstRecord.ID)
	}

	var projection ledger.Projection
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID+"/ledger/projections/current_scope"), nil, &projection)
	requireStatus(t, resp, body, http.StatusOK)
	if projection.Digest == "" {
		t.Fatal("projection digest is empty")
	}
	if len(projection.Items) != 1 || projection.Items[0].ID != firstRecord.ID {
		t.Fatalf("current_scope projection items = %+v, want exactly [%s]", projection.Items, firstRecord.ID)
	}
	if err := projection.VerifyDigest(); err != nil {
		t.Fatalf("projection digest does not verify: %v", err)
	}

	// Unknown projection name: 400 in the documented shape.
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID+"/ledger/projections/not_a_real_projection"), nil, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	// --- reviews: create at ledger version 1, append another record
	// (moving the ledger to version 2), then commit against the now-stale
	// expected version 1 and require a 409. ---
	reviewerID := f.insertActor("reviewer")
	var review apipkg.ReviewRequestOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/reviews"),
		createReviewReq{RecordIDs: []string{firstRecord.ID}, LedgerVersion: 1, ReviewerActorID: reviewerID}, &review)
	requireStatus(t, resp, body, http.StatusCreated)
	if review.Status != "requested" {
		t.Fatalf("review status = %q, want requested", review.Status)
	}

	if _, err := f.api.Ledger.Append(t.Context(), ledger.Record{
		RecordType: ledger.RecordAnnouncement,
		RunID:      run.ID,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: actorID},
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(`{"statement":"a second, unrelated announcement"}`),
	}); err != nil {
		t.Fatalf("append second announcement: %v", err)
	}

	//
	// expected_ledger_version must match both the review's own recorded
	// ledger_version (1, permanently — CommitReview treats a caller-passed
	// value that disagrees with what the request itself recorded as a
	// separate staleness reason) and the run's *current* ledger version.
	// Once the ledger has moved past what a review was created against,
	// nothing can make that same review committable again — passing the
	// review's own recorded version still fails, because the ledger's
	// current version (2) no longer matches it. This is deliberate (PRD
	// §10.8): a review is a statement about a specific frame, not a
	// pointer a caller can keep re-aiming.
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/reviews/"+review.ID+"/commit"),
		commitReviewReq{Decisions: map[string]string{firstRecord.ID: "confirm"}, ExpectedLedgerVersion: 1}, nil)
	requireStatus(t, resp, body, http.StatusConflict)
	decodeAPIError(t, body)

	// The only way forward once stale is a fresh review request against
	// the run's current ledger version.
	var freshReview apipkg.ReviewRequestOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/reviews"),
		createReviewReq{RecordIDs: []string{firstRecord.ID}, LedgerVersion: 2, ReviewerActorID: reviewerID}, &freshReview)
	requireStatus(t, resp, body, http.StatusCreated)

	var commitResult apipkg.ReviewCommitResultOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/reviews/"+freshReview.ID+"/commit"),
		commitReviewReq{Decisions: map[string]string{firstRecord.ID: "confirm"}, ExpectedLedgerVersion: 2}, &commitResult)
	requireStatus(t, resp, body, http.StatusOK)
	if commitResult.LedgerVersion != 3 { // 2 announcements + 1 review record
		t.Fatalf("ledger_version after commit = %d, want 3", commitResult.LedgerVersion)
	}
}

// TestCancelRun exercises POST /v1alpha1/runs/{id}/cancel, including the
// "already terminal" 409.
func TestCancelRun(t *testing.T) {
	f := newFixture(t)
	source := readFixtureWorkflow(t, "edge-order-ordered.workflow.yaml")

	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	var cancelled apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/cancel"), nil, &cancelled)
	requireStatus(t, resp, body, http.StatusOK)
	if cancelled.State != string(engine.RunCancelled) {
		t.Fatalf("state = %q, want %q", cancelled.State, engine.RunCancelled)
	}

	// The node run and token are cancelled/consumed too.
	view := getRunView(t, f, run.ID)
	if view.NodeRuns[0].State != "cancelled" {
		t.Fatalf("entry node run state = %q, want cancelled", view.NodeRuns[0].State)
	}
	if view.Tokens[0].State != "consumed" {
		t.Fatalf("entry token state = %q, want consumed", view.Tokens[0].State)
	}

	// Cancelling an already-terminal run is a 409.
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/cancel"), nil, nil)
	requireStatus(t, resp, body, http.StatusConflict)
	decodeAPIError(t, body)

	// Cancelling an unknown run is a 404.
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/does-not-exist/cancel"), nil, nil)
	requireStatus(t, resp, body, http.StatusNotFound)
	decodeAPIError(t, body)
}

// TestHealthzReadyz exercises the two operations probes hit directly.
func TestHealthzReadyz(t *testing.T) {
	f := newFixture(t)

	var health apipkg.HealthOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/healthz"), nil, &health)
	requireStatus(t, resp, body, http.StatusOK)
	if health.Status != "ok" {
		t.Fatalf("healthz status = %q, want ok", health.Status)
	}

	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/readyz"), nil, &health)
	requireStatus(t, resp, body, http.StatusOK)
	if health.Status != "ok" {
		t.Fatalf("readyz status = %q, want ok", health.Status)
	}
}

// --- small assertion/lookup helpers shared by the tests above ---

func containsRunID(runs []apipkg.RunOut, id string) bool {
	for _, r := range runs {
		if r.ID == id {
			return true
		}
	}
	return false
}

func assertEventTypes(t *testing.T, got []sseEvent, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: got=%v want=%v", len(got), len(want), eventTypes(got), want)
	}
	for i, ev := range got {
		if ev.Type != want[i] {
			t.Fatalf("event %d type = %q, want %q (full: got=%v want=%v)", i, ev.Type, want[i], eventTypes(got), want)
		}
	}
}

func eventTypes(events []sseEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.Type
	}
	return out
}

func sequenceNumbers(events []sseEvent) []int64 {
	out := make([]int64, len(events))
	for i, ev := range events {
		out[i] = ev.ID
	}
	return out
}

func getRunView(t *testing.T, f *fixture, runID string) apipkg.RunViewOut {
	t.Helper()
	var view apipkg.RunViewOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+runID), nil, &view)
	requireStatus(t, resp, body, http.StatusOK)
	return view
}

// onlyReadyNodeRun returns the single node run currently in state "ready",
// failing the test if there is not exactly one.
func onlyReadyNodeRun(t *testing.T, view apipkg.RunViewOut) apipkg.NodeRunOut {
	t.Helper()
	var ready []apipkg.NodeRunOut
	for _, nr := range view.NodeRuns {
		if nr.State == "ready" {
			ready = append(ready, nr)
		}
	}
	if len(ready) != 1 {
		t.Fatalf("expected exactly one ready node run, got %+v", view.NodeRuns)
	}
	return ready[0]
}

// completeStep drives one claimed node run to a successful "completed"
// outcome through the real engine — the same "hand-operated worker" shape
// internal/engine's own tests (fixture.step in harness_test.go) use.
func completeStep(t *testing.T, srv *apipkg.Server, claimed storepg.ClaimedWork, workerID string) engine.CompletionResult {
	t.Helper()
	result, err := srv.Engine.CompleteAttempt(context.Background(), engine.CompletionRequest{
		WorkID:       claimed.ID,
		WorkerID:     workerID,
		FencingToken: claimed.FencingToken,
		Attempt:      int(claimed.Attempt),
		TechStatus:   engine.StatusSucceeded,
		Outcome:      "completed",
		Output:       json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CompleteAttempt for work %s: %v", claimed.ID, err)
	}
	return result
}
