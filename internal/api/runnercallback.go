package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

type runnerCallbackStore interface {
	RunnerOperation(context.Context, string, string) (postgres.RunnerOperation, error)
	TightenRunnerPoll(context.Context, string, string, time.Time) (bool, error)
}

type runnerCallbackResponse struct {
	OperationID string `json:"operation_id,omitempty"`
	Disposition string `json:"disposition,omitempty"`
	Error       string `json:"error,omitempty"`
}

const maxRunnerCallbackBodyBytes = 16 << 10

// handleRunnerOperationEvent accepts the optional runner completion hint.
// Its only durable effect is to move the next status poll forward.
func (s *Server) handleRunnerOperationEvent(w http.ResponseWriter, r *http.Request) {
	token, ok := runnerCallbackBearerToken(r)
	if !ok {
		writeRunnerCallbackJSON(w, http.StatusUnauthorized,
			runnerCallbackResponse{Error: "an attempt-scoped bearer token is required"})
		return
	}

	attemptID, err := s.callbackSigner.Verify(token)
	if err != nil {
		writeRunnerCallbackJSON(w, http.StatusUnauthorized, runnerCallbackResponse{Error: err.Error()})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRunnerCallbackBodyBytes))
	if err != nil {
		writeRunnerCallbackJSON(w, http.StatusBadRequest,
			runnerCallbackResponse{Error: "callback body could not be read"})
		return
	}
	var note runners.CallbackNotification
	if err := json.Unmarshal(body, &note); err != nil || note.OperationID == "" {
		writeRunnerCallbackJSON(w, http.StatusBadRequest,
			runnerCallbackResponse{Error: "callback body is not a runner-protocol notification"})
		return
	}
	if note.OperationID != r.PathValue("id") {
		writeRunnerCallbackJSON(w, http.StatusNotFound,
			runnerCallbackResponse{Error: "callback path and operation_id do not agree"})
		return
	}

	op, err := s.runnerCallbackStore.RunnerOperation(r.Context(), s.NamespaceID, attemptID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, postgres.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeRunnerCallbackJSON(w, status, runnerCallbackResponse{Error: err.Error()})
		return
	}
	if op.OperationID != note.OperationID {
		writeRunnerCallbackJSON(w, http.StatusNotFound,
			runnerCallbackResponse{Error: "callback token does not authorize this operation"})
		return
	}

	tightened, err := s.runnerCallbackStore.TightenRunnerPoll(r.Context(), s.NamespaceID, attemptID, time.Now().UTC())
	if err != nil {
		writeRunnerCallbackJSON(w, http.StatusInternalServerError, runnerCallbackResponse{Error: err.Error()})
		return
	}
	disposition := "already_scheduled"
	if tightened {
		disposition = "sample_advanced"
	}
	writeRunnerCallbackJSON(w, http.StatusAccepted,
		runnerCallbackResponse{OperationID: note.OperationID, Disposition: disposition})
}

func runnerCallbackBearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get(runners.AuthorizationHeader)
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

func writeRunnerCallbackJSON(w http.ResponseWriter, status int, body runnerCallbackResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
