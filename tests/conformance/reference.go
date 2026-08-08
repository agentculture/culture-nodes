package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// ReferenceActor is a correct, minimal PRD §13 actor.
//
// It exists for two reasons. The first is that a conformance kit nobody has
// ever passed is a kit that might be asserting something impossible, so the
// suite runs against this actor on every `go test ./...` and the kit is
// itself under test. The second is that an adapter author who fails a check
// needs to see what passing looks like, and prose in a doc comment is a much
// worse answer than seventy lines of Go they can read.
//
// It is deliberately *only* the protocol. It runs no work, holds no state
// beyond an idempotency map, and decides synchronous versus asynchronous by
// reading a flag out of the input — because none of that is what §13 is
// about, and a reference implementation that also had business logic would
// make it harder to see which parts are the protocol.
//
// The input flags it understands:
//
//	{"async": true}   answer 202 and report through callbacks
//	{"reject": true}  answer 422: an input the actor refuses
//	{"delay_ms": N}   how long the async path takes before completing
type ReferenceActor struct {
	server    *httptest.Server
	authToken string
	client    *http.Client

	mu        sync.Mutex
	responses map[string]recordedResponse
	cancelled map[string]bool
	// callbackRetries bounds how many times a refused callback delivery is
	// redelivered. §13.4 presupposes redelivery; a bound stops a permanently
	// refusing receiver from being retried forever.
	callbackRetries int
}

type recordedResponse struct {
	status int
	body   []byte
}

// NewReferenceActor starts the reference actor. Close it when done.
// authToken, when non-empty, is the bearer credential it requires.
func NewReferenceActor(authToken string) *ReferenceActor {
	a := &ReferenceActor{
		authToken:       authToken,
		client:          &http.Client{Timeout: 10 * time.Second},
		responses:       make(map[string]recordedResponse),
		cancelled:       make(map[string]bool),
		callbackRetries: 5,
	}
	a.server = httptest.NewServer(http.HandlerFunc(a.serve))
	return a
}

// URL is the actor's base URL.
func (a *ReferenceActor) URL() string { return a.server.URL }

// Close shuts the actor down.
func (a *ReferenceActor) Close() { a.server.Close() }

func (a *ReferenceActor) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == actors.InvocationPath:
		a.invoke(w, r)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
		a.cancel(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// invoke implements §13.1.
func (a *ReferenceActor) invoke(w http.ResponseWriter, r *http.Request) {
	// §13.1's Authorization header. Answering 401 (rather than, say, 400) is
	// what makes an adapter classify this as auth_or_policy and stop retrying.
	if a.authToken != "" && bearer(r) != a.authToken {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "a scoped workload token is required"})
		return
	}

	// §13.1's Idempotency-Key. Without one there is no way to tell a retry
	// from a second dispatch, so it is required rather than optional.
	key := r.Header.Get(actors.IdempotencyKeyHeader)
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key is required"})
		return
	}

	// §20.3: a key already accepted replays its recorded response byte for
	// byte. The work is not performed a second time.
	a.mu.Lock()
	recorded, replay := a.responses[key]
	a.mu.Unlock()
	if replay {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(recorded.status)
		_, _ = w.Write(recorded.body)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body could not be read"})
		return
	}
	var req actors.InvocationRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is not a §13.1 invocation"})
		return
	}
	if req.ProtocolVersion != actors.ProtocolVersion {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("protocol_version %q is not supported", req.ProtocolVersion),
		})
		return
	}

	var flags struct {
		Async   bool `json:"async"`
		Reject  bool `json:"reject"`
		DelayMS int  `json:"delay_ms"`
	}
	if len(req.Input) > 0 {
		_ = json.Unmarshal(req.Input, &flags)
	}

	switch {
	case flags.Reject:
		// A refused input is 422: the request was well-formed and the actor
		// will not accept it. Answering 500 here would be classified as an
		// execution failure and would mislead an operator about who is wrong.
		a.record(key, http.StatusUnprocessableEntity, map[string]any{
			"error":  "input does not satisfy this actor's contract",
			"detail": "the conformance reference actor rejects any input carrying {\"reject\": true}",
		}, w)

	case flags.Async:
		accepted := actors.AsyncAccepted{
			InvocationID:          "ref_" + req.AttemptID,
			HeartbeatAfterSeconds: 1,
			SupportsCancellation:  true,
		}
		a.record(key, http.StatusAccepted, accepted, w)
		go a.reportAsync(req, accepted.InvocationID, time.Duration(flags.DelayMS)*time.Millisecond)

	default:
		a.record(key, http.StatusOK, actors.InvocationResult{
			Outcome:      "completed",
			Output:       json.RawMessage(fmt.Sprintf(`{"echo":%s,"node":%q}`, string(nonEmpty(req.Input)), req.Node.ID)),
			LedgerDelta:  &actors.LedgerDelta{},
			ArtifactRefs: []string{},
			Usage:        &actors.Usage{InputTokens: 0, OutputTokens: 0},
		}, w)
	}
}

// record writes a response and remembers it under the idempotency key, so a
// re-invocation replays exactly what was said the first time.
func (a *ReferenceActor) record(key string, status int, body any, w http.ResponseWriter) {
	encoded, err := json.Marshal(body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "response could not be encoded"})
		return
	}
	a.mu.Lock()
	a.responses[key] = recordedResponse{status: status, body: encoded}
	a.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// cancel implements §13.6. It is best-effort by design: the actor records the
// instruction and answers, and the control plane's state does not depend on
// what it does next.
func (a *ReferenceActor) cancel(w http.ResponseWriter, r *http.Request) {
	if a.authToken != "" && bearer(r) != a.authToken {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "a scoped workload token is required"})
		return
	}
	var req actors.CancelRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.InvocationID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invocation_id is required"})
		return
	}
	a.mu.Lock()
	a.cancelled[req.InvocationID] = true
	a.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// reportAsync is the §13.4 callback stream: accepted, a heartbeat, then a
// terminal event. Every event carries a stable id and the next sequence
// number, and the terminal one is redelivered — with the SAME id — if the
// receiver refuses it.
func (a *ReferenceActor) reportAsync(req actors.InvocationRequest, invocationID string, delay time.Duration) {
	sequence := int64(0)
	next := func() int64 { sequence++; return sequence }

	acceptedPayload, _ := json.Marshal(actors.AcceptedPayload{
		InvocationID:          invocationID,
		HeartbeatAfterSeconds: 1,
	})
	a.postEvent(req, actors.CallbackEvent{
		EventID: invocationID + "-accepted", Sequence: next(),
		Kind: actors.EventAccepted, Payload: acceptedPayload,
	})

	a.postEvent(req, actors.CallbackEvent{
		EventID: invocationID + "-hb-1", Sequence: next(), Kind: actors.EventHeartbeat,
	})

	if delay > 0 {
		time.Sleep(delay)
	}

	a.mu.Lock()
	cancelled := a.cancelled[invocationID]
	a.mu.Unlock()
	if cancelled {
		failed, _ := json.Marshal(actors.FailedPayload{
			Class:   actors.ClassCancelled,
			Message: "cancelled at the actor's request",
		})
		a.postEvent(req, actors.CallbackEvent{
			EventID: invocationID + "-failed", Sequence: next(),
			Kind: actors.EventFailed, Payload: failed,
		})
		return
	}

	completed, _ := json.Marshal(actors.CompletedPayload{
		Outcome:      "completed",
		Output:       json.RawMessage(fmt.Sprintf(`{"echo":%s,"node":%q}`, string(nonEmpty(req.Input)), req.Node.ID)),
		LedgerDelta:  &actors.LedgerDelta{},
		ArtifactRefs: []string{},
	})
	a.postEvent(req, actors.CallbackEvent{
		EventID: invocationID + "-completed", Sequence: next(),
		Kind: actors.EventCompleted, Payload: completed,
	})
}

// postEvent delivers one event, retrying on a non-2xx answer with the same
// event id and sequence — which is exactly what makes a receiver's
// deduplication meaningful.
func (a *ReferenceActor) postEvent(req actors.InvocationRequest, ev actors.CallbackEvent) {
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	for attempt := 0; attempt <= a.callbackRetries; attempt++ {
		httpReq, err := http.NewRequest(http.MethodPost, req.Callback.URL, bytes.NewReader(body))
		if err != nil {
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+req.Callback.Token)

		resp, err := a.client.Do(httpReq)
		if err == nil {
			status := resp.StatusCode
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if status >= 200 && status < 300 {
				return
			}
			if status == http.StatusUnauthorized || status == http.StatusNotFound {
				// The credential or the attempt is gone. Retrying cannot fix
				// either, and hammering the control plane would be worse than
				// giving up.
				return
			}
		}
		time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
