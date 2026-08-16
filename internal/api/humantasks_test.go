package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// decisionAuthSecret is a fixed, sufficiently long test secret — length is
// all api.WithDecisionAuthSecret cares about; it is not a production value.
const decisionAuthSecret = "test-only-decision-secret-not-for-production"

// newFixtureWithDecisionAuth mirrors fixture_test.go's newFixture, but
// configures the server with a decision auth secret so the tests in this
// file can exercise both "wrong/missing credentials refused" and "correct
// credentials accepted" against one server. newFixture's own default
// (no secret configured at all) is what TestHumanTaskDecisionRefusedWhenNoSecretConfigured
// below exercises — the two together cover PRD spec decision c45's carve-out
// for this one endpoint.
// extra options let a test tighten one more knob (task t32's repair-router
// identity) without a second near-identical constructor.
func newFixtureWithDecisionAuth(t *testing.T, secret string, extra ...apipkg.Option) *fixture {
	t.Helper()
	s := requireStore(t)

	nsID := pgtest.MustNamespace(t, s, "api").ID
	srv, err := apipkg.NewServer(s, nsID, append([]apipkg.Option{
		apipkg.WithPollInterval(30 * time.Millisecond),
		apipkg.WithDecisionAuthSecret(secret),
	}, extra...)...)
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &fixture{t: t, server: ts, api: srv, store: s, nsID: nsID, client: ts.Client()}
}

// readApprovalWorkflow reads internal/engine/testdata/approval.workflow.yaml
// — t6's three-node approval slice (intake -> review -> finish) — rather
// than forking a copy into internal/compiler/testdata: it already exists
// exactly for exercising an approval node, and this package's tests read it
// straight off disk the same way readFixtureWorkflow reads its own
// directory.
func readApprovalWorkflow(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("..", "engine", "testdata", "approval.workflow.yaml")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return source
}

// advanceToReview publishes the approval workflow, creates a run, and drives
// intake to completion (through the real claiming path), landing the run's
// token on `review` — the approval node every test below is about. It
// returns the run and the human task GET response for it.
func advanceToReview(t *testing.T, f *fixture) (apipkg.RunOut, apipkg.HumanTaskOut) {
	t.Helper()

	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(readApprovalWorkflow(t))}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{"subject":"ship it"}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	view := getRunView(t, f, run.ID)
	intakeNodeRun := onlyReadyNodeRun(t, view)
	claimed := f.claim("worker-a", intakeNodeRun.ID)
	if _, err := f.api.Engine.CompleteAttempt(context.Background(), engine.CompletionRequest{
		WorkID:       claimed.ID,
		WorkerID:     "worker-a",
		FencingToken: claimed.FencingToken,
		Attempt:      int(claimed.Attempt),
		TechStatus:   engine.StatusSucceeded,
		Outcome:      "completed",
		Output:       json.RawMessage(`{"scope":"s"}`),
	}); err != nil {
		t.Fatalf("complete intake: %v", err)
	}

	var tasks apipkg.HumanTaskListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks?status=pending"), nil, &tasks)
	requireStatus(t, resp, body, http.StatusOK)

	var task *apipkg.HumanTaskOut
	for i := range tasks.Items {
		if tasks.Items[i].RunID == run.ID {
			task = &tasks.Items[i]
		}
	}
	if task == nil {
		t.Fatalf("no pending human task for run %s among %+v", run.ID, tasks.Items)
	}
	return run, *task
}

type decideHumanTaskReq struct {
	Outcome               string          `json:"outcome"`
	DeciderActorID        string          `json:"decider_actor_id"`
	Response              json.RawMessage `json:"response,omitempty"`
	ExpectedLedgerVersion int64           `json:"expected_ledger_version"`
	RecordIDs             []string        `json:"record_ids,omitempty"`
}

// authedDecide sends the decision request with the given bearer token
// (empty means no Authorization header at all).
func authedDecide(t *testing.T, f *fixture, taskID, bearer string, req decideHumanTaskReq) (*http.Response, []byte) {
	t.Helper()

	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal decision request: %v", err)
	}
	httpReq, err := http.NewRequest(http.MethodPost, f.url("/v1alpha1/human-tasks/"+taskID+"/decision"), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := f.client.Do(httpReq)
	if err != nil {
		t.Fatalf("POST decision: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, data
}

// TestHumanTasksListGetDecisionLifecycle is t7's centerpiece: publish an
// approval workflow, drive it to the paused human task over HTTP, list and
// get it, refuse an unauthenticated and a wrongly-authenticated decision,
// commit the real decision, and prove the task cannot be decided again.
func TestHumanTasksListGetDecisionLifecycle(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, task := advanceToReview(t, f)

	// GET .../{id} carries the PRD §9.9 request payload t6 stored, verbatim.
	var fetched apipkg.HumanTaskOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks/"+task.ID), nil, &fetched)
	requireStatus(t, resp, body, http.StatusOK)
	if fetched.ID != task.ID || fetched.RunID != run.ID {
		t.Fatalf("fetched task = %+v, want id=%s run_id=%s", fetched, task.ID, run.ID)
	}
	if fetched.Status != "pending" {
		t.Fatalf("status = %q, want pending", fetched.Status)
	}
	var request struct {
		DecisionSchemaRef string   `json:"decision_schema_ref"`
		ApproverRef       string   `json:"approver_ref"`
		AllowedOutcomes   []string `json:"allowed_outcomes"`
	}
	if err := json.Unmarshal(fetched.Request, &request); err != nil {
		t.Fatalf("decode request payload: %v (%s)", err, fetched.Request)
	}
	if request.ApproverRef != "group/platform-ai-approvers" {
		t.Errorf("request.approver_ref = %q", request.ApproverRef)
	}
	equalStringSlice(t, request.AllowedOutcomes, []string{"approved", "expired", "rejected"})

	// GET (list, unfiltered) still finds it.
	var unfiltered apipkg.HumanTaskListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks"), nil, &unfiltered)
	requireStatus(t, resp, body, http.StatusOK)
	if !containsHumanTaskID(unfiltered.Items, task.ID) {
		t.Fatalf("task %s not present in unfiltered list", task.ID)
	}

	// --- auth: no Authorization header at all ---
	decideBody := decideHumanTaskReq{
		Outcome:               "approved",
		DeciderActorID:        f.insertActorKind("approver", "human"),
		ExpectedLedgerVersion: 0,
	}
	resp2, body2 := authedDecide(t, f, task.ID, "", decideBody)
	requireStatus(t, resp2, body2, http.StatusUnauthorized)
	decodeAPIError(t, body2)

	// --- auth: wrong bearer token ---
	resp2, body2 = authedDecide(t, f, task.ID, "not-the-configured-secret", decideBody)
	requireStatus(t, resp2, body2, http.StatusUnauthorized)
	decodeAPIError(t, body2)

	// The task is still pending: neither refused attempt wrote anything.
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks/"+task.ID), nil, &fetched)
	requireStatus(t, resp, body, http.StatusOK)
	if fetched.Status != "pending" {
		t.Fatalf("status = %q after refused decisions, want pending", fetched.Status)
	}

	// --- the real decision, correctly authenticated ---
	var result apipkg.HumanTaskDecisionResultOut
	resp2, body2 = authedDecide(t, f, task.ID, decisionAuthSecret, decideBody)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("decide: status = %d, body = %s", resp2.StatusCode, body2)
	}
	if err := json.Unmarshal(body2, &result); err != nil {
		t.Fatalf("decode decision result: %v (%s)", err, body2)
	}
	if result.RunState != string(engine.RunCompleted) {
		t.Fatalf("run_state = %q, want %q — review.approved routes straight to the end node `finish`", result.RunState, engine.RunCompleted)
	}
	if result.Outcome != "approved" {
		t.Fatalf("outcome = %q, want approved", result.Outcome)
	}
	if len(result.LedgerRecords) != 2 {
		t.Fatalf("ledger_records has %d entries, want 2 (the decision record and the confirming review)", len(result.LedgerRecords))
	}

	// GET reflects the decided status and the stored response.
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks/"+task.ID), nil, &fetched)
	requireStatus(t, resp, body, http.StatusOK)
	if fetched.Status != "decided" {
		t.Fatalf("status = %q after decision, want decided", fetched.Status)
	}
	if fetched.ResolvedAt == nil {
		t.Fatal("resolved_at is unset after decision")
	}
	var response struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(fetched.Response, &response); err != nil {
		t.Fatalf("decode response payload: %v (%s)", err, fetched.Response)
	}
	if response.Outcome != "approved" {
		t.Errorf("response.outcome = %q, want approved", response.Outcome)
	}

	// list ?status=decided finds it; ?status=pending no longer does.
	var decided apipkg.HumanTaskListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks?status=decided"), nil, &decided)
	requireStatus(t, resp, body, http.StatusOK)
	if !containsHumanTaskID(decided.Items, task.ID) {
		t.Fatalf("task %s not present in ?status=decided list", task.ID)
	}
	var pending apipkg.HumanTaskListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks?status=pending"), nil, &pending)
	requireStatus(t, resp, body, http.StatusOK)
	if containsHumanTaskID(pending.Items, task.ID) {
		t.Fatalf("task %s still present in ?status=pending list after being decided", task.ID)
	}

	// A second decision — even correctly authenticated — is refused: the
	// run must resume exactly once.
	resp2, body2 = authedDecide(t, f, task.ID, decisionAuthSecret, decideHumanTaskReq{
		Outcome:               "rejected",
		DeciderActorID:        decideBody.DeciderActorID,
		ExpectedLedgerVersion: 2,
	})
	requireStatus(t, resp2, body2, http.StatusConflict)
	decodeAPIError(t, body2)
}

// TestHumanTaskDecisionRefusedWhenNoSecretConfigured proves the default
// posture: a server built without api.WithDecisionAuthSecret (newFixture's
// own default, exactly what every other test in this package uses) refuses
// every decision, regardless of credentials presented — there is nothing to
// authenticate a decider against, so "unconfigured" is not "open".
func TestHumanTaskDecisionRefusedWhenNoSecretConfigured(t *testing.T) {
	f := newFixture(t)
	_, task := advanceToReview(t, f)

	resp, body := authedDecide(t, f, task.ID, "any-token-at-all", decideHumanTaskReq{
		Outcome:               "approved",
		DeciderActorID:        f.insertActorKind("approver", "human"),
		ExpectedLedgerVersion: 0,
	})
	requireStatus(t, resp, body, http.StatusUnauthorized)
	decodeAPIError(t, body)
}

// TestHumanTaskDecisionStaleLedgerVersion mirrors
// TestRunLifecycleEventsLedgerAndReviews's stale-review case for the
// decision endpoint: naming a ledger version the run has since moved past
// is refused with 409, nothing written.
func TestHumanTaskDecisionStaleLedgerVersion(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, task := advanceToReview(t, f)

	agentID := f.insertActor("agent")
	if _, err := f.api.Ledger.Append(context.Background(), ledger.Record{
		RecordType: ledger.RecordAnnouncement,
		RunID:      run.ID,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: agentID},
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(`{"statement":"an unrelated proposal"}`),
	}); err != nil {
		t.Fatalf("append unrelated record: %v", err)
	}

	resp, body := authedDecide(t, f, task.ID, decisionAuthSecret, decideHumanTaskReq{
		Outcome:               "approved",
		DeciderActorID:        f.insertActorKind("approver", "human"),
		ExpectedLedgerVersion: 0, // stale: the ledger is now at version 1
	})
	requireStatus(t, resp, body, http.StatusConflict)
	decodeAPIError(t, body)
}

// TestHumanTaskDecisionUnknownOutcomeIsBadRequest mirrors the engine-level
// TestDecideHumanTaskRejectsOutcomeNotAllowed test through the HTTP surface.
func TestHumanTaskDecisionUnknownOutcomeIsBadRequest(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	_, task := advanceToReview(t, f)

	resp, body := authedDecide(t, f, task.ID, decisionAuthSecret, decideHumanTaskReq{
		Outcome:               "not-a-real-outcome",
		DeciderActorID:        f.insertActorKind("approver", "human"),
		ExpectedLedgerVersion: 0,
	})
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)
}

// TestHumanTaskDecisionUnknownTaskIsNotFound.
func TestHumanTaskDecisionUnknownTaskIsNotFound(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)

	resp, body := authedDecide(t, f, "does-not-exist", decisionAuthSecret, decideHumanTaskReq{
		Outcome:               "approved",
		DeciderActorID:        f.insertActorKind("approver", "human"),
		ExpectedLedgerVersion: 0,
	})
	requireStatus(t, resp, body, http.StatusNotFound)
	decodeAPIError(t, body)
}

func containsHumanTaskID(tasks []apipkg.HumanTaskOut, id string) bool {
	for _, task := range tasks {
		if task.ID == id {
			return true
		}
	}
	return false
}

func equalStringSlice(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
