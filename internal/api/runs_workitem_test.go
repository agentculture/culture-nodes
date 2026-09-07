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

// TestPatchRunRefusesWorkItem is t1's first criterion: PATCH /runs/{id}
// still accepts category and nothing else. work_item is set at creation
// (or stamped by the trigger) and is not retaggable; a body naming it is
// refused with a structured 400 rather than silently ignored, the same way
// name/description are.
func TestPatchRunRefusesWorkItem(t *testing.T) {
	f := newFixture(t)
	digest := publishFixtureWorkflow(t, f)
	run := createWorkItemRun(t, f, digest, "SCRUM-9")

	type patchWithWorkItem struct {
		Category *string `json:"category,omitempty"`
		WorkItem *string `json:"work_item,omitempty"`
	}
	category, moved := "audit", "SCRUM-10"

	resp, body := doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+run.ID),
		patchWithWorkItem{Category: &category, WorkItem: &moved}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	resp, body = doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+run.ID),
		patchWithWorkItem{WorkItem: &moved}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
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
