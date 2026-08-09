package loadtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// The stub runner service: an in-test HTTP server that speaks
// api/runner-protocol well enough to hold N operations in flight for as long
// as the measurement needs, and that counts every request it serves.
//
// It is NOT headspace and does not pretend to be. The conformance kit
// (tests/runnerconformance) is where protocol fidelity is proven; what this
// server needs to be faithful about is only the four things the load
// measurement depends on:
//
//  1. Authentication on every request, including status reads. An
//     unauthenticated status read would make the sampling numbers cheaper than
//     a real deployment's, since the bearer round trip is part of the cost.
//  2. 202 + Acceptance on dispatch, never a result. A stub that answered
//     synchronously would be measuring a code path the protocol forbids.
//  3. `accepted` then `running` until a configurable completion time, so
//     "in flight" is a state this test controls rather than a race it hopes
//     to win.
//  4. A terminal status carrying a real Result document, so the operations
//     actually commit through engine.CompleteAttempt at the end — proving the
//     hundred parked rows were genuine work items and not a parked-forever
//     artefact of the harness.
//
// Every request is timestamped, because "sampling load" is a rate, and a rate
// needs the times as well as the count.

const (
	stubSecretRef = "runner/load/execute-token"
	stubSecret    = "load-execute-token-not-the-ref"
	// stubRunnerRef is the registry name the workflow's `uses` declares.
	stubRunnerRef = "runner://headspace/docker@sha256:5555555555555555555555555555555555555555555555555555555555555555"
	stubDigest    = "sha256:57cd7c3a7a273101a6485ba99423ee568157882804b1124b4dd04266317710de"
	stubRunnerNm  = "headspace"
)

// stubRunner is the service's state. Its clock is real time: an operation
// becomes terminal once opDuration has elapsed since it was accepted, or
// immediately once finishAll is called.
type stubRunner struct {
	mu sync.Mutex

	// opDuration is how long an operation stays non-terminal. Zero means
	// "never finishes on its own" — the operation stays in flight until
	// finishAll.
	opDuration time.Duration
	// startupDelay is how long an operation reports `accepted` before it
	// reports `running`. Both are non-terminal; having both exercises the
	// sampler's non-terminal path in its two real shapes.
	startupDelay time.Duration

	accepted     map[string]stubOp
	dispatches   int
	redispatches int
	statusReads  []time.Time
	perOpReads   map[string]int
	unauthorized int
}

type stubOp struct {
	operation  runners.Operation
	acceptance runners.Acceptance
	acceptedAt time.Time
	forced     bool
}

func newStubRunner(opDuration, startupDelay time.Duration) *stubRunner {
	return &stubRunner{
		opDuration:   opDuration,
		startupDelay: startupDelay,
		accepted:     map[string]stubOp{},
		perOpReads:   map[string]int{},
	}
}

// start puts the stub behind a real TCP socket, so the worker process reaches
// it the way it would reach any runner service.
func (s *stubRunner) start() *httptest.Server { return httptest.NewServer(s.handler()) }

func (s *stubRunner) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(runners.AuthorizationHeader) != "Bearer "+stubSecret {
			s.mu.Lock()
			s.unauthorized++
			s.mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == runners.OperationsPath:
			s.execute(w, r)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, runners.OperationsPath+"/"):
			s.status(w, strings.TrimPrefix(r.URL.Path, runners.OperationsPath+"/"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (s *stubRunner) execute(w http.ResponseWriter, r *http.Request) {
	var op runners.Operation
	if err := json.NewDecoder(r.Body).Decode(&op); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	existing, known := s.accepted[op.OperationID]
	acceptance := runners.Acceptance{
		OperationID: op.OperationID,
		// No poll_after_seconds: the stub expresses no preference, so the
		// worker's own configured interval is the one under measurement.
		StatusRetentionSeconds: 86400,
	}
	if known {
		// Re-sending the same idempotency key returns the acceptance already
		// issued rather than starting the work again.
		s.redispatches++
		acceptance = existing.acceptance
	} else {
		s.dispatches++
		s.accepted[op.OperationID] = stubOp{operation: op, acceptance: acceptance, acceptedAt: time.Now()}
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(acceptance)
}

func (s *stubRunner) status(w http.ResponseWriter, operationID string) {
	now := time.Now()

	s.mu.Lock()
	s.statusReads = append(s.statusReads, now)
	s.perOpReads[operationID]++
	op, known := s.accepted[operationID]
	duration, startup := s.opDuration, s.startupDelay
	s.mu.Unlock()

	if !known {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	envelope := runners.OperationStatus{OperationID: operationID, State: runners.StateAccepted}
	elapsed := now.Sub(op.acceptedAt)
	switch {
	case op.forced || (duration > 0 && elapsed >= duration):
		result := stubResult(operationID, op)
		envelope.State = result.State
		envelope.Result = &result
	case elapsed >= startup:
		envelope.State = runners.StateRunning
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(envelope)
}

// stubResult is the terminal Result document a finished operation reports. It
// declares exactly one measured observation — the process exit status — which
// is the one the workflow's acceptance criterion reads.
func stubResult(operationID string, op stubOp) runners.Result {
	exit := 0
	finished := time.Now().UTC()
	return runners.Result{
		OperationID: operationID,
		State:       runners.StateCompleted,
		Exit:        &runners.Exit{Code: &exit},
		Timing: runners.Timing{
			StartedAt:  op.acceptedAt.UTC(),
			FinishedAt: finished,
			DurationMs: int(finished.Sub(op.acceptedAt).Milliseconds()),
		},
		Environment: runners.Environment{
			ImageDigest:  op.operation.Execution.ImageDigest,
			PolicyDigest: "sha256:" + strings.Repeat("c", 64),
		},
		Observations: runners.Observations{
			ExitStatus: runners.Observation{Measured: true, Complete: true, Method: "container_wait_status"},
		},
	}
}

// finishAll makes every operation accepted so far report a terminal result on
// its next status read. It is how a measurement ends: the in-flight fleet is
// released and the worker commits all of it through the ordinary path.
func (s *stubRunner) finishAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, op := range s.accepted {
		op.forced = true
		s.accepted[id] = op
	}
}

// counts returns the dispatch tally: how many distinct operations were
// accepted, how many re-sends of an already-accepted key arrived, and how many
// requests were refused for want of a credential.
func (s *stubRunner) counts() (dispatches, redispatches, unauthorized int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dispatches, s.redispatches, s.unauthorized
}

// readsBetween counts the status reads served in [from, to). The window
// matters: reads served while the fleet was still being dispatched are not
// steady-state sampling load and must not be averaged into it.
func (s *stubRunner) readsBetween(from, to time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, at := range s.statusReads {
		if !at.Before(from) && at.Before(to) {
			n++
		}
	}
	return n
}

// perOperationReads returns the smallest and largest number of status reads
// any single operation received, which is how "every operation is sampled on
// the same cadence" is checked rather than assumed.
func (s *stubRunner) perOperationReads() (minReads, maxReads int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	first := true
	for _, n := range s.perOpReads {
		if first {
			minReads, maxReads, first = n, n, false
			continue
		}
		if n < minReads {
			minReads = n
		}
		if n > maxReads {
			maxReads = n
		}
	}
	return minReads, maxReads
}
