package actors

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// The HTTP face of callback ingest: the endpoint §13.1's `callback.url`
// points at. It is a plain http.Handler rather than a server so the API
// process can mount it on whatever mux it already has, and so a test can
// stand one up in-process and let a real actor POST to it.
//
// The status codes matter to an actor's retry logic, so they are chosen
// deliberately:
//
//   - 202 for anything the ingest handled, including a duplicate, a
//     reordering, and a late completion. All three are the protocol working;
//     answering 4xx would make a conforming actor retry forever.
//   - 401 for a refused token, so an actor with a stale token knows to ask
//     for a fresh invocation rather than to back off.
//   - 404 for an attempt with no in-flight invocation.
//   - 400 for a body that is not a §13.4 event.
//   - 500 for an infrastructure failure — the one case where the actor
//     SHOULD retry, because the event has not been ingested.

// callbackPathPrefix is the fixed part of §13.1's callback URL:
// /v1/attempts/<attempt-id>/events.
const callbackPathPrefix = "/v1/attempts/"

const callbackPathSuffix = "/events"

// CallbackHandler serves §13.4 events.
type CallbackHandler struct {
	deps CallbackDeps
	// maxBodyBytes bounds a callback body. An actor may report a large
	// output, but not an unbounded one.
	maxBodyBytes int64
}

// DefaultMaxCallbackBodyBytes bounds a callback request body.
const DefaultMaxCallbackBodyBytes int64 = 4 << 20 // 4 MiB

// NewCallbackHandler returns the handler for §13.1's callback URL.
func NewCallbackHandler(deps CallbackDeps) *CallbackHandler {
	return &CallbackHandler{deps: deps, maxBodyBytes: DefaultMaxCallbackBodyBytes}
}

// callbackResponse is the body this endpoint returns. It tells a conforming
// actor what happened to its event, which is the difference between an
// adapter author debugging a dropped completion in minutes and in days.
type callbackResponse struct {
	AttemptID   string `json:"attempt_id,omitempty"`
	Disposition string `json:"disposition,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Error       string `json:"error,omitempty"`
}

func (h *CallbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeCallbackJSON(w, http.StatusMethodNotAllowed, callbackResponse{Error: "callback events are POSTed"})
		return
	}
	if _, ok := attemptIDFromPath(r.URL.Path); !ok {
		writeCallbackJSON(w, http.StatusNotFound, callbackResponse{
			Error: "callback path must be " + callbackPathPrefix + "<attempt-id>" + callbackPathSuffix,
		})
		return
	}

	token, ok := bearerToken(r)
	if !ok {
		writeCallbackJSON(w, http.StatusUnauthorized, callbackResponse{Error: "an attempt-scoped bearer token is required"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, h.maxBodyBytes))
	if err != nil {
		writeCallbackJSON(w, http.StatusBadRequest, callbackResponse{Error: "callback body could not be read"})
		return
	}
	var ev CallbackEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		writeCallbackJSON(w, http.StatusBadRequest, callbackResponse{Error: "callback body is not a §13.4 event: " + err.Error()})
		return
	}

	result, err := HandleCallback(r.Context(), h.deps, token, ev)
	switch {
	case err == nil:
		writeCallbackJSON(w, http.StatusAccepted, callbackResponse{
			AttemptID:   result.AttemptID,
			Disposition: string(result.Disposition),
			Detail:      result.Diagnostic,
		})
	case errors.Is(err, ErrToken):
		writeCallbackJSON(w, http.StatusUnauthorized, callbackResponse{Error: err.Error()})
	case errors.Is(err, ErrUnknownAttempt):
		writeCallbackJSON(w, http.StatusNotFound, callbackResponse{Error: err.Error()})
	default:
		// Distinguishing "your event was malformed" from "our store is down"
		// without a typed error would mean guessing; the ingest validates the
		// event shape up front and returns a plain error for it, so treat a
		// bare error as ours and let the actor retry.
		writeCallbackJSON(w, http.StatusInternalServerError, callbackResponse{Error: err.Error()})
	}
}

// attemptIDFromPath extracts the attempt id from §13.1's callback URL shape.
// The id is used only to validate the path: the authoritative attempt id
// comes from the signed token, never from the URL.
func attemptIDFromPath(path string) (string, bool) {
	if !strings.HasPrefix(path, callbackPathPrefix) || !strings.HasSuffix(path, callbackPathSuffix) {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, callbackPathPrefix), callbackPathSuffix)
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

func writeCallbackJSON(w http.ResponseWriter, status int, body callbackResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

var _ http.Handler = (*CallbackHandler)(nil)
