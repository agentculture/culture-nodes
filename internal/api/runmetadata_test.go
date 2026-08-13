package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
)

// createRunWithMetadataReq mirrors createRunReq (api_test.go) but adds task
// t3's optional name/description/category — internal/api's own
// createRunRequest is unexported, so this package encodes the documented
// wire shape the same way api_test.go's siblings do.
type createRunWithMetadataReq struct {
	WorkflowDigest string          `json:"workflow_digest"`
	Input          json.RawMessage `json:"input"`
	Name           string          `json:"name,omitempty"`
	Description    string          `json:"description,omitempty"`
	Category       string          `json:"category,omitempty"`
}

type patchRunReq struct {
	Category    *string `json:"category,omitempty"`
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
}

// publishFixtureWorkflow publishes edge-order-ordered.workflow.yaml, the
// same fixture api_test.go's own run-lifecycle tests use, and returns its
// digest.
func publishFixtureWorkflow(t *testing.T, f *fixture) string {
	t.Helper()
	source := readFixtureWorkflow(t, "edge-order-ordered.workflow.yaml")
	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	requireStatus(t, resp, body, http.StatusCreated)
	return published.Digest
}

// TestCreateRunContractOldBodyStillWorks proves task t3's optional
// name/description/category are additive: a body carrying exactly the
// pre-t3 shape ({workflow_digest, input}, no metadata keys at all) still
// succeeds and reports every new field absent — the "existing clients keep
// working unchanged" acceptance criterion.
func TestCreateRunContractOldBodyStillWorks(t *testing.T) {
	f := newFixture(t)
	digest := publishFixtureWorkflow(t, f)

	// The exact pre-t3 body shape, marshaled from a struct that has no
	// metadata fields at all — proof this is not merely "the new fields
	// happened to be empty" but the literal old contract.
	oldBody := createRunReq{WorkflowDigest: digest, Input: json.RawMessage(`{}`)}

	var run apipkg.RunOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"), oldBody, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	if run.Name != "" {
		t.Errorf("name = %q, want empty for a request that never sent one", run.Name)
	}
	if run.Description != "" {
		t.Errorf("description = %q, want empty", run.Description)
	}
	if run.Category != "" {
		t.Errorf("category = %q, want empty", run.Category)
	}
}

// TestCreateRunWithMetadataPersistsAndRenders proves name/description/
// category round-trip through createRun, getRun, and listRuns.
func TestCreateRunWithMetadataPersistsAndRenders(t *testing.T) {
	f := newFixture(t)
	digest := publishFixtureWorkflow(t, f)

	var run apipkg.RunOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunWithMetadataReq{
			WorkflowDigest: digest,
			Input:          json.RawMessage(`{}`),
			Name:           "nightly retry-backoff pass",
			Description:    "adds retry backoff to the queue consumer",
			Category:       "implement",
		}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	if run.Name != "nightly retry-backoff pass" {
		t.Errorf("name = %q", run.Name)
	}
	if run.Description != "adds retry backoff to the queue consumer" {
		t.Errorf("description = %q", run.Description)
	}
	if run.Category != "implement" {
		t.Errorf("category = %q", run.Category)
	}
	// A named run never carries a guessed hint alongside its real name.
	if run.DisplayHint != "" {
		t.Errorf("display_hint = %q, want empty when name is set", run.DisplayHint)
	}

	// getRun renders the same metadata.
	var view apipkg.RunViewOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID), nil, &view)
	requireStatus(t, resp, body, http.StatusOK)
	if view.Run.Name != run.Name || view.Run.Category != run.Category {
		t.Errorf("getRun metadata = %+v, want name/category to match createRun's response", view.Run)
	}

	// listRuns renders it too.
	var list apipkg.RunListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)
	found := false
	for _, item := range list.Items {
		if item.ID == run.ID {
			found = true
			if item.Name != run.Name {
				t.Errorf("listRuns name = %q, want %q", item.Name, run.Name)
			}
			if item.Category != run.Category {
				t.Errorf("listRuns category = %q, want %q", item.Category, run.Category)
			}
		}
	}
	if !found {
		t.Fatalf("run %s not present in listRuns", run.ID)
	}
}

// TestCreateRunWithoutNameListsWithDerivedHint proves the input-derived
// display hint: a run created with no name still lists with a truncated
// hint pulled from an instruction/request-ish field of its input, and that
// hint is absent (never fabricated) when no such field is present.
func TestCreateRunWithoutNameListsWithDerivedHint(t *testing.T) {
	f := newFixture(t)
	digest := publishFixtureWorkflow(t, f)

	cases := []struct {
		name      string
		input     string
		wantHint  string
		wantEmpty bool
	}{
		{
			name:     "instruction field (nodes-operator assign shape)",
			input:    `{"instruction":"add retry backoff to the queue consumer","repo":"agentculture/culture-nodes"}`,
			wantHint: "add retry backoff to the queue consumer",
		},
		{
			name:     "request field (examples/delivery-loop shape)",
			input:    `{"request":"add retry backoff to the queue consumer"}`,
			wantHint: "add retry backoff to the queue consumer",
		},
		{
			name:     "compound *_instruction field (examples/independent-review shape)",
			input:    `{"build_instruction":"add retry backoff","review_instruction":"review the diff"}`,
			wantHint: "add retry backoff",
		},
		{
			name:      "no candidate field at all",
			input:     `{"repository":"agentculture/culture-nodes"}`,
			wantEmpty: true,
		},
		{
			name:      "empty input",
			input:     `{}`,
			wantEmpty: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var run apipkg.RunOut
			resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
				createRunReq{WorkflowDigest: digest, Input: json.RawMessage(tc.input)}, &run)
			requireStatus(t, resp, body, http.StatusCreated)

			if run.Name != "" {
				t.Fatalf("run.Name = %q, want empty (no name was sent)", run.Name)
			}

			var list apipkg.RunListOut
			resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs"), nil, &list)
			requireStatus(t, resp, body, http.StatusOK)

			var got string
			found := false
			for _, item := range list.Items {
				if item.ID == run.ID {
					found = true
					got = item.DisplayHint
				}
			}
			if !found {
				t.Fatalf("run %s not present in listRuns", run.ID)
			}
			if tc.wantEmpty {
				if got != "" {
					t.Errorf("display_hint = %q, want empty", got)
				}
				return
			}
			if got != tc.wantHint {
				t.Errorf("display_hint = %q, want %q", got, tc.wantHint)
			}
		})
	}
}

// TestDisplayHintTruncatesLongInput proves the hint is bounded to a sane
// length rather than reproducing an arbitrarily long instruction verbatim.
func TestDisplayHintTruncatesLongInput(t *testing.T) {
	f := newFixture(t)
	digest := publishFixtureWorkflow(t, f)

	long := strings.Repeat("a", 500)
	input, err := json.Marshal(map[string]string{"instruction": long})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	var run apipkg.RunOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: digest, Input: input}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	var list apipkg.RunListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs"), nil, &list)
	requireStatus(t, resp, body, http.StatusOK)

	var hint string
	for _, item := range list.Items {
		if item.ID == run.ID {
			hint = item.DisplayHint
		}
	}
	if hint == "" {
		t.Fatal("display_hint is empty, want a truncated hint")
	}
	if len([]rune(hint)) >= 500 {
		t.Errorf("display_hint has length %d, want it truncated well below the 500-rune input", len([]rune(hint)))
	}
	if !strings.HasSuffix(hint, "…") {
		t.Errorf("display_hint = %q, want a truncation ellipsis suffix", hint)
	}
}

// TestPatchRunCategoryRetagsAndRejectsNameDescription is this task's
// centerpiece for the immutability half of frame decision q4: category
// alone is retaggable via PATCH; any attempt to also carry name or
// description in the same request body is refused with a structured error
// rather than silently ignored.
func TestPatchRunCategoryRetagsAndRejectsNameDescription(t *testing.T) {
	f := newFixture(t)
	digest := publishFixtureWorkflow(t, f)

	var run apipkg.RunOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunWithMetadataReq{WorkflowDigest: digest, Input: json.RawMessage(`{}`), Name: "keep-me", Category: "explore"}, &run)
	requireStatus(t, resp, body, http.StatusCreated)

	// Retag category alone: succeeds, name is untouched.
	newCategory := "audit"
	var patched apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+run.ID),
		patchRunReq{Category: &newCategory}, &patched)
	requireStatus(t, resp, body, http.StatusOK)
	if patched.Category != "audit" {
		t.Errorf("category = %q, want %q", patched.Category, "audit")
	}
	if patched.Name != "keep-me" {
		t.Errorf("name = %q, want it unchanged by a category-only PATCH", patched.Name)
	}

	// getRun confirms the retag persisted.
	var view apipkg.RunViewOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID), nil, &view)
	requireStatus(t, resp, body, http.StatusOK)
	if view.Run.Category != "audit" {
		t.Errorf("getRun category after patch = %q, want %q", view.Run.Category, "audit")
	}

	// Attempting to also patch name is refused with a structured 400, and
	// the run's category is untouched by the refused request.
	attemptedName := "renamed"
	resp, body = doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+run.ID),
		patchRunReq{Category: &newCategory, Name: &attemptedName}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	// Attempting to patch description alone (no category at all) is also
	// refused — description is immutable, and this endpoint accepts
	// category only.
	attemptedDescription := "new description"
	resp, body = doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+run.ID),
		patchRunReq{Description: &attemptedDescription}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	// A body with no category at all is refused too.
	resp, body = doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+run.ID), struct{}{}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	// Patching an unknown run is a 404.
	resp, body = doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/does-not-exist"),
		patchRunReq{Category: &newCategory}, nil)
	requireStatus(t, resp, body, http.StatusNotFound)
	decodeAPIError(t, body)
}

// TestPatchRunCategoryCanClear proves an empty-string category clears the
// tag rather than being rejected or stored as a literal empty string
// distinct from "no category".
func TestPatchRunCategoryCanClear(t *testing.T) {
	f := newFixture(t)
	digest := publishFixtureWorkflow(t, f)

	var run apipkg.RunOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunWithMetadataReq{WorkflowDigest: digest, Input: json.RawMessage(`{}`), Category: "explore"}, &run)
	requireStatus(t, resp, body, http.StatusCreated)
	if run.Category != "explore" {
		t.Fatalf("category = %q, want %q", run.Category, "explore")
	}

	empty := ""
	var patched apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPatch, f.url("/v1alpha1/runs/"+run.ID),
		patchRunReq{Category: &empty}, &patched)
	requireStatus(t, resp, body, http.StatusOK)
	if patched.Category != "" {
		t.Errorf("category after clearing = %q, want empty", patched.Category)
	}
}
