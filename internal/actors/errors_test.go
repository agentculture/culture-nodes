package actors_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
	if len(classes) != 9 {
		t.Fatalf("ErrorClasses() has %d entries, want the 9 PRD §13.5 lists", len(classes))
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
