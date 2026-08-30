package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

type runnerCallbackStoreStub struct {
	operation postgres.RunnerOperation
	tightened bool
}

func (s *runnerCallbackStoreStub) RunnerOperation(_ context.Context, namespaceID, attemptID string) (postgres.RunnerOperation, error) {
	if namespaceID != s.operation.NamespaceID || attemptID != s.operation.AttemptID {
		return postgres.RunnerOperation{}, fmt.Errorf("unexpected runner operation lookup %s/%s", namespaceID, attemptID)
	}
	return s.operation, nil
}

func (s *runnerCallbackStoreStub) TightenRunnerPoll(_ context.Context, namespaceID, attemptID string, _ time.Time) (bool, error) {
	if namespaceID != s.operation.NamespaceID || attemptID != s.operation.AttemptID {
		return false, fmt.Errorf("unexpected runner poll update %s/%s", namespaceID, attemptID)
	}
	s.tightened = true
	return true, nil
}

func TestRunnerOperationEventRouteAcceptsTheRunnersExactPost(t *testing.T) {
	const (
		namespaceID = "ns_runner_callback"
		attemptID   = "att_runner_callback"
		operationID = "op_runner_callback"
		secret      = "0123456789abcdef0123456789abcdef"
	)
	signer, err := actors.NewTokenSigner([]byte(secret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	token, err := signer.Mint(attemptID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	store := &runnerCallbackStoreStub{operation: postgres.RunnerOperation{
		NamespaceID: namespaceID,
		AttemptID:   attemptID,
		OperationID: operationID,
	}}
	s := &Server{
		NamespaceID:         namespaceID,
		callbackSigner:      signer,
		runnerCallbackStore: store,
	}

	note := runners.CallbackNotification{OperationID: operationID, State: runners.StateCompleted}
	body, err := json.Marshal(note)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf(runners.CallbackPathFormat, operationID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(runners.AuthorizationHeader, "Bearer "+token)
	req.Header.Set(runners.ProtocolVersionHeader, runners.ProtocolVersion)
	rw := httptest.NewRecorder()

	s.Handler().ServeHTTP(rw, req)
	if rw.Code < 200 || rw.Code >= 300 {
		t.Fatalf("runner callback status = %d, want 2xx; body: %s", rw.Code, rw.Body.String())
	}
	if !store.tightened {
		t.Fatal("runner callback did not tighten the operation's next status sample")
	}
}
