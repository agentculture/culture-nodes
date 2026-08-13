package actors_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"context"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
)

// The §13.5 classification table, driven end-to-end: each row stands up an
// actor that returns the status, invokes it, and asserts the class, the
// retryability, and the technical status the engine will record.
//
// It goes through a real HTTP server rather than calling an unexported
// classifier so the table proves the behaviour a worker actually gets.
func TestErrorClassificationTable(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		class      actors.ErrorClass
		retryable  bool
		techStatus engine.TechStatus
	}{
		{"bad request", http.StatusBadRequest, actors.ClassActorRejectedInput, false, engine.StatusContractRejected},
		{"unprocessable", http.StatusUnprocessableEntity, actors.ClassActorRejectedInput, false, engine.StatusContractRejected},
		{"unauthorized", http.StatusUnauthorized, actors.ClassAuthOrPolicy, false, engine.StatusPolicyDenied},
		{"forbidden", http.StatusForbidden, actors.ClassAuthOrPolicy, false, engine.StatusPolicyDenied},
		{"not found", http.StatusNotFound, actors.ClassActorUnavailable, true, engine.StatusFailed},
		{"request timeout", http.StatusRequestTimeout, actors.ClassTimeout, true, engine.StatusTimedOut},
		{"conflict", http.StatusConflict, actors.ClassContract, false, engine.StatusContractRejected},
		{"too many requests", http.StatusTooManyRequests, actors.ClassRateLimited, true, engine.StatusFailed},
		{"internal error", http.StatusInternalServerError, actors.ClassExecution, false, engine.StatusFailed},
		{"bad gateway", http.StatusBadGateway, actors.ClassActorUnavailable, true, engine.StatusFailed},
		{"unavailable", http.StatusServiceUnavailable, actors.ClassActorUnavailable, true, engine.StatusFailed},
		{"gateway timeout", http.StatusGatewayTimeout, actors.ClassTimeout, true, engine.StatusTimedOut},
		{"teapot", http.StatusTeapot, actors.ClassActorRejectedInput, false, engine.StatusContractRejected},
		{"unexpected 2xx", http.StatusNoContent, actors.ClassContract, false, engine.StatusContractRejected},
		{"redirect body", http.StatusNotModified, actors.ClassContract, false, engine.StatusContractRejected},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			// One request only: this table is about classification, and a
			// retry loop would just repeat the same answer.
			_, err := newClient(t, actors.WithMaxRequests(1)).
				Invoke(context.Background(), actors.Endpoint{URL: server.URL}, testRequest())

			got, ok := actors.ClassOf(err)
			if !ok {
				t.Fatalf("HTTP %d produced an unclassified error: %v", tc.status, err)
			}
			if got != tc.class {
				t.Errorf("HTTP %d classified as %s, want %s", tc.status, got, tc.class)
			}
			if got.Retryable() != tc.retryable {
				t.Errorf("%s.Retryable() = %v, want %v", got, got.Retryable(), tc.retryable)
			}
			if status := actors.TechStatusFor(got); status != tc.techStatus {
				t.Errorf("TechStatusFor(%s) = %s, want %s", got, status, tc.techStatus)
			}
		})
	}
}

// §13.5's closing rule, stated as an assertion rather than left implicit in
// the table above: exactly four classes are retryable, and no mapping ever
// produces `succeeded`.
func TestOnlyExplicitlyRetryableClassesAreRetryable(t *testing.T) {
	wantRetryable := map[actors.ErrorClass]bool{
		actors.ClassRetryableTransport: true,
		actors.ClassRateLimited:        true,
		actors.ClassActorUnavailable:   true,
		actors.ClassTimeout:            true,
	}

	classes := actors.ErrorClasses()
	if len(classes) != 10 {
		t.Fatalf("ErrorClasses() has %d entries, want the 9 PRD §13.5 lists plus capacity_exhausted", len(classes))
	}
	for _, class := range classes {
		if !class.Valid() {
			t.Errorf("%s is not reported as valid", class)
		}
		if class.Retryable() != wantRetryable[class] {
			t.Errorf("%s.Retryable() = %v, want %v", class, class.Retryable(), wantRetryable[class])
		}
		if status := actors.TechStatusFor(class); status == engine.StatusSucceeded {
			t.Errorf("TechStatusFor(%s) = succeeded; a technical failure has no domain answer (PRD §3.4)", class)
		} else if !status.Valid() {
			t.Errorf("TechStatusFor(%s) = %q, which is not a PRD §3.4 status", class, status)
		}
	}

	if actors.ErrorClass("invented").Valid() {
		t.Error("an unrecognised class reported itself valid")
	}
	if got := actors.TechStatusFor("invented"); got != engine.StatusFailed {
		t.Errorf("TechStatusFor(unrecognised) = %s, want failed", got)
	}
}

// A bridge error body that declares class capacity_exhausted is
// authoritative over the status-code heuristic classifyStatus would
// otherwise produce — the whole point of task t8 (issue #48): a provider
// quota, a per-session limit, and ordinary rate_limited backpressure are not
// distinguishable by status code alone, only the bridge that talked to the
// provider knows which one happened.
func TestCapacityExhaustedIsBodyDeclared(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"declared on a 429", http.StatusTooManyRequests, `{"error":"quota exhausted","class":"capacity_exhausted"}`},
		{"declared on a 503", http.StatusServiceUnavailable, `{"error":"session limit reached","class":"capacity_exhausted"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			_, err := newClient(t, actors.WithMaxRequests(1)).
				Invoke(context.Background(), actors.Endpoint{URL: server.URL}, testRequest())

			got, ok := actors.ClassOf(err)
			if !ok {
				t.Fatalf("HTTP %d produced an unclassified error: %v", tc.status, err)
			}
			if got != actors.ClassCapacityExhausted {
				t.Errorf("class = %s, want capacity_exhausted", got)
			}
			if got.Retryable() {
				t.Error("capacity_exhausted.Retryable() = true, want false (retrying inside the attempt is issue #48's cascade)")
			}
			if status := actors.TechStatusFor(got); status != engine.StatusFailed {
				t.Errorf("TechStatusFor(capacity_exhausted) = %s, want failed (the class rides along on the attempt output, not on the tech status)", status)
			}
		})
	}
}

// A plain 429 with no declared class stays rate_limited: adding
// capacity_exhausted must not silently remap the existing status heuristic.
func TestRateLimitedStatusHeuristicUnchangedWithoutADeclaredClass(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, err := newClient(t, actors.WithMaxRequests(1)).
		Invoke(context.Background(), actors.Endpoint{URL: server.URL}, testRequest())

	got, ok := actors.ClassOf(err)
	if !ok {
		t.Fatalf("HTTP 429 produced an unclassified error: %v", err)
	}
	if got != actors.ClassRateLimited {
		t.Errorf("class = %s, want rate_limited (no body declared capacity_exhausted, so the status heuristic must stand)", got)
	}
}

// A body declaring some OTHER class is not honored: capacity_exhausted is
// the only class a bridge is trusted to self-declare, so classifyStatus's
// own reading of the status code stands for everything else.
func TestBodyDeclaredClassIsIgnoredExceptForCapacityExhausted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom","class":"auth_or_policy"}`))
	}))
	defer server.Close()

	_, err := newClient(t, actors.WithMaxRequests(1)).
		Invoke(context.Background(), actors.Endpoint{URL: server.URL}, testRequest())

	got, ok := actors.ClassOf(err)
	if !ok {
		t.Fatalf("HTTP 500 produced an unclassified error: %v", err)
	}
	if got != actors.ClassExecution {
		t.Errorf("class = %s, want execution (a bridge cannot relabel a 500 as auth_or_policy; only capacity_exhausted is self-declarable)", got)
	}
}

// RetryAfter is surfaced on the terminal InvocationError for
// capacity_exhausted exactly like every other class: the parsed delay is not
// discarded just because Retryable() keeps this class out of the in-attempt
// backoff sleep.
func TestCapacityExhaustedSurfacesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"quota exhausted","class":"capacity_exhausted"}`))
	}))
	defer server.Close()

	// maxRequests is 3 here specifically to prove the non-retryable class
	// stops at one request rather than spending the retry budget.
	_, err := newClient(t, actors.WithMaxRequests(3)).
		Invoke(context.Background(), actors.Endpoint{URL: server.URL}, testRequest())

	var invErr *actors.InvocationError
	if !errors.As(err, &invErr) {
		t.Fatalf("Invoke returned %T, want *actors.InvocationError", err)
	}
	if invErr.Class != actors.ClassCapacityExhausted {
		t.Fatalf("class = %s, want capacity_exhausted", invErr.Class)
	}
	if invErr.RetryAfter != 120*time.Second {
		t.Errorf("RetryAfter = %s, want 120s (parsed from the header and preserved even though this class never sleeps on it)", invErr.RetryAfter)
	}
	if invErr.Requests != 1 {
		t.Errorf("Requests = %d, want 1 — a non-retryable class must not spend the maxRequests budget", invErr.Requests)
	}
}
