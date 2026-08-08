package conformance

import (
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

// Receiver is the callback endpoint the kit advertises in §13.1's callback
// block: a real HTTP server that authenticates the attempt-scoped token the
// same way the control plane does, records every §13.4 event, and can be told
// to refuse a delivery so an actor's redelivery behaviour becomes observable.
//
// It is a recording receiver rather than the production ingest because the
// kit is testing the ACTOR, not the control plane. It still uses the real
// actors.TokenSigner, so an actor that mangles or drops the token fails here
// exactly as it would in production.
type Receiver struct {
	signer *actors.TokenSigner
	server *httptest.Server
	base   string

	mu       sync.Mutex
	events   map[string][]actors.CallbackEvent
	rejected map[string]int
	refuse   map[string]int
	// settled records that a terminal delivery was ACCEPTED (answered 202).
	// A terminal event the receiver refused has not been ingested, so waiting
	// on its mere arrival would race a conforming actor's redelivery.
	settled map[string]bool
}

// NewReceiver starts a callback receiver. Close it when the suite is done.
//
// advertiseBase overrides the URL handed to actors; empty means advertise the
// server's own address, which is what an in-process actor needs.
func NewReceiver(signer *actors.TokenSigner, advertiseBase string) *Receiver {
	r := &Receiver{
		signer:   signer,
		events:   make(map[string][]actors.CallbackEvent),
		rejected: make(map[string]int),
		refuse:   make(map[string]int),
		settled:  make(map[string]bool),
	}
	r.server = httptest.NewServer(http.HandlerFunc(r.serve))
	r.base = strings.TrimRight(advertiseBase, "/")
	if r.base == "" {
		r.base = r.server.URL
	}
	return r
}

// Close shuts the receiver down.
func (r *Receiver) Close() { r.server.Close() }

// CallbackURL is the §13.1 callback URL for an attempt.
func (r *Receiver) CallbackURL(attemptID string) string {
	return r.base + fmt.Sprintf(actors.CallbackEventsPathFormat, attemptID)
}

// RefuseNextTerminal makes the receiver answer 503 to the next n terminal
// deliveries for an attempt, so a conforming actor's redelivery — with the
// same event id — becomes observable.
func (r *Receiver) RefuseNextTerminal(attemptID string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refuse[attemptID] = n
}

// Events returns the events recorded for an attempt, in arrival order.
func (r *Receiver) Events(attemptID string) []actors.CallbackEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]actors.CallbackEvent(nil), r.events[attemptID]...)
}

// RefusedDeliveries is how many deliveries the receiver answered 503 to.
func (r *Receiver) RefusedDeliveries(attemptID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rejected[attemptID]
}

// WaitForTerminal blocks until a terminal §13.4 event has been ACCEPTED for
// an attempt, or the timeout elapses. It reports whether one was.
//
// "Accepted" rather than "arrived" is the load-bearing distinction: when the
// receiver has been told to refuse a delivery, the refused terminal event has
// not been ingested, and returning on its arrival would race the actor's
// redelivery and make the retry check flaky.
func (r *Receiver) WaitForTerminal(attemptID string, timeout time.Duration) (actors.CallbackEvent, bool) {
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		settled := r.settled[attemptID]
		r.mu.Unlock()
		if settled {
			for _, ev := range r.Events(attemptID) {
				if ev.Kind.Terminal() {
					return ev, true
				}
			}
		}
		if time.Now().After(deadline) {
			return actors.CallbackEvent{}, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *Receiver) serve(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "callback events are POSTed", http.StatusMethodNotAllowed)
		return
	}
	attemptID, ok := attemptIDFromPath(req.URL.Path)
	if !ok {
		http.Error(w, "unexpected callback path", http.StatusNotFound)
		return
	}

	token := bearer(req)
	if token == "" {
		http.Error(w, "an attempt-scoped bearer token is required", http.StatusUnauthorized)
		return
	}
	if err := r.signer.VerifyFor(token, attemptID); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, 4<<20))
	if err != nil {
		http.Error(w, "callback body could not be read", http.StatusBadRequest)
		return
	}
	var ev actors.CallbackEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "callback body is not a §13.4 event", http.StatusBadRequest)
		return
	}

	r.mu.Lock()
	r.events[attemptID] = append(r.events[attemptID], ev)
	refuse := ev.Kind.Terminal() && r.refuse[attemptID] > 0
	switch {
	case refuse:
		r.refuse[attemptID]--
		r.rejected[attemptID]++
	case ev.Kind.Terminal():
		r.settled[attemptID] = true
	}
	r.mu.Unlock()

	if refuse {
		// A 5xx is the one answer that means "not ingested, please retry"
		// (see internal/actors/callback_http.go).
		http.Error(w, "injected failure: redeliver this event", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func attemptIDFromPath(path string) (string, bool) {
	const prefix = "/v1/attempts/"
	const suffix = "/events"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

func bearer(req *http.Request) string {
	header := req.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
