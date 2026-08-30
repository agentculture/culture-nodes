package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
)

func TestListRunsFiltersByWorkflowKeyAndState(t *testing.T) {
	f := newFixture(t)
	source := string(readFixtureWorkflow(t, "edge-order-ordered.workflow.yaml"))

	publish := func(key string) apipkg.WorkflowVersionOut {
		t.Helper()
		var workflow apipkg.WorkflowVersionOut
		body := workflowSourceReq{Format: "yaml", Source: strings.Replace(source, "name: edge-order", "name: "+key, 1)}
		resp, raw := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"), body, &workflow)
		requireStatus(t, resp, raw, http.StatusCreated)
		return workflow
	}
	create := func(workflow apipkg.WorkflowVersionOut, state string) apipkg.RunOut {
		t.Helper()
		var run apipkg.RunOut
		resp, raw := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"), createRunReq{
			WorkflowDigest: workflow.Digest, Input: json.RawMessage(`{}`),
		}, &run)
		requireStatus(t, resp, raw, http.StatusCreated)
		if _, err := f.store.Pool().Exec(context.Background(), `UPDATE runs SET status = $1 WHERE id = $2`, state, run.ID); err != nil {
			t.Fatalf("set run %s state: %v", run.ID, err)
		}
		return run
	}

	sweep := publish("pr-upkeep-sweep-cycle")
	other := publish("another-workflow")
	want := create(sweep, "failed")
	create(sweep, "completed")
	create(other, "failed")

	var listed apipkg.RunListOut
	resp, raw := doJSON(t, f.client, http.MethodGet,
		f.url("/v1alpha1/runs?workflow_key=pr-upkeep-sweep-cycle&state=failed"), nil, &listed)
	requireStatus(t, resp, raw, http.StatusOK)
	if len(listed.Items) != 1 || listed.Items[0].ID != want.ID {
		t.Fatalf("items = %+v, want only failed sweep run %s", listed.Items, want.ID)
	}
	if listed.Items[0].WorkflowKey != "pr-upkeep-sweep-cycle" {
		t.Fatalf("workflow_key = %q, want pr-upkeep-sweep-cycle", listed.Items[0].WorkflowKey)
	}
}
