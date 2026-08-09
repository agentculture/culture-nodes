package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
)

// TestListRunsUpdatedAtWindowAndSort is task t11's centerpiece for GET
// /v1alpha1/runs: it proves updated_since/updated_until actually filter on
// updated_at (not created_at), that sort=updated_at actually orders by
// updated_at, and that omitting sort while a time window is set defaults to
// the same updated_at order -- all server-side (honesty condition h5: never
// a client-side filter). created_at and updated_at are set to deliberately
// UNCORRELATED orderings by hand so a test that accidentally sorted by the
// wrong column would fail rather than pass by coincidence.
func TestListRunsUpdatedAtWindowAndSort(t *testing.T) {
	f := newFixture(t)
	source := readFixtureWorkflow(t, "edge-order-ordered.workflow.yaml")

	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	// Create 5 runs (r0..r4). Each gets a distinct, hand-assigned
	// created_at and updated_at so the two orders disagree: created_at
	// ascends r0->r4, updated_at ascends in the reshuffled order
	// r2,r4,r1,r3,r0 (i.e. updated_at-DESC order is r0,r3,r1,r4,r2).
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	var runIDs [5]string
	for i := 0; i < 5; i++ {
		var run apipkg.RunOut
		resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
			createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
		requireStatus(t, resp, body, http.StatusCreated)
		runIDs[i] = run.ID
	}

	createdAt := func(i int) time.Time { return base.Add(time.Duration(i) * time.Hour) }
	// updated_at-DESC order must be r0,r3,r1,r4,r2 -- assign updated_at
	// accordingly (largest first).
	updatedRank := map[string]int{runIDs[0]: 4, runIDs[3]: 3, runIDs[1]: 2, runIDs[4]: 1, runIDs[2]: 0}
	updatedAt := func(id string) time.Time { return base.Add(time.Duration(10+updatedRank[id]) * time.Hour) }

	for i, id := range runIDs {
		if _, err := f.store.Pool().Exec(context.Background(),
			`UPDATE runs SET created_at = $2, updated_at = $3 WHERE id = $1`,
			id, createdAt(i), updatedAt(id)); err != nil {
			t.Fatalf("set run %s timestamps: %v", id, err)
		}
	}

	// Default (no params): unchanged pre-t11 behavior, newest-first by
	// created_at -- r4,r3,r2,r1,r0.
	var byCreated apipkg.RunListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs?limit=10"), nil, &byCreated)
	requireStatus(t, resp, body, http.StatusOK)
	assertRunOrder(t, byCreated.Items, runIDs[4], runIDs[3], runIDs[2], runIDs[1], runIDs[0])

	// Explicit sort=updated_at, no time window: r0,r3,r1,r4,r2.
	var byUpdated apipkg.RunListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs?sort=updated_at&limit=10"), nil, &byUpdated)
	requireStatus(t, resp, body, http.StatusOK)
	assertRunOrder(t, byUpdated.Items, runIDs[0], runIDs[3], runIDs[1], runIDs[4], runIDs[2])

	// updated_since/updated_until window to [rank 1, rank 3] inclusive
	// (r4, r1, r3), sort omitted: the time window alone must default sort
	// to updated_at, newest first -- r3,r1,r4.
	// .UTC() before formatting: a "+HH:MM" offset RFC3339 timestamp
	// contains a literal '+', which a raw (non-escaped) query string turns
	// into a space -- UTC's "Z" suffix sidesteps that entirely.
	since := base.Add(11 * time.Hour).UTC().Format(time.RFC3339)
	until := base.Add(13 * time.Hour).UTC().Format(time.RFC3339)
	var windowed apipkg.RunListOut
	resp, body = doJSON(t, f.client, http.MethodGet,
		f.url("/v1alpha1/runs?updated_since="+since+"&updated_until="+until+"&limit=10"), nil, &windowed)
	requireStatus(t, resp, body, http.StatusOK)
	assertRunOrder(t, windowed.Items, runIDs[3], runIDs[1], runIDs[4])

	// Malformed updated_since is refused with 400 in the documented shape,
	// not silently ignored (a silently-ignored typo would return an
	// unfiltered page while the caller believes it filtered).
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs?updated_since=not-a-timestamp"), nil, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	// Malformed updated_until: same.
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs?updated_until=not-a-timestamp"), nil, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	// Unrecognized sort value is refused, not silently defaulted.
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs?sort=not_a_column"), nil, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)
}

// assertRunOrder fails the test unless got is exactly wantIDs, in order.
func assertRunOrder(t *testing.T, got []apipkg.RunOut, wantIDs ...string) {
	t.Helper()
	gotIDs := make([]string, len(got))
	for i, r := range got {
		gotIDs[i] = r.ID
	}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("got %d runs %v, want %d %v", len(gotIDs), gotIDs, len(wantIDs), wantIDs)
	}
	for i, want := range wantIDs {
		if gotIDs[i] != want {
			t.Fatalf("run order mismatch at index %d: got %v, want %v", i, gotIDs, wantIDs)
		}
	}
}
