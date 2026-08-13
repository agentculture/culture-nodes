package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// POST /v1alpha1/events tests (task t10, issue #39): the authenticated
// inbound signal delivery route. The park here is performed exactly as the
// worker's signal wait dispatch performs it (Store.StartDurableSignalWait
// under the real claim's fencing tuple) — the same hand-operated-worker
// discipline cancelwait_test.go uses for timer waits; the full real-worker
// walk (dispatch parks, POST resumes, run completes through its edges)
// lives in internal/worker/wait_test.go.

// eventTokenSecret is a fixed, sufficiently long test secret — length is
// all api.WithEventTokenSecret cares about; not a production value.
const eventTokenSecret = "test-only-event-secret-not-for-production"

// newFixtureWithEventAuth mirrors newFixtureWithDecisionAuth: the standard
// fixture plus a configured event token secret, so one server can exercise
// both "wrong/missing credentials refused" and "correct credentials
// accepted". newFixture's own default (no secret at all) is what
// TestDeliverEventRefusedWhenNoSecretConfigured exercises.
func newFixtureWithEventAuth(t *testing.T, secret string) *fixture {
	t.Helper()
	s := requireStore(t)

	nsID := pgtest.MustNamespace(t, s, "api").ID
	srv, err := apipkg.NewServer(s, nsID,
		apipkg.WithPollInterval(30*time.Millisecond),
		apipkg.WithEventTokenSecret(secret),
	)
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &fixture{t: t, server: ts, api: srv, store: s, nsID: nsID, client: ts.Client()}
}

// postEvent sends POST /v1alpha1/events with the given bearer token ("" for
// no Authorization header at all) and decodes the response into out when
// out is non-nil.
func postEvent(t *testing.T, f *fixture, token string, reqBody any, out any) (*http.Response, []byte) {
	t.Helper()
	payload, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, f.url("/v1alpha1/events"), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1alpha1/events: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(buf.Bytes(), out); err != nil {
			t.Fatalf("decode response %s: %v", buf.Bytes(), err)
		}
	}
	return resp, buf.Bytes()
}

type deliverEventReq struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload,omitempty"`
	RunID   string          `json:"run_id,omitempty"`
	Emitter string          `json:"emitter,omitempty"`
}

// parkRunOnSignal publishes the wait workflow, creates a run, and parks its
// wait node on a signal subscription under the real claim's fencing tuple —
// returning the run id, node run id, work item id, and subscription id.
func parkRunOnSignal(t *testing.T, f *fixture, eventName string) (runID, nodeRunID, workID, subID string) {
	t.Helper()
	source := readFixtureWorkflow(t, "wait.workflow.yaml")

	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	view := getRunView(t, f, run.ID)
	if len(view.NodeRuns) != 1 {
		t.Fatalf("got %d node runs, want 1", len(view.NodeRuns))
	}
	nodeRunID = view.NodeRuns[0].ID

	claimed := f.claim("worker-sig", nodeRunID)
	subID = "signal-" + nodeRunID
	if err := f.store.StartDurableSignalWait(context.Background(), storepg.StartDurableSignalWaitInput{
		WorkID:         claimed.ID,
		WorkerID:       "worker-sig",
		FencingToken:   claimed.FencingToken,
		Attempt:        int(claimed.Attempt),
		NamespaceID:    f.nsID,
		RunID:          run.ID,
		NodeRunID:      nodeRunID,
		NodeID:         "pause",
		AttemptID:      "att_" + store.NewULID(),
		SubscriptionID: subID,
		EventName:      eventName,
	}); err != nil {
		t.Fatalf("StartDurableSignalWait: %v", err)
	}
	return run.ID, nodeRunID, claimed.ID, subID
}

func subscriptionStatus(t *testing.T, f *fixture, subID string) string {
	t.Helper()
	var status string
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT status FROM signal_subscriptions WHERE id = $1`, subID).Scan(&status); err != nil {
		t.Fatalf("read subscription %s: %v", subID, err)
	}
	return status
}

// Acceptance criterion 2: unauthenticated delivery is refused — wrong
// token and missing header alike, 401 before anything is written.
func TestDeliverEventRefusedWithoutValidToken(t *testing.T) {
	f := newFixtureWithEventAuth(t, eventTokenSecret)

	resp, body := postEvent(t, f, "not-the-secret", deliverEventReq{Name: "green-light"}, nil)
	requireStatus(t, resp, body, http.StatusUnauthorized)

	resp, body = postEvent(t, f, "", deliverEventReq{Name: "green-light"}, nil)
	requireStatus(t, resp, body, http.StatusUnauthorized)

	// Nothing was appended: a refused delivery is not a fact.
	var n int
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM signal_events WHERE namespace_id = $1`, f.nsID).Scan(&n); err != nil {
		t.Fatalf("count signal events: %v", err)
	}
	if n != 0 {
		t.Errorf("signal events after refused deliveries = %d, want 0", n)
	}
}

// The closed-by-default posture: a server with no event secret configured
// refuses every delivery rather than serving the route authless.
func TestDeliverEventRefusedWhenNoSecretConfigured(t *testing.T) {
	f := newFixture(t)
	resp, body := postEvent(t, f, eventTokenSecret, deliverEventReq{Name: "green-light"}, nil)
	requireStatus(t, resp, body, http.StatusUnauthorized)
}

// An event with no waiting subscription is appended as a fact, not treated
// as an error — resumed is simply empty.
func TestDeliverEventWithNoSubscriberIsAppendedAsAFact(t *testing.T) {
	f := newFixtureWithEventAuth(t, eventTokenSecret)

	var out apipkg.EventDeliveryOut
	resp, body := postEvent(t, f, eventTokenSecret,
		deliverEventReq{Name: "nobody-listening", Payload: json.RawMessage(`{"n":1}`)}, &out)
	requireStatus(t, resp, body, http.StatusCreated)

	if len(out.Resumed) != 0 {
		t.Errorf("resumed = %+v, want empty", out.Resumed)
	}
	if out.Event.ID == "" || out.Event.Name != "nobody-listening" {
		t.Errorf("event = %+v, want an appended fact named nobody-listening", out.Event)
	}
	if out.Event.Emitter != "external" {
		t.Errorf("emitter = %q, want the documented default %q", out.Event.Emitter, "external")
	}
	ev, found, err := f.store.SignalEventByID(context.Background(), out.Event.ID)
	if err != nil || !found {
		t.Fatalf("SignalEventByID(%s) = (found=%v, err=%v), want the appended row", out.Event.ID, found, err)
	}
	if !bytes.Contains(ev.Payload, []byte(`"n"`)) {
		t.Errorf("stored payload = %s, want the delivered body", ev.Payload)
	}
}

// An authenticated delivery resumes a parked signal wait: subscription
// fired, work item claimable again — the store-transaction half of
// acceptance criterion 1 (the full worker-driven completion is
// internal/worker/wait_test.go's TestWaitSignalParksDurablyAndEventDeliveryResumesIt).
func TestDeliverEventResumesParkedSignalWait(t *testing.T) {
	f := newFixtureWithEventAuth(t, eventTokenSecret)
	runID, nodeRunID, workID, subID := parkRunOnSignal(t, f, "green-light")

	var out apipkg.EventDeliveryOut
	resp, body := postEvent(t, f, eventTokenSecret,
		deliverEventReq{Name: "green-light", Payload: json.RawMessage(`{"go":true}`), Emitter: "ops"}, &out)
	requireStatus(t, resp, body, http.StatusCreated)

	if len(out.Resumed) != 1 || out.Resumed[0].SubscriptionID != subID ||
		out.Resumed[0].RunID != runID || out.Resumed[0].NodeRunID != nodeRunID {
		t.Fatalf("resumed = %+v, want exactly (%s, %s, %s)", out.Resumed, subID, runID, nodeRunID)
	}
	if got := subscriptionStatus(t, f, subID); got != "fired" {
		t.Errorf("subscription status = %q, want fired", got)
	}
	if got := workItemState(t, f, workID); got != "ready" {
		t.Errorf("work item state = %q, want ready (the delivery's resume effect)", got)
	}
}

// A subscription created after the event does NOT retroactively fire — the
// documented limitation this pass carries (issue #43).
func TestDeliverEventDoesNotRetroactivelyFireLaterSubscriber(t *testing.T) {
	f := newFixtureWithEventAuth(t, eventTokenSecret)

	resp, body := postEvent(t, f, eventTokenSecret, deliverEventReq{Name: "early-bird"}, nil)
	requireStatus(t, resp, body, http.StatusCreated)

	_, _, workID, subID := parkRunOnSignal(t, f, "early-bird")
	if got := subscriptionStatus(t, f, subID); got != "pending" {
		t.Errorf("late subscription = %q, want pending (no retroactive fire)", got)
	}
	if got := workItemState(t, f, workID); got != "waiting" {
		t.Errorf("work item = %q, want waiting", got)
	}
}

// The signal sibling of TestCancelRunReapsPendingWaitTimer: cancelling a
// run parked on until.signal retires the pending subscription along with
// the work item, so a later delivery finds nothing to fire for the dead run.
func TestCancelRunReapsPendingSignalSubscription(t *testing.T) {
	f := newFixtureWithEventAuth(t, eventTokenSecret)
	runID, _, workID, subID := parkRunOnSignal(t, f, "green-light")

	var cancelled apipkg.RunOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+runID+"/cancel"), nil, &cancelled)
	requireStatus(t, resp, body, http.StatusOK)

	if got := subscriptionStatus(t, f, subID); got != "canceled" {
		t.Errorf("subscription after cancel = %q, want canceled (nothing may ever fire for a dead run)", got)
	}
	if got := workItemState(t, f, workID); got != "cancelled" {
		t.Errorf("work item after cancel = %q, want cancelled", got)
	}

	// A delivery after the cancel appends the fact but resumes nothing.
	var out apipkg.EventDeliveryOut
	resp, body = postEvent(t, f, eventTokenSecret, deliverEventReq{Name: "green-light"}, &out)
	requireStatus(t, resp, body, http.StatusCreated)
	if len(out.Resumed) != 0 {
		t.Errorf("delivery after cancel resumed %+v, want nothing", out.Resumed)
	}
	if got := workItemState(t, f, workID); got != "cancelled" {
		t.Errorf("work item after post-cancel delivery = %q, want cancelled", got)
	}
}

func TestDeliverEventValidatesBody(t *testing.T) {
	f := newFixtureWithEventAuth(t, eventTokenSecret)

	resp, body := postEvent(t, f, eventTokenSecret, deliverEventReq{Name: ""}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)

	resp, body = postEvent(t, f, eventTokenSecret,
		deliverEventReq{Name: "green-light", RunID: "does-not-exist"}, nil)
	requireStatus(t, resp, body, http.StatusNotFound)
}
