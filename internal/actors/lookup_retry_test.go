package actors_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
)

// Fast, deterministic unit coverage for HandleCallback's tolerance of a
// callback that arrives before Store.Invocation can find the row a
// dispatch's park write (postgres.StartAsyncWait) is still committing — see
// callback.go's lookupInvocation doc, and
// docs/deliveries/2026-08-08-culture-nodes-app-design.md's "run.output
// observed null for the live smoke's end-node binding". The slower,
// Postgres-backed proof that this closes the race end to end is
// tests/e2e/asyncoutput_test.go; this file isolates the retry mechanism
// itself against fakes so it can be asserted without a real timing race or a
// database.

// fakeCallbackStore backs one HandleCallback call. Every method but
// Invocation is a fixed, successful no-op: this test is only about how many
// times, and with what outcome, Invocation is retried — the rest of the
// pipeline (dedup, sequencing, resume, close) is already covered against a
// real PostgreSQL by callback_test.go's asyncFixture.
type fakeCallbackStore struct {
	// misses is how many leading calls to Invocation report ErrUnknownAttempt
	// before (optionally) succeeding.
	misses int32
	inv    actors.PendingInvocation

	calls int32
}

func (f *fakeCallbackStore) Invocation(_ context.Context, _ string) (actors.PendingInvocation, error) {
	n := atomic.AddInt32(&f.calls, 1)
	if n <= f.misses {
		return actors.PendingInvocation{}, actors.ErrUnknownAttempt
	}
	return f.inv, nil
}

func (f *fakeCallbackStore) callCount() int32 { return atomic.LoadInt32(&f.calls) }

func (f *fakeCallbackStore) ClaimCallbackEvent(_ context.Context, _ actors.PendingInvocation, _ string) (bool, error) {
	return true, nil
}

func (f *fakeCallbackStore) ReleaseCallbackEvent(_ context.Context, _ actors.PendingInvocation, _ string) error {
	return nil
}

func (f *fakeCallbackStore) AdvanceCallbackSequence(_ context.Context, _ string, _ int64) (bool, error) {
	return true, nil
}

func (f *fakeCallbackStore) RollbackCallbackSequence(_ context.Context, _ string, _, _ int64) error {
	return nil
}

func (f *fakeCallbackStore) TouchInvocation(_ context.Context, _, _ string, _ time.Time) error {
	return nil
}

func (f *fakeCallbackStore) CloseInvocation(_ context.Context, _, _ string) error { return nil }

func (f *fakeCallbackStore) EmitSignalEvent(_ context.Context, _ actors.PendingInvocation, _ actors.EmitSignalInput) (actors.EmitSignalResult, error) {
	return actors.EmitSignalResult{}, nil
}

func (f *fakeCallbackStore) ResumeWaitingWork(_ context.Context, _ actors.PendingInvocation, _ time.Duration) error {
	return nil
}

func (f *fakeCallbackStore) ReparkResumedWork(_ context.Context, _ actors.PendingInvocation) error {
	return nil
}

func (f *fakeCallbackStore) AppendRunEvent(_ context.Context, _, _, _ string, _ map[string]any) error {
	return nil
}

var _ actors.CallbackStore = (*fakeCallbackStore)(nil)

// fakeCompleter always reports the same completion, successfully — this
// test does not exercise engine.CompleteAttempt's own behaviour.
type fakeCompleter struct{ result engine.CompletionResult }

func (f *fakeCompleter) CompleteAttempt(context.Context, engine.CompletionRequest) (engine.CompletionResult, error) {
	return f.result, nil
}

var _ actors.Completer = (*fakeCompleter)(nil)

func completedEventPayload(t *testing.T) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(actors.CompletedPayload{
		Outcome: "completed",
		Output:  json.RawMessage(`{"ok":true}`),
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return payload
}

// TestHandleCallbackRetriesAnInvocationLookupRace proves a callback that
// loses the race against the dispatch's own park write still commits: the
// first few lookups report ErrUnknownAttempt (simulating the row not being
// visible yet), and HandleCallback is expected to retry rather than refuse
// outright.
func TestHandleCallbackRetriesAnInvocationLookupRace(t *testing.T) {
	signer, err := actors.NewTokenSigner([]byte(testSecret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	attemptID := "att_race"
	token, err := signer.Mint(attemptID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	store := &fakeCallbackStore{
		misses: 3,
		inv: actors.PendingInvocation{
			AttemptID: attemptID, NamespaceID: "ns", RunID: "run_1", NodeRunID: "nr_1",
			WorkID: "work_1", WorkerID: "worker_1", FencingToken: 1, Attempt: 1,
		},
	}
	deps := actors.CallbackDeps{
		Store:                   store,
		Engine:                  &fakeCompleter{result: engine.CompletionResult{RunID: "run_1"}},
		Signer:                  signer,
		InvocationLookupRetries: 5,
		InvocationLookupDelay:   time.Millisecond,
	}

	result, err := actors.HandleCallback(context.Background(), deps, token, actors.CallbackEvent{
		EventID: "ev-1", Sequence: 1, Kind: actors.EventCompleted, Payload: completedEventPayload(t),
	})
	if err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}
	if got := store.callCount(); got != 4 {
		t.Errorf("Store.Invocation was called %d times, want 4 (3 misses + the hit)", got)
	}
}

// TestHandleCallbackReportsAGenuinelyUnknownAttemptAfterRetrying proves the
// retry is bounded and does not weaken ErrUnknownAttempt's meaning: an
// attempt that never shows up still, eventually, reports exactly that,
// after exhausting the configured budget — not one attempt more.
func TestHandleCallbackReportsAGenuinelyUnknownAttemptAfterRetrying(t *testing.T) {
	signer, err := actors.NewTokenSigner([]byte(testSecret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	token, err := signer.Mint("att_never_existed")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	store := &fakeCallbackStore{misses: 1000} // never resolves
	deps := actors.CallbackDeps{
		Store:                   store,
		Engine:                  &fakeCompleter{},
		Signer:                  signer,
		InvocationLookupRetries: 3,
		InvocationLookupDelay:   time.Millisecond,
	}

	_, err = actors.HandleCallback(context.Background(), deps, token, actors.CallbackEvent{
		EventID: "ev-1", Sequence: 1, Kind: actors.EventCompleted, Payload: completedEventPayload(t),
	})
	if !errors.Is(err, actors.ErrUnknownAttempt) {
		t.Fatalf("HandleCallback err = %v, want ErrUnknownAttempt", err)
	}
	if got := store.callCount(); got != 3 {
		t.Errorf("Store.Invocation was called %d times, want exactly the configured 3", got)
	}
}

// TestHandleCallbackDefaultInvocationLookupRetriesAreUsedWhenUnset proves a
// zero-value CallbackDeps (what every existing caller in this repo already
// constructs) still gets the production retry budget, not a bypass.
func TestHandleCallbackDefaultInvocationLookupRetriesAreUsedWhenUnset(t *testing.T) {
	signer, err := actors.NewTokenSigner([]byte(testSecret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	attemptID := "att_default_budget"
	token, err := signer.Mint(attemptID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	store := &fakeCallbackStore{
		misses: actors.DefaultInvocationLookupRetries - 1,
		inv:    actors.PendingInvocation{AttemptID: attemptID, NamespaceID: "ns", RunID: "run_2", NodeRunID: "nr_2", WorkID: "work_2", WorkerID: "worker_2", FencingToken: 1, Attempt: 1},
	}
	// InvocationLookupRetries/Delay left unset: this is the production default
	// path every real CallbackDeps caller (cmd/nodes, internal/api) takes.
	deps := actors.CallbackDeps{
		Store:  store,
		Engine: &fakeCompleter{result: engine.CompletionResult{RunID: "run_2"}},
		Signer: signer,
	}

	result, err := actors.HandleCallback(context.Background(), deps, token, actors.CallbackEvent{
		EventID: "ev-1", Sequence: 1, Kind: actors.EventCompleted, Payload: completedEventPayload(t),
	})
	if err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}
	if got := store.callCount(); got != int32(actors.DefaultInvocationLookupRetries) {
		t.Errorf("Store.Invocation was called %d times, want the default budget of %d",
			got, actors.DefaultInvocationLookupRetries)
	}
}
