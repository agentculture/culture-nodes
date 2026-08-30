package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/artifacts"
	artifactpg "github.com/agentculture/culture-nodes/internal/artifacts/postgres"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// TestCodeNodeStdoutRefIsReadable pins the production-shaped gap from
// SCRUM-6: headspace returned a durable stdout_ref in the code-node result,
// but the artifact row was not indexed by attempts.id. The attempt result is
// still an authoritative association between that attempt and those bytes,
// so both the listing and the named stdout read must resolve through it.
func TestCodeNodeStdoutRefIsReadable(t *testing.T) {
	f := newFixture(t)
	_, nodeRunID := createMinimalRun(t, f)

	driver := artifactpg.New(f.store, artifactpg.DefaultCapBytes)
	router := artifacts.NewRouter(driver, driver, artifactpg.DefaultCapBytes)
	stdout := []byte("code node stdout\n")
	ref, err := router.Put(context.Background(), artifacts.ArtifactMeta{
		NamespaceID: f.nsID,
		Name:        "stdout",
		MediaType:   "text/plain; charset=utf-8",
		// Deliberately no AttemptID: this is the unindexed shape observed on
		// production. The code-node result below is the surviving association.
	}, bytes.NewReader(stdout))
	if err != nil {
		t.Fatalf("store code-node stdout: %v", err)
	}

	attemptID := store.NewULID()
	result, err := json.Marshal(map[string]any{
		"state":     "succeeded",
		"artifacts": map[string]string{"stdout_ref": string(ref)},
	})
	if err != nil {
		t.Fatalf("marshal code-node result: %v", err)
	}
	es, err := storepg.NewEngineStore(f.store, f.nsID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	if err := es.InTx(context.Background(), func(ctx context.Context, tx engine.Tx) error {
		return tx.InsertAttempt(ctx, engine.Attempt{
			ID: attemptID, NodeRunID: nodeRunID, Number: 1,
			Status: engine.StatusSucceeded, Result: result,
		})
	}); err != nil {
		t.Fatalf("insert code-node attempt: %v", err)
	}

	srv, err := apipkg.NewServer(f.store, f.nsID, apipkg.WithArtifactRouter(router))
	if err != nil {
		t.Fatalf("api.NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	listResp, err := ts.Client().Get(ts.URL + "/v1alpha1/attempts/" + attemptID + "/artifacts")
	if err != nil {
		t.Fatalf("GET artifact listing: %v", err)
	}
	listBody, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK || !bytes.Contains(listBody, []byte(`"name":"stdout"`)) ||
		!bytes.Contains(listBody, []byte(string(ref))) {
		t.Fatalf("listing status=%d body=%s, want stdout entry with ref %s", listResp.StatusCode, listBody, ref)
	}

	getResp, err := ts.Client().Get(ts.URL + "/v1alpha1/attempts/" + attemptID + "/artifacts/stdout")
	if err != nil {
		t.Fatalf("GET stdout: %v", err)
	}
	got, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK || !bytes.Equal(got, stdout) {
		t.Fatalf("stdout status=%d body=%q, want 200 with %q", getResp.StatusCode, got, stdout)
	}
}
