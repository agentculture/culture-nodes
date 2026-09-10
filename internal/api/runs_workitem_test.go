package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
)

// createRunWithWorkItemReq is the documented CreateRunRequest wire shape
// with plan loop-closure-claude-codex task t1's optional `work_item`
// (migrations/0057). internal/api's createRunRequest is unexported, so this
// package encodes the shape the same way runmetadata_test.go's
// createRunWithMetadataReq does for task t3's fields.
type createRunWithWorkItemReq struct {
	WorkflowDigest string          `json:"workflow_digest"`
	Input          json.RawMessage `json:"input"`
	Category       string          `json:"category,omitempty"`
	WorkItem       string          `json:"work_item,omitempty"`
}

func createWorkItemRun(t *testing.T, f *fixture, digest, workItem string) apipkg.RunOut {
	t.Helper()
	var run apipkg.RunOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunWithWorkItemReq{WorkflowDigest: digest, Input: json.RawMessage(`{}`), WorkItem: workItem}, &run)
	requireStatus(t, resp, body, http.StatusCreated)
	return run
}

func listRunIDs(t *testing.T, f *fixture, query string) []string {
	t.Helper()
	var list apipkg.RunListOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs"+query), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	ids := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

// TestCreateRunWorkItemPersistsAndFilters is the acceptance test for t1's
// second criterion: POST /runs with work_item=SCRUM-9 stores it, GET
// /runs?work_item=SCRUM-9 returns exactly those runs, and run.category is
// untouched (decision c41: work_item is its OWN column, not a category
// overload).
func TestCreateRunWorkItemPersistsAndFilters(t *testing.T) {
	f := newFixture(t)
	digest := publishFixtureWorkflow(t, f)

	first := createWorkItemRun(t, f, digest, "SCRUM-9")
	second := createWorkItemRun(t, f, digest, "SCRUM-9")
	other := createWorkItemRun(t, f, digest, "SCRUM-10")
	bare := createWorkItemRun(t, f, digest, "")

	if first.WorkItem != "SCRUM-9" {
		t.Errorf("createRun work_item = %q, want SCRUM-9", first.WorkItem)
	}
	if first.Category != "" {
		t.Errorf("createRun category = %q, want empty: work_item must not be written into category (c41)", first.Category)
	}
	if bare.WorkItem != "" {
		t.Errorf("createRun without work_item rendered %q, want absent", bare.WorkItem)
	}

	// getRun renders it from the persisted row.
	var view apipkg.RunViewOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+first.ID), nil, &view)
	requireStatus(t, resp, body, http.StatusOK)
	if view.Run.WorkItem != "SCRUM-9" {
		t.Errorf("getRun work_item = %q, want SCRUM-9", view.Run.WorkItem)
	}

	// The filter returns exactly the two SCRUM-9 runs, newest first.
	got := listRunIDs(t, f, "?work_item=SCRUM-9")
	if len(got) != 2 || got[0] != second.ID || got[1] != first.ID {
		t.Fatalf("work_item=SCRUM-9 listed %v, want exactly [%s %s]", got, second.ID, first.ID)
	}
	if got := listRunIDs(t, f, "?work_item=SCRUM-10"); len(got) != 1 || got[0] != other.ID {
		t.Fatalf("work_item=SCRUM-10 listed %v, want exactly [%s]", got, other.ID)
	}
	if got := listRunIDs(t, f, "?work_item=SCRUM-404"); len(got) != 0 {
		t.Fatalf("work_item=SCRUM-404 listed %v, want none", got)
	}
	// No filter: every run, work_item rendered per row where set.
	var list apipkg.RunListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	if len(list.Items) != 4 {
		t.Fatalf("unfiltered listing has %d items, want 4", len(list.Items))
	}
	for _, item := range list.Items {
		switch item.ID {
		case first.ID, second.ID:
			if item.WorkItem != "SCRUM-9" {
				t.Errorf("listRuns %s work_item = %q, want SCRUM-9", item.ID, item.WorkItem)
			}
		case bare.ID:
			if item.WorkItem != "" {
				t.Errorf("listRuns %s work_item = %q, want absent", item.ID, item.WorkItem)
			}
		}
	}
}

// TestListRunsDoesNotFilterByCategory pins the deliberate NON-feature in
// t1's acceptance criteria: decision c41 gives work_item its own filter and
// leaves category exactly as it was — a `category=` query parameter is not
// a filter and must not silently narrow the listing.
func TestListRunsDoesNotFilterByCategory(t *testing.T) {
	f := newFixture(t)
	digest := publishFixtureWorkflow(t, f)

	var tagged apipkg.RunOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunWithWorkItemReq{WorkflowDigest: digest, Input: json.RawMessage(`{}`), Category: "audit"}, &tagged)
	requireStatus(t, resp, body, http.StatusCreated)
	untagged := createWorkItemRun(t, f, digest, "")

	got := listRunIDs(t, f, "?category=audit")
	if len(got) != 2 {
		t.Fatalf("category=audit listed %v, want both %s and %s (category is not a list filter)", got, tagged.ID, untagged.ID)
	}
}

// TestPatchRunRefusesWorkItem: PATCH /runs/{id} accepts category, and
// accepts work_item for exactly one transition — the orphan intake's re-key
// from the transient `gh:<owner>/<repo>#<n>` form to a Jira key (task t4,
// decision c42). Everything else about work_item stays what t1 made it: a
// run already keyed to a ticket is not retaggable, and a body naming
// work_item alongside a run that cannot take it is refused with a structured
// error rather than silently ignored, the same way name/description are.
func TestPatchRunRefusesWorkItem(t *testing.T) {
	f := newFixture(t)
	digest := publishFixtureWorkflow(t, f)
	run := createWorkItemRun(t, f, digest, "SCRUM-9")

	type patchWithWorkItem struct {
		Category *string `json:"category,omitempty"`
		WorkItem *string `json:"work_item,omitempty"`
	}
	category, moved := "audit", "SCRUM-10"

	// A run keyed to a ticket is not re-keyable: 409, the run is the conflict.
	resp, body := doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+run.ID),
		patchWithWorkItem{Category: &category, WorkItem: &moved}, nil)
	requireStatus(t, resp, body, http.StatusConflict)
	decodeAPIError(t, body)

	resp, body = doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+run.ID),
		patchWithWorkItem{WorkItem: &moved}, nil)
	requireStatus(t, resp, body, http.StatusConflict)
	decodeAPIError(t, body)

	// The refused requests changed nothing; a category-only PATCH still works
	// and leaves work_item alone.
	var patched apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+run.ID),
		patchWithWorkItem{Category: &category}, &patched)
	requireStatus(t, resp, body, http.StatusOK)
	if patched.Category != "audit" || patched.WorkItem != "SCRUM-9" {
		t.Fatalf("after PATCH: category=%q work_item=%q, want audit / SCRUM-9", patched.Category, patched.WorkItem)
	}
}

// TestPatchRunRekeysOrphanWorkItemOnce is the API half of the orphan intake
// (task t4): a run minted for a `gh:` work item takes exactly one PATCH to
// its Jira key, after which the list filter finds it under the key and a
// second re-key is refused. The target must look like a Jira key — the
// transient form may not be replaced by another transient form or by
// arbitrary text — and an untagged run (no work item at all) is not an
// orphan and is refused too.
func TestPatchRunRekeysOrphanWorkItemOnce(t *testing.T) {
	f := newFixture(t)
	digest := publishFixtureWorkflow(t, f)
	orphan := createWorkItemRun(t, f, digest, "gh:agentculture/culture-nodes#307")
	untagged := createWorkItemRun(t, f, digest, "")

	type patchWorkItem struct {
		Category *string `json:"category,omitempty"`
		WorkItem string  `json:"work_item"`
	}

	// Not a Jira key: 400 (a request problem, not a state problem).
	for _, bad := range []string{"gh:agentculture/culture-nodes#308", "scrum-7", "", "SCRUM-7 or so"} {
		resp, body := doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+orphan.ID),
			patchWorkItem{WorkItem: bad}, nil)
		requireStatus(t, resp, body, http.StatusBadRequest)
		decodeAPIError(t, body)
	}
	// An untagged run is not an orphan: 409.
	resp, body := doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+untagged.ID),
		patchWorkItem{WorkItem: "SCRUM-7"}, nil)
	requireStatus(t, resp, body, http.StatusConflict)
	decodeAPIError(t, body)

	// The one allowed transition, with a category retag riding along.
	category := "orphan-intake"
	var patched apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+orphan.ID),
		patchWorkItem{Category: &category, WorkItem: "SCRUM-7"}, &patched)
	requireStatus(t, resp, body, http.StatusOK)
	if patched.WorkItem != "SCRUM-7" || patched.Category != "orphan-intake" {
		t.Fatalf("after re-key: work_item=%q category=%q, want SCRUM-7 / orphan-intake", patched.WorkItem, patched.Category)
	}
	if got := listRunIDs(t, f, "?work_item=SCRUM-7"); len(got) != 1 || got[0] != orphan.ID {
		t.Fatalf("work_item=SCRUM-7 listed %v, want exactly %s", got, orphan.ID)
	}
	if got := listRunIDs(t, f, "?work_item=gh:agentculture/culture-nodes%23307"); len(got) != 0 {
		t.Fatalf("the gh: form still lists %v after the re-key; the transient form must not survive", got)
	}

	// Once. The second attempt finds a run that is no longer an orphan.
	resp, body = doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+orphan.ID),
		patchWorkItem{WorkItem: "SCRUM-8"}, nil)
	requireStatus(t, resp, body, http.StatusConflict)
	decodeAPIError(t, body)
	var after apipkg.RunViewOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+orphan.ID), nil, &after)
	requireStatus(t, resp, body, http.StatusOK)
	if after.Run.WorkItem != "SCRUM-7" {
		t.Fatalf("refused second re-key changed work_item to %q", after.Run.WorkItem)
	}
}

// TestPatchRunRefusedCategoryDoesNotRekeyWorkItem pins the atomicity the
// once-only re-key needs: PATCH carries two fields, and the work_item half
// can never be replayed. A body whose work_item is good and whose category
// is unusable must therefore be refused having written NOTHING — otherwise
// the caller is answered 400 by a request that nevertheless spent the
// transition, and the obvious repair (the same body, category fixed) is met
// with the 409 the half-applied re-key now earns, with no way left to set
// the category at all. The same shape covers a category UPDATE the database
// refuses; a decode failure is the half of it a test can drive honestly.
func TestPatchRunRefusedCategoryDoesNotRekeyWorkItem(t *testing.T) {
	f := newFixture(t)
	digest := publishFixtureWorkflow(t, f)
	const orphanKey = "gh:agentculture/culture-nodes#307"
	orphan := createWorkItemRun(t, f, digest, orphanKey)

	resp, body := doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+orphan.ID),
		json.RawMessage(`{"work_item":"SCRUM-7","category":42}`), nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	var after apipkg.RunViewOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+orphan.ID), nil, &after)
	requireStatus(t, resp, body, http.StatusOK)
	if after.Run.WorkItem != orphanKey {
		t.Fatalf("a refused PATCH moved work_item to %q; the once-only re-key was spent by a request that failed", after.Run.WorkItem)
	}
	if got := listRunIDs(t, f, "?work_item=SCRUM-7"); len(got) != 0 {
		t.Fatalf("work_item=SCRUM-7 lists %v after a refused PATCH, want none", got)
	}

	// So the retry is the repair: the same body, category fixed, lands both.
	var patched apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+orphan.ID),
		json.RawMessage(`{"work_item":"SCRUM-7","category":"orphan-intake"}`), &patched)
	requireStatus(t, resp, body, http.StatusOK)
	if patched.WorkItem != "SCRUM-7" || patched.Category != "orphan-intake" {
		t.Fatalf("after the repair: work_item=%q category=%q, want SCRUM-7 / orphan-intake", patched.WorkItem, patched.Category)
	}
}

// TestTriggeredRunStampsWorkItemFromEventPayload covers the engine's
// event->run minting path: a fact whose payload carries a string
// `work_item` lands as a run keyed by it (so a sweep-emitted pr-upkeep.pr
// fact is reachable by GET /runs?work_item=KEY without a second write),
// while a payload with no such field, or a non-string one, stamps nothing.
func TestTriggeredRunStampsWorkItemFromEventPayload(t *testing.T) {
	f := newFixtureWithEventAuth(t, eventTokenSecret)
	publishTriggerWorkflow(t, f, triggeredWorkflow)

	keyed := deliver(t, f, "pull-request", json.RawMessage(`{"action":"opened","work_item":"SCRUM-9"}`))
	if len(keyed.Triggered) != 1 {
		t.Fatalf("keyed delivery triggered %d runs, want 1: %+v", len(keyed.Triggered), keyed)
	}
	unkeyed := deliver(t, f, "pull-request", json.RawMessage(`{"action":"opened"}`))
	if len(unkeyed.Triggered) != 1 {
		t.Fatalf("unkeyed delivery triggered %d runs, want 1: %+v", len(unkeyed.Triggered), unkeyed)
	}
	badType := deliver(t, f, "pull-request", json.RawMessage(`{"action":"opened","work_item":42}`))
	if len(badType.Triggered) != 1 {
		t.Fatalf("non-string work_item delivery triggered %d runs, want 1: %+v", len(badType.Triggered), badType)
	}

	got := listRunIDs(t, f, "?work_item=SCRUM-9")
	if len(got) != 1 || got[0] != keyed.Triggered[0].RunID {
		t.Fatalf("work_item=SCRUM-9 listed %v, want exactly the keyed triggered run %s", got, keyed.Triggered[0].RunID)
	}
	for _, id := range []string{unkeyed.Triggered[0].RunID, badType.Triggered[0].RunID} {
		var view apipkg.RunViewOut
		resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+id), nil, &view)
		requireStatus(t, resp, body, http.StatusOK)
		if view.Run.WorkItem != "" {
			t.Errorf("run %s work_item = %q, want absent for a payload without a string work_item", id, view.Run.WorkItem)
		}
		if view.Run.Category != "" {
			t.Errorf("run %s category = %q, want untouched by the trigger", id, view.Run.Category)
		}
	}
	var view apipkg.RunViewOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+keyed.Triggered[0].RunID), nil, &view)
	requireStatus(t, resp, body, http.StatusOK)
	if view.Run.Category != "" {
		t.Errorf("keyed run category = %q, want empty: the trigger stamps work_item, never category (c41)", view.Run.Category)
	}
}
