package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
)

// The schedule surface (issue #107, task t33). The operator-facing half of
// acceptance criterion 1: an operator declares a cadence, and can turn it off
// without republishing the workflow it starts.

func createSchedule(t *testing.T, f *fixture, body map[string]any) (*http.Response, apipkg.ScheduleOut) {
	t.Helper()
	var out apipkg.ScheduleOut
	resp, _ := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/schedules"), body, &out)
	return resp, out
}

func TestScheduleIsDeclaredReadBackAndDisabledWithoutTouchingTheWorkflow(t *testing.T) {
	f := newFixture(t)
	first := time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)

	resp, created := createSchedule(t, f, map[string]any{
		"name":             "nightly-upkeep",
		"event_name":       "pr-upkeep.pr",
		"emitter":          "schedule:nightly-upkeep",
		"payload":          json.RawMessage(`{"source":"github_pr"}`),
		"interval_seconds": 86400,
		"first_fire_at":    first,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /schedules = %d, want 201", resp.StatusCode)
	}
	if !created.Enabled {
		t.Fatal("a schedule declared without `enabled` was created disabled; an operator who declared a cadence meant it to run")
	}
	if !created.NextFireAt.Equal(first) {
		t.Fatalf("next_fire_at = %s, want the declared first_fire_at %s", created.NextFireAt, first)
	}
	if created.CatchUp != "fire-once" {
		t.Fatalf("catch_up = %q, want the fire-once default", created.CatchUp)
	}
	if created.FireCount != 0 || created.LastEventID != "" {
		t.Fatalf("a fresh schedule reports fire_count=%d last_event_id=%q", created.FireCount, created.LastEventID)
	}

	var list apipkg.ScheduleListOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/schedules"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	if len(list.Items) != 1 || list.Items[0].ID != created.ID {
		t.Fatalf("GET /schedules returned %+v", list.Items)
	}

	// Disable it. Criterion 1's second clause is an API call, not a redeploy.
	var patched apipkg.ScheduleOut
	resp, body = doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/schedules/"+created.ID),
		map[string]any{"enabled": false}, &patched)
	requireStatus(t, resp, body, http.StatusOK)
	if patched.Enabled {
		t.Fatal("PATCH enabled=false left the schedule enabled")
	}
	// Disabling must not move the cursor: a pause is not a reset.
	if !patched.NextFireAt.Equal(created.NextFireAt) {
		t.Fatalf("disabling moved next_fire_at from %s to %s", created.NextFireAt, patched.NextFireAt)
	}

	// A disabled schedule stays LISTED. It is precisely what an operator
	// asking "why has nothing run" needs to see.
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/schedules"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	if len(list.Items) != 1 || list.Items[0].Enabled {
		t.Fatalf("a disabled schedule is not visible in the list: %+v", list.Items)
	}

	resp, body = doJSON(t, f.client, http.MethodDelete, f.url("/v1alpha1/schedules/"+created.ID), nil, nil)
	requireStatus(t, resp, body, http.StatusNoContent)
	resp, _ = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/schedules/"+created.ID), nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET a deleted schedule = %d, want 404", resp.StatusCode)
	}
}

func TestScheduleDeclarationRefusalsAreCallerErrors(t *testing.T) {
	f := newFixture(t)
	base := map[string]any{
		"name": "a", "event_name": "x", "interval_seconds": 60,
	}
	with := func(k string, v any) map[string]any {
		out := map[string]any{}
		for kk, vv := range base {
			out[kk] = vv
		}
		out[k] = v
		return out
	}

	for _, tc := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"zero interval", with("interval_seconds", 0), http.StatusBadRequest},
		{"negative interval", with("interval_seconds", -60), http.StatusBadRequest},
		{"no event name", with("event_name", ""), http.StatusBadRequest},
		{"no name", with("name", ""), http.StatusBadRequest},
		{"unknown catch-up policy", with("catch_up", "whenever"), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := createSchedule(t, f, tc.body)
			if resp.StatusCode != tc.want {
				t.Fatalf("POST /schedules with %s = %d, want %d", tc.name, resp.StatusCode, tc.want)
			}
		})
	}

	// A name already in use is a 409, not a 500: the caller can fix it.
	if resp, _ := createSchedule(t, f, with("name", "taken")); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create = %d", resp.StatusCode)
	}
	resp, _ := createSchedule(t, f, with("name", "taken"))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate schedule name = %d, want 409", resp.StatusCode)
	}
}

func TestPatchingAScheduleRefusesEverythingButEnabled(t *testing.T) {
	f := newFixture(t)
	_, created := createSchedule(t, f, map[string]any{
		"name": "cadence", "event_name": "x", "interval_seconds": 3600})

	// Changing the cadence of a live schedule would leave already-fired runs
	// pointing at a declaration that no longer exists. It is a delete and a
	// create, and the API says so rather than silently ignoring the field.
	resp, _ := doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/schedules/"+created.ID),
		map[string]any{"interval_seconds": 60}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PATCH with only interval_seconds = %d, want 400", resp.StatusCode)
	}
}

// TestScheduleReadSurfaceCarriesSuppressionState is task t9's fourth clause
// (spec c40/c4/c44, issue #253). The read surface is where the whole
// suppression design either works or does not: a schedule that quietly stops
// minting is worse than one that floods, and the only thing standing between
// those two is that "held back, N times, for THIS reason" is answerable from
// the API alone.
//
// The raw-JSON assertions are deliberate. `suppressed_count` is REQUIRED in
// api/openapi/openapi.yaml, so it must appear even at zero — a client reading
// its absence as "this build has no suppression" would be reading a fact that
// is not there. `last_failure_detail` is the opposite: omitempty, so its
// presence means the schedule is currently carrying a reason.
func TestScheduleReadSurfaceCarriesSuppressionState(t *testing.T) {
	f := newFixture(t)

	resp, created := createSchedule(t, f, map[string]any{
		"name":             "pr-upkeep-sweep-5m",
		"event_name":       "pr-upkeep.pr",
		"interval_seconds": 300,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /schedules = %d, want 201", resp.StatusCode)
	}
	if created.SuppressedCount != 0 || created.LastFailureDetail != "" {
		t.Fatalf("a fresh schedule reports suppressed_count=%d last_failure_detail=%q",
			created.SuppressedCount, created.LastFailureDetail)
	}

	const detail = `node "work" reported outcome "changes_required", which the pinned definition does not declare`
	if _, err := f.store.Pool().Exec(context.Background(),
		`UPDATE schedules SET suppressed_count = 29, consecutive_failures = 5, last_failure_detail = $2
		 WHERE id = $1`, created.ID, detail); err != nil {
		t.Fatalf("record suppression state: %v", err)
	}

	var raw map[string]any
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/schedules/"+created.ID), nil, &raw)
	requireStatus(t, resp, body, http.StatusOK)
	if got, ok := raw["suppressed_count"]; !ok || got != float64(29) {
		t.Errorf("GET /schedules/{id} suppressed_count = %v (present=%t), want 29", got, ok)
	}
	if got := raw["last_failure_detail"]; got != detail {
		t.Errorf("GET /schedules/{id} last_failure_detail = %v, want the repeated reason", got)
	}

	var list apipkg.ScheduleListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/schedules"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	if len(list.Items) != 1 {
		t.Fatalf("GET /schedules returned %d items, want 1", len(list.Items))
	}
	if list.Items[0].SuppressedCount != 29 || list.Items[0].LastFailureDetail != detail {
		t.Errorf("GET /schedules item = suppressed_count %d / last_failure_detail %q, want 29 / the repeated reason",
			list.Items[0].SuppressedCount, list.Items[0].LastFailureDetail)
	}
}
