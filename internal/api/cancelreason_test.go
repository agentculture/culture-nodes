package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
)

// The cleanup node's half of POST /v1alpha1/runs/{id}/cancel (plan
// loop-closure t13, spec c6/c37): an optional JSON body {"reason": ...}
// drawn from a short allowlist rides the existing cancelRunWithReason seam so
// the run.cancelled event -- and runs.reason -- carry a machine-readable
// cause. The operator's bodiless cancel is the pre-t13 behaviour, unchanged:
// no reason recorded, and no `reason` key in the event at all.

// runCancelledEvent returns the data payload of the single run.cancelled
// event appended against runID, decoded.
func runCancelledEvent(t *testing.T, f *fixture, runID string) map[string]any {
	t.Helper()
	rows, err := f.store.Pool().Query(context.Background(), `
		SELECT data FROM events WHERE aggregate_id = $1 AND event_type = $2 ORDER BY sequence`,
		runID, engine.TypeRunCancelled)
	if err != nil {
		t.Fatalf("query run.cancelled events for run %s: %v", runID, err)
	}
	defer rows.Close()

	var payloads []map[string]any
	for rows.Next() {
		var data json.RawMessage
		if err := rows.Scan(&data); err != nil {
			t.Fatalf("scan run.cancelled event: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("decode run.cancelled event %s: %v", data, err)
		}
		payloads = append(payloads, decoded)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate run.cancelled events: %v", err)
	}
	if len(payloads) != 1 {
		t.Fatalf("run %s has %d run.cancelled events, want exactly 1", runID, len(payloads))
	}
	return payloads[0]
}

// publishAndStartRun publishes the ordered-edge fixture and starts one run
// of it, the same way TestCancelRun does.
func publishAndStartRun(t *testing.T, f *fixture) apipkg.RunOut {
	t.Helper()
	source := readFixtureWorkflow(t, "edge-order-ordered.workflow.yaml")

	// 201 on the first publish in a fixture, 200 when a sibling subtest
	// already published the identical bytes (same digest, same version).
	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("publish fixture workflow: status = %d, want 201 or 200; body = %s", resp.StatusCode, body)
	}

	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)
	return run
}

type cancelRunReq struct {
	Reason string `json:"reason"`
}

func TestCancelRunWithReasonWritesReasonIntoTheEvent(t *testing.T) {
	f := newFixture(t)
	for _, reason := range []string{"pr_merged", "pr_closed"} {
		t.Run(reason, func(t *testing.T) {
			run := publishAndStartRun(t, f)

			var cancelled apipkg.RunOut
			resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/cancel"),
				cancelRunReq{Reason: reason}, &cancelled)
			requireStatus(t, resp, body, http.StatusOK)
			if cancelled.State != string(engine.RunCancelled) {
				t.Fatalf("state = %q, want %q", cancelled.State, engine.RunCancelled)
			}
			if cancelled.Reason != reason {
				t.Fatalf("RunOut.reason = %q, want %q (migrations/0052 runs.reason)", cancelled.Reason, reason)
			}

			event := runCancelledEvent(t, f, run.ID)
			if got := event["reason"]; got != reason {
				t.Fatalf("run.cancelled event reason = %v, want %q", got, reason)
			}
			if got := event["state"]; got != string(engine.RunCancelled) {
				t.Fatalf("run.cancelled event state = %v, want %q", got, engine.RunCancelled)
			}
			detail, _ := event["detail"].(string)
			if detail == "" {
				t.Fatalf("run.cancelled event carries no detail: %v", event)
			}
		})
	}
}

func TestCancelRunRefusesAnUnknownReason(t *testing.T) {
	f := newFixture(t)
	run := publishAndStartRun(t, f)

	for _, tc := range []struct {
		name string
		body any
	}{
		{"unknown reason", cancelRunReq{Reason: "because"}},
		{"unknown field", map[string]any{"reason": "pr_merged", "detail": "smuggled"}},
		{"not an object", json.RawMessage(`"pr_merged"`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/cancel"), tc.body, nil)
			requireStatus(t, resp, body, http.StatusBadRequest)
			decodeAPIError(t, body)
		})
	}

	// A refused reason cancels nothing: the run is still live and no
	// run.cancelled event was appended.
	view := getRunView(t, f, run.ID)
	if engine.RunState(view.Run.State).Terminal() {
		t.Fatalf("run state = %q after refused cancels, want a live run", view.Run.State)
	}
	var n int
	if err := f.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE aggregate_id = $1 AND event_type = $2`, run.ID, engine.TypeRunCancelled).Scan(&n); err != nil {
		t.Fatalf("count run.cancelled events: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d run.cancelled event(s) after refused cancels, want 0", n)
	}
}

func TestOperatorBodilessCancelRecordsNoReason(t *testing.T) {
	f := newFixture(t)

	for _, tc := range []struct {
		name string
		body any
	}{
		{"no body", nil},
		{"empty object", map[string]any{}},
		{"empty reason", cancelRunReq{Reason: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := publishAndStartRun(t, f)

			var cancelled apipkg.RunOut
			resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/cancel"), tc.body, &cancelled)
			requireStatus(t, resp, body, http.StatusOK)
			if cancelled.State != string(engine.RunCancelled) {
				t.Fatalf("state = %q, want %q", cancelled.State, engine.RunCancelled)
			}
			if cancelled.Reason != "" {
				t.Fatalf("RunOut.reason = %q, want none: the operator cancel records no machine-readable reason", cancelled.Reason)
			}

			event := runCancelledEvent(t, f, run.ID)
			if _, present := event["reason"]; present {
				t.Fatalf("run.cancelled event carries a reason key (%v); the bodiless cancel must omit it", event["reason"])
			}
			if got := event["detail"]; got != "cancelled via POST /v1alpha1/runs/{id}/cancel" {
				t.Fatalf("run.cancelled event detail = %v, want the pre-t13 operator detail unchanged", got)
			}
		})
	}
}
