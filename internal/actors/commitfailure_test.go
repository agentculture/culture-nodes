package actors_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The 2026-08-11 terminal-commit incident (issue #16), reproduced end to end
// against a real PostgreSQL and then against fakes.
//
// What was measured live: a deterministic error inside CompleteAttempt (an
// unregistered actor id violating ledger_records_origin_actor_id_fkey) rolled
// the §12.5 transaction back, and the callback ingest then held three things
// it had taken for an event it had not processed — the event-id claim, the
// per-attempt sequence mark, and the work item's resumed lease. Releasing
// only the first left the §13.4-mandated same-id/same-sequence redelivery
// permanently rejected as out-of-order, and left the work item
// leased-but-incomplete, whose lease expiry fed ReclaimExpired and a fresh
// billable dispatch every cycle.

// failOnceCompleter fails the first CompleteAttempt with a deterministic,
// non-refusal error — the incident's exact shape — and delegates every later
// call to the real engine, so the redelivery commits through the same code
// path a fixed deployment would.
type failOnceCompleter struct {
	inner actors.Completer
	err   error
	calls int
}

func (f *failOnceCompleter) CompleteAttempt(ctx context.Context, req engine.CompletionRequest) (engine.CompletionResult, error) {
	f.calls++
	if f.calls == 1 {
		return engine.CompletionResult{}, f.err
	}
	return f.inner.CompleteAttempt(ctx, req)
}

var _ actors.Completer = (*failOnceCompleter)(nil)

// lastSequence reads the per-attempt high-water mark the ratchet keeps.
func (f *asyncFixture) lastSequence() int64 {
	f.t.Helper()
	var sequence int64
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT last_sequence FROM actor_invocations WHERE attempt_id = $1`, f.attemptID).Scan(&sequence); err != nil {
		f.t.Fatalf("read last_sequence: %v", err)
	}
	return sequence
}

// countEvent counts one diagnostic type on the run's audit log.
func (f *asyncFixture) countEvent(eventType string) int {
	f.t.Helper()
	n := 0
	for _, t := range f.eventTypes() {
		if t == eventType {
			n++
		}
	}
	return n
}

// ownerName renders a nullable lease_owner for a failure message.
func ownerName(owner *string) string {
	if owner == nil {
		return "nobody"
	}
	return *owner
}

// expireLease backdates the work item's lease so ReclaimExpired treats it as
// expired now, without the test sleeping through a real lease.
func (f *asyncFixture) expireLease(workID string) {
	f.t.Helper()
	if _, err := f.store.Pool().Exec(f.ctx,
		`UPDATE work_items SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, workID); err != nil {
		f.t.Fatalf("expire lease: %v", err)
	}
}

// TestCallbackTerminalCommitFailureIsRecoverableOnRedelivery is issue #16's
// incident 1: a terminal event whose commit fails once must leave no trace
// that blocks its own redelivery, and must leave a recorded reason.
func TestCallbackTerminalCommitFailureIsRecoverableOnRedelivery(t *testing.T) {
	f := newAsyncFixture(t)

	failing := &failOnceCompleter{
		inner: f.engine,
		err: errors.New(`ERROR: insert or update on table "ledger_records" violates ` +
			`foreign key constraint "ledger_records_origin_actor_id_fkey" (SQLSTATE 23503)`),
	}
	f.deps.Engine = failing

	f.handle(actors.CallbackEvent{EventID: "ev-1", Sequence: 1, Kind: actors.EventAccepted})

	// ---- the failing terminal delivery ----
	terminal := completedEvent("ev-terminal", 2, "done")
	if _, err := actors.HandleCallback(f.ctx, f.deps, f.token, terminal); err == nil {
		t.Fatal("a terminal commit that failed reported success to the actor")
	}

	// The failure is written down. Live, nothing was: no log line, no event,
	// and a node the UI showed as forever "ready" with zero attempts.
	if got := f.countEvent(actors.TypeCallbackCommitFailed); got != 1 {
		t.Errorf("commit-failure events recorded = %d, want exactly 1; events were %v", got, f.eventTypes())
	}

	// The sequence mark records what was PROCESSED, not what was seen, so the
	// redelivery below is not pre-refused by the ratchet.
	if got := f.lastSequence(); got != 1 {
		t.Errorf("last_sequence after a failed commit = %d, want 1 (the mark of the last processed event)", got)
	}

	// And the work item is parked again rather than leased-but-incomplete —
	// the lease expiry of a resumed-then-abandoned item is the motor of the
	// ~90s billable re-dispatch loop.
	state, owner := f.workItemState(f.claimed.ID)
	if state != storepg.WaitingWorkState || owner != nil {
		t.Errorf("work item after a failed commit = %q owned by %s, want %q with no owner",
			state, ownerName(owner), storepg.WaitingWorkState)
	}
	f.expireLease(f.claimed.ID)
	if _, err := f.store.ReclaimExpired(f.ctx); err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if state, _ := f.workItemState(f.claimed.ID); state != storepg.WaitingWorkState {
		t.Errorf("after a lease sweep the work item is %q, want still %q: a failed commit must not feed re-dispatch",
			state, storepg.WaitingWorkState)
	}

	// ---- §13.4's redelivery, same event id and same sequence ----
	redelivered := f.handle(completedEvent("ev-terminal", 2, "done"))
	if redelivered.Disposition != actors.DispositionCommitted {
		t.Fatalf("redelivery disposition = %s (%s), want committed", redelivered.Disposition, redelivered.Diagnostic)
	}
	if failing.calls != 2 {
		t.Errorf("engine CompleteAttempt calls = %d, want 2 (the failure and the redelivery)", failing.calls)
	}

	run, err := f.engine.Store().Run(f.ctx, f.run.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.State != engine.RunCompleted {
		t.Errorf("run state = %s, want completed after the redelivery committed", run.State)
	}
	if state, _ := f.workItemState(f.claimed.ID); state != "completed" {
		t.Errorf("work item state = %q, want completed", state)
	}
	var invocationState string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT state FROM actor_invocations WHERE attempt_id = $1`, f.attemptID).Scan(&invocationState); err != nil {
		t.Fatalf("read invocation: %v", err)
	}
	if invocationState != actors.InvocationCompleted {
		t.Errorf("invocation state = %q, want completed", invocationState)
	}
}

// compensationStore is a CallbackStore that remembers, in order, every call
// HandleCallback makes to it, and keeps a real sequence mark and claim set so
// the compensations can be asserted for effect and not only for occurrence.
// It exists because the ORDER matters — the event-id claim is the gate, and
// releasing it before the mark and the lease are back would let a redelivery
// in mid-repair — and order is not observable against a real database.
type compensationStore struct {
	inv    actors.PendingInvocation
	mark   int64
	claims map[string]bool
	leased bool

	calls   []string
	events  []recordedEvent
	emitted []actors.EmitSignalInput

	// superseded counts the late-report corrections appended (task t11), and
	// supersedeErr makes that append fail so the compensation order can be
	// asserted for a failure at this stage too.
	superseded   int
	supersedeErr error
}

type recordedEvent struct {
	eventType string
	data      map[string]any
}

func newCompensationStore(inv actors.PendingInvocation) *compensationStore {
	return &compensationStore{inv: inv, mark: inv.LastSequence, claims: map[string]bool{}}
}

func (s *compensationStore) note(call string) { s.calls = append(s.calls, call) }

func (s *compensationStore) Invocation(context.Context, string) (actors.PendingInvocation, error) {
	inv := s.inv
	inv.LastSequence = s.mark
	return inv, nil
}

func (s *compensationStore) ClaimCallbackEvent(_ context.Context, _ actors.PendingInvocation, eventID string) (bool, error) {
	if s.claims[eventID] {
		return false, nil
	}
	s.claims[eventID] = true
	s.note("claim")
	return true, nil
}

func (s *compensationStore) ReleaseCallbackEvent(_ context.Context, _ actors.PendingInvocation, eventID string) error {
	delete(s.claims, eventID)
	s.note("release_claim")
	return nil
}

func (s *compensationStore) AdvanceCallbackSequence(_ context.Context, _ string, sequence int64) (bool, error) {
	if sequence <= s.mark {
		return false, nil
	}
	s.mark = sequence
	s.note("advance_sequence")
	return true, nil
}

func (s *compensationStore) RollbackCallbackSequence(_ context.Context, _ string, sequence, previous int64) error {
	if s.mark != sequence {
		return nil
	}
	s.mark = previous
	s.note("rollback_sequence")
	return nil
}

func (s *compensationStore) TouchInvocation(context.Context, string, string, time.Time) error {
	s.note("touch")
	return nil
}

func (s *compensationStore) EmitSignalEvent(_ context.Context, _ actors.PendingInvocation, in actors.EmitSignalInput) (actors.EmitSignalResult, error) {
	s.note("emit_signal")
	s.emitted = append(s.emitted, in)
	return actors.EmitSignalResult{EventID: "evt_" + in.Name}, nil
}

func (s *compensationStore) CloseInvocation(context.Context, string, string) error {
	s.note("close")
	return nil
}

func (s *compensationStore) RecordSupersedingAttempt(
	_ context.Context, _ actors.PendingInvocation, _ engine.CompletionRequest,
) (actors.SupersedingAttempt, error) {
	s.note("record_superseding_attempt")
	if s.supersedeErr != nil {
		return actors.SupersedingAttempt{}, s.supersedeErr
	}
	s.superseded++
	return actors.SupersedingAttempt{
		AttemptID:  fmt.Sprintf("att_superseding_%d", s.superseded),
		Number:     s.superseded + 1,
		Supersedes: "att_superseded",
	}, nil
}

func (s *compensationStore) ResumeWaitingWork(context.Context, actors.PendingInvocation, time.Duration) error {
	s.leased = true
	s.note("resume")
	return nil
}

func (s *compensationStore) ReparkResumedWork(context.Context, actors.PendingInvocation) error {
	s.leased = false
	s.note("repark")
	return nil
}

func (s *compensationStore) AppendRunEvent(_ context.Context, _, _, eventType string, data map[string]any) error {
	s.events = append(s.events, recordedEvent{eventType: eventType, data: data})
	return nil
}

var _ actors.CallbackStore = (*compensationStore)(nil)

// alwaysFailingCompleter is a §12.5 transaction that always rolls back for an
// infrastructure reason — not a §13.4 refusal, which is a different outcome.
type alwaysFailingCompleter struct{ err error }

func (c *alwaysFailingCompleter) CompleteAttempt(context.Context, engine.CompletionRequest) (engine.CompletionResult, error) {
	return engine.CompletionResult{}, c.err
}

func mintedToken(t *testing.T, attemptID string) (*actors.TokenSigner, string) {
	t.Helper()
	signer, err := actors.NewTokenSigner([]byte(testSecret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}
	token, err := signer.Mint(attemptID)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return signer, token
}

// TestTerminalCommitFailureCompensatesInReverseOrder pins the mechanism the
// PostgreSQL regression above proves the outcome of: the lease goes back
// first, then the mark, then the claim — and the reason is recorded.
func TestTerminalCommitFailureCompensatesInReverseOrder(t *testing.T) {
	attemptID := "att_compensation"
	signer, token := mintedToken(t, attemptID)
	store := newCompensationStore(actors.PendingInvocation{
		AttemptID: attemptID, NamespaceID: "ns", RunID: "run_1", NodeRunID: "nr_1",
		WorkID: "work_1", WorkerID: "worker_1", FencingToken: 7, Attempt: 1, LastSequence: 4,
	})
	cause := errors.New("ledger_records_origin_actor_id_fkey")
	deps := actors.CallbackDeps{Store: store, Engine: &alwaysFailingCompleter{err: cause}, Signer: signer}

	_, err := actors.HandleCallback(context.Background(), deps, token, actors.CallbackEvent{
		EventID: "ev-terminal", Sequence: 5, Kind: actors.EventCompleted, Payload: completedEventPayload(t),
	})
	if !errors.Is(err, cause) {
		t.Fatalf("HandleCallback err = %v, want the engine's own cause", err)
	}

	want := []string{"claim", "advance_sequence", "resume", "repark", "rollback_sequence", "release_claim"}
	if !equalStrings(store.calls, want) {
		t.Errorf("store calls = %v, want %v", store.calls, want)
	}
	if store.mark != 4 {
		t.Errorf("sequence mark = %d, want the pre-delivery 4: the event was not processed", store.mark)
	}
	if store.claims["ev-terminal"] {
		t.Error("the event-id claim was kept for an event that was not processed")
	}
	if store.leased {
		t.Error("the work item was left leased to a completion that never committed")
	}

	var failures int
	for _, ev := range store.events {
		if ev.eventType != actors.TypeCallbackCommitFailed {
			continue
		}
		failures++
		if ev.data["stage"] != actors.StageComplete {
			t.Errorf("commit-failure stage = %v, want %q", ev.data["stage"], actors.StageComplete)
		}
		if got, _ := ev.data["error"].(string); got != cause.Error() {
			t.Errorf("commit-failure error = %q, want %q", got, cause.Error())
		}
		if ev.data["attempt_id"] != attemptID {
			t.Errorf("commit-failure attempt_id = %v, want %q", ev.data["attempt_id"], attemptID)
		}
	}
	if failures != 1 {
		t.Errorf("commit-failure events = %d, want exactly 1 (%v)", failures, store.events)
	}
}

// A non-terminal event that fails after the ratchet moved is the same trap on
// a cheaper path: the compensation is not terminal-specific.
func TestNonTerminalFailureAlsoReturnsTheSequenceMark(t *testing.T) {
	attemptID := "att_touch_failure"
	signer, token := mintedToken(t, attemptID)
	store := &touchFailingStore{compensationStore: newCompensationStore(actors.PendingInvocation{
		AttemptID: attemptID, NamespaceID: "ns", RunID: "run_2", NodeRunID: "nr_2",
		WorkID: "work_2", WorkerID: "worker_2", FencingToken: 1, Attempt: 1, LastSequence: 2,
	})}
	deps := actors.CallbackDeps{Store: store, Engine: &alwaysFailingCompleter{}, Signer: signer}

	_, err := actors.HandleCallback(context.Background(), deps, token, actors.CallbackEvent{
		EventID: "ev-progress", Sequence: 3, Kind: actors.EventProgress,
	})
	if err == nil {
		t.Fatal("a failed TouchInvocation reported success")
	}
	if store.mark != 2 {
		t.Errorf("sequence mark = %d, want the pre-delivery 2", store.mark)
	}
	if store.claims["ev-progress"] {
		t.Error("the event-id claim was kept for an event that was not processed")
	}
}

type touchFailingStore struct{ *compensationStore }

func (s *touchFailingStore) TouchInvocation(context.Context, string, string, time.Time) error {
	return errors.New("connection reset")
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A §13.4 refusal is not a failure — the sequence mark and the event-id claim
// are kept, because the event WAS processed (into a late diagnostic) — but the
// work item was still re-leased for a completion that never happened, so it is
// parked all the same. Left leased it would expire into a re-dispatch of a run
// that is already terminal.
func TestRefusedTerminalCommitStillReparksTheWorkItem(t *testing.T) {
	attemptID := "att_refused"
	signer, token := mintedToken(t, attemptID)
	store := newCompensationStore(actors.PendingInvocation{
		AttemptID: attemptID, NamespaceID: "ns", RunID: "run_3", NodeRunID: "nr_3",
		WorkID: "work_3", WorkerID: "worker_3", FencingToken: 2, Attempt: 1, LastSequence: 1,
	})
	deps := actors.CallbackDeps{
		Store:  store,
		Engine: &alwaysFailingCompleter{err: engine.ErrTerminalRun},
		Signer: signer,
	}

	result, err := actors.HandleCallback(context.Background(), deps, token, actors.CallbackEvent{
		EventID: "ev-late", Sequence: 2, Kind: actors.EventCompleted, Payload: completedEventPayload(t),
	})
	if err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if result.Disposition != actors.DispositionLate {
		t.Fatalf("disposition = %s, want late", result.Disposition)
	}

	// record_superseding_attempt sits between the repark and the close (task
	// t11): the correction is durable state, so it is written BEFORE the
	// bookkeeping that retires the invocation. A close that then fails leaves
	// the record already there, and the redelivery it invites finds it
	// through migrations/0028's unique index instead of appending a twin.
	want := []string{"claim", "advance_sequence", "resume", "repark", "record_superseding_attempt", "close"}
	if !equalStrings(store.calls, want) {
		t.Errorf("store calls = %v, want %v", store.calls, want)
	}
	if store.leased {
		t.Error("a refused completion left the work item leased")
	}
	if store.mark != 2 || !store.claims["ev-late"] {
		t.Errorf("mark = %d, claim held = %v; want 2 and true: a refusal processed the event",
			store.mark, store.claims["ev-late"])
	}
}
