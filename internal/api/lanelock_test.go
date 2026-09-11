package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The async completion path locks the lane THROUGH THE SERVER (code-review
// fix D): the callback route (*api.Server).Handler mounts is wired with the
// lane locker, so a `failed` event carrying class credential_spent posted
// by a real bridge lands the control-plane lock, and the actor read
// surface shows it. internal/actors/lanelock_test.go proves the handler;
// this proves the wiring, which is the half that was missing in production.
func TestAsyncCredentialSpentCallbackLocksTheLaneThroughTheServer(t *testing.T) {
	signer, err := actors.NewTokenSigner([]byte(callbackTestSecret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	f := newCallbackFixture(t, signer)
	ctx := context.Background()

	const actorKey = "test/async-spent-lane"
	registerActor(t, f, actorKey, "http://127.0.0.1:9/never-called")
	var actorID string
	if err := f.store.Pool().QueryRow(ctx,
		`SELECT id FROM actors WHERE namespace_id = $1 AND actor_key = $2`, f.nsID, actorKey).Scan(&actorID); err != nil {
		t.Fatalf("read actor id: %v", err)
	}

	source := readFixtureWorkflow(t, "minimal.workflow.yaml")
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
	nodeRun := view.NodeRuns[0]
	claimed := f.claim("worker-1", nodeRun.ID)
	attemptID := "att_" + store.NewULID()
	if err := f.store.StartAsyncWait(ctx, storepg.StartAsyncWaitInput{
		WorkID: claimed.ID, WorkerID: "worker-1", FencingToken: claimed.FencingToken, Attempt: int(claimed.Attempt),
		NamespaceID: f.nsID, RunID: run.ID, NodeRunID: nodeRun.ID, NodeID: nodeRun.NodeID,
		AttemptID: attemptID, ActorRef: "actor://" + actorKey + "@sha256:abc", ActorID: actorID,
		InvocationID: "ext-" + store.NewULID(), Deadline: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("StartAsyncWait: %v", err)
	}

	token, err := signer.Mint(attemptID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	payload, _ := json.Marshal(actors.FailedPayload{Class: actors.ClassCredentialSpent, Message: "refresh token revoked"})
	event, _ := json.Marshal(actors.CallbackEvent{EventID: "ev-spent", Sequence: 1, Kind: actors.EventFailed, Payload: payload})
	req, err := http.NewRequest(http.MethodPost, f.url("/v1/attempts/"+attemptID+"/events"), bytes.NewReader(event))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	cbResp, err := f.client.Do(req)
	if err != nil {
		t.Fatalf("POST callback: %v", err)
	}
	defer cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusAccepted {
		t.Fatalf("callback status = %d, want 202", cbResp.StatusCode)
	}

	row, ok, err := f.store.ActorLiveness(ctx, f.nsID, actorKey)
	if err != nil || !ok {
		t.Fatalf("ActorLiveness = (%v, %v), want the locked row: the server's callback route must be wired to the lane locker", ok, err)
	}
	if !row.Locked || row.LockedByRunID != run.ID || row.LockedByAttemptID != attemptID {
		t.Fatalf("row = %+v, want locked by run %s attempt %s", row, run.ID, attemptID)
	}

	var got actorLivenessWire
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/actors/"+actorID), nil, &got)
	requireStatus(t, resp, body, http.StatusOK)
	if got.Liveness == nil || !got.Liveness.Locked || got.Liveness.Live {
		t.Fatalf("GET actor liveness = %+v, want locked and not live: %s", got.Liveness, body)
	}
}
