package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
)

// TestListNodeRunsAcrossRuns is task t11's centerpiece for the new GET
// /v1alpha1/node-runs cross-run "jobs timeline" listing: it proves the
// listing actually crosses runs, that updated_since/updated_until filter
// server-side on updated_at (honesty condition h5), that actor_id reflects
// the most recent attempt's actor (empty for a node run never dispatched),
// and that keyset (cursor) pagination walks the full result set newest-first
// with no gaps or duplicates.
func TestListNodeRunsAcrossRuns(t *testing.T) {
	f := newFixture(t)
	source := readFixtureWorkflow(t, "minimal.workflow.yaml")

	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)

	// 5 distinct runs (task t11: this is the CROSS-run listing), each with
	// exactly one node run at "start" (minimal.workflow.yaml's start.completed
	// edge goes straight to the end node "finish", which never becomes a
	// node run of its own -- so each run contributes exactly one row here,
	// keeping the fixture's expected set exact).
	const n = 5
	var runIDs [n]string
	var nodeRunIDs [n]string
	for i := 0; i < n; i++ {
		var run apipkg.RunOut
		resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
			createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
		requireStatus(t, resp, body, http.StatusCreated)
		runIDs[i] = run.ID

		view := getRunView(t, f, run.ID)
		if len(view.NodeRuns) != 1 {
			t.Fatalf("run %s: got %d node runs, want 1", run.ID, len(view.NodeRuns))
		}
		nodeRunIDs[i] = view.NodeRuns[0].ID
	}

	// Drive runs 0 and 2 to completion through the real claiming path, so
	// their node run carries a real attempt (and therefore an actor_id) and
	// ends up in state "completed" with outcome "completed". Runs 1, 3, 4
	// are left "ready", undispatched -- no attempt, no actor_id.
	actorID := f.insertActor("worker")
	for _, i := range []int{0, 2} {
		claimed := f.claim("worker-1", nodeRunIDs[i])
		if _, err := f.api.Engine.CompleteAttempt(context.Background(), engine.CompletionRequest{
			WorkID:       claimed.ID,
			WorkerID:     "worker-1",
			FencingToken: claimed.FencingToken,
			Attempt:      int(claimed.Attempt),
			TechStatus:   engine.StatusSucceeded,
			Outcome:      "completed",
			Output:       json.RawMessage(`{}`),
			ActorID:      actorID,
		}); err != nil {
			t.Fatalf("complete node run %s: %v", nodeRunIDs[i], err)
		}
	}

	// Overwrite every node run's updated_at (after the completions above,
	// which themselves already touched it) to a deliberately uncorrelated,
	// widely-spaced, entirely-in-the-past schedule -- far from real "now",
	// which is what keeps this listing (queried with an explicit
	// updated_since/updated_until window below) free of any other rows this
	// namespace's own workflow-publish/run-create bookkeeping might have
	// touched at real "now".
	base := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	// updated_at-DESC order (newest first): run2, run0, run4, run1, run3.
	rank := map[int]int{2: 4, 0: 3, 4: 2, 1: 1, 3: 0}
	updatedAt := func(i int) time.Time { return base.Add(time.Duration(10+rank[i]) * time.Hour) }
	for i := 0; i < n; i++ {
		if _, err := f.store.Pool().Exec(context.Background(),
			`UPDATE node_runs SET updated_at = $2 WHERE id = $1`, nodeRunIDs[i], updatedAt(i)); err != nil {
			t.Fatalf("set node run %s updated_at: %v", nodeRunIDs[i], err)
		}
	}

	descAll := []int{2, 0, 4, 1, 3} // full expected newest-first order

	// --- full window, single page (limit >= n): exact cross-run order,
	// actor_id present only for the two completed node runs. ---
	// .UTC() before formatting: a "+HH:MM" offset RFC3339 timestamp
	// contains a literal '+', which a raw (non-escaped) query string turns
	// into a space -- UTC's "Z" suffix sidesteps that entirely.
	wideSince := base.UTC().Format(time.RFC3339)
	wideUntil := base.Add(24 * time.Hour).UTC().Format(time.RFC3339)
	var full apipkg.NodeRunListOut
	resp, body = doJSON(t, f.client, http.MethodGet,
		f.url("/v1alpha1/node-runs?updated_since="+wideSince+"&updated_until="+wideUntil+"&limit=50"), nil, &full)
	requireStatus(t, resp, body, http.StatusOK)
	assertNodeRunOrder(t, full.Items, indexIDs(nodeRunIDs[:], descAll)...)
	if full.NextCursor != "" {
		t.Fatalf("next_cursor = %q, want empty: the full set fit in one page", full.NextCursor)
	}
	byID := make(map[string]apipkg.NodeRunListItemOut)
	for _, item := range full.Items {
		byID[item.ID] = item
		if item.RunID == "" {
			t.Fatalf("item %s has empty run_id", item.ID)
		}
		if item.NodeID != "start" {
			t.Fatalf("item %s node_id = %q, want start", item.ID, item.NodeID)
		}
	}
	for _, i := range []int{0, 2} {
		item := byID[nodeRunIDs[i]]
		if item.ActorID != actorID {
			t.Fatalf("run %d node run actor_id = %q, want %q (the completing worker's actor)", i, item.ActorID, actorID)
		}
		if item.State != "completed" || item.Outcome != "completed" {
			t.Fatalf("run %d node run state/outcome = %q/%q, want completed/completed", i, item.State, item.Outcome)
		}
		if item.CompletedAt == nil {
			t.Fatalf("run %d node run completed_at is unset", i)
		}
	}
	for _, i := range []int{1, 3, 4} {
		item := byID[nodeRunIDs[i]]
		if item.ActorID != "" {
			t.Fatalf("run %d node run (never dispatched) actor_id = %q, want empty", i, item.ActorID)
		}
		if item.State != "ready" {
			t.Fatalf("run %d node run state = %q, want ready", i, item.State)
		}
	}

	// --- narrower time window: only rank 1..3 (runs 4, 0... precisely
	// runs whose updated_at falls in [base+11h, base+13h]) -- run1 (11h),
	// run4 (12h), run0 (13h) -- excluding run3 (10h) and run2 (14h). This
	// is the server-side filter honesty check (h5): the excluded rows are
	// never sent to the client to filter out themselves. ---
	narrowSince := base.Add(11 * time.Hour).UTC().Format(time.RFC3339)
	narrowUntil := base.Add(13 * time.Hour).UTC().Format(time.RFC3339)
	var windowed apipkg.NodeRunListOut
	resp, body = doJSON(t, f.client, http.MethodGet,
		f.url("/v1alpha1/node-runs?updated_since="+narrowSince+"&updated_until="+narrowUntil+"&limit=50"), nil, &windowed)
	requireStatus(t, resp, body, http.StatusOK)
	assertNodeRunOrder(t, windowed.Items, indexIDs(nodeRunIDs[:], []int{0, 4, 1})...)

	// --- pagination: same wide window, limit=2, walk every page by cursor
	// and confirm the concatenation is exactly descAll with no gaps or
	// duplicates. ---
	var paged []apipkg.NodeRunListItemOut
	cursor := ""
	for page := 0; ; page++ {
		if page > n {
			t.Fatalf("pagination did not terminate after %d pages; collected so far: %+v", page, paged)
		}
		u := f.url("/v1alpha1/node-runs?updated_since=" + wideSince + "&updated_until=" + wideUntil + "&limit=2")
		if cursor != "" {
			u += "&cursor=" + cursor
		}
		var pageOut apipkg.NodeRunListOut
		resp, body := doJSON(t, f.client, http.MethodGet, u, nil, &pageOut)
		requireStatus(t, resp, body, http.StatusOK)
		paged = append(paged, pageOut.Items...)
		if pageOut.NextCursor == "" {
			break
		}
		cursor = pageOut.NextCursor
	}
	assertNodeRunOrder(t, paged, indexIDs(nodeRunIDs[:], descAll)...)

	// --- malformed params are refused with 400, not silently ignored. ---
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/node-runs?updated_since=not-a-timestamp"), nil, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/node-runs?updated_until=not-a-timestamp"), nil, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/node-runs?cursor=not-a-real-cursor"), nil, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)
}

func indexIDs(ids []string, order []int) []string {
	out := make([]string, len(order))
	for i, idx := range order {
		out[i] = ids[idx]
	}
	return out
}

// assertNodeRunOrder fails the test unless got is exactly wantIDs, in order.
func assertNodeRunOrder(t *testing.T, got []apipkg.NodeRunListItemOut, wantIDs ...string) {
	t.Helper()
	gotIDs := make([]string, len(got))
	for i, item := range got {
		gotIDs[i] = item.ID
	}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("got %d node runs %v, want %d %v", len(gotIDs), gotIDs, len(wantIDs), wantIDs)
	}
	for i, want := range wantIDs {
		if gotIDs[i] != want {
			t.Fatalf("node run order mismatch at index %d: got %v, want %v", i, gotIDs, wantIDs)
		}
	}
}
