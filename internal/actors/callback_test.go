package actors_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Callback-ingest tests against a real PostgreSQL.
//
// Everything below goes through the real path: a real run created by the
// engine, a real work item claimed through Store.ClaimWork (so the fencing
// token and attempt number are the ones the claiming code assigns, not
// invented ones), a real StartAsyncWait that parks it, and a real
// HandleCallback. A fixture that hand-wrote any of those rows would prove the
// ingest works against a fixture rather than against the runtime.

const testLease = 2 * time.Minute

type asyncFixture struct {
	t         *testing.T
	ctx       context.Context
	store     *storepg.Store
	ns        storepg.Namespace
	engine    *engine.Engine
	callbacks *storepg.CallbackStore
	signer    *actors.TokenSigner
	deps      actors.CallbackDeps
	cw        *compiler.CompiledWorkflow

	workerID  string
	run       engine.Run
	nodeRunID string
	claimed   storepg.ClaimedWork
	attemptID string
	token     string
	// actorID is the resolved actors-table row id the dispatch attributed
	// this invocation to (migration 0015), empty when the fixture does not
	// need per-actor attribution. It is set BEFORE park() so it reaches the
	// actor_invocations row the way a real dispatch writes it, rather than
	// being patched in afterwards.
	actorID string
}

func newAsyncFixture(t *testing.T) *asyncFixture {
	t.Helper()
	return newAsyncFixtureWith(t, false)
}

// newAsyncFixtureForActor is newAsyncFixture with the dispatch attributed to
// a freshly registered actor (f.actorID), for the tests that read per-actor
// statistics back. The actor is registered before the park so the
// attribution reaches actor_invocations.actor_id the way a real dispatch
// writes it (migration 0015), not by patching the row afterwards.
func newAsyncFixtureForActor(t *testing.T) *asyncFixture {
	t.Helper()
	return newAsyncFixtureWith(t, true)
}

func newAsyncFixtureWith(t *testing.T, withActor bool) *asyncFixture {
	return newAsyncFixtureWithWorkflow(t, withActor, "async.workflow.yaml")
}

func compileFixture(t *testing.T, name string) *compiler.CompiledWorkflow {
	t.Helper()
	path := filepath.Join("testdata", name)
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	cw, diags, err := compiler.Compile(source, compiler.FormatForPath(path))
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
	}
	for _, d := range diags {
		if d.Level == compiler.LevelError {
			t.Fatalf("compile %s: %s at %s: %s", path, d.Code, d.Path, d.Message)
		}
	}
	return cw
}

func (f *asyncFixture) readyNodeRun(runID string) string {
	f.t.Helper()
	var id string
	err := f.store.Pool().QueryRow(f.ctx,
		`SELECT id FROM node_runs WHERE run_id = $1 AND status = 'ready' ORDER BY created_at DESC, id DESC LIMIT 1`,
		runID).Scan(&id)
	if err != nil {
		f.t.Fatalf("no ready node run for run %s: %v", runID, err)
	}
	return id
}

// claim wins the work item for nodeRunID through the real claiming path,
// handing back anything else it happened to claim within the fixture's
// namespace (a parallel test's item must not be left leased).
func (f *asyncFixture) claim(workerID, nodeRunID string) storepg.ClaimedWork {
	f.t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		claimed, err := f.store.ClaimWork(f.ctx, f.ns.ID, workerID, testLease, 20)
		if err != nil {
			f.t.Fatalf("ClaimWork: %v", err)
		}
		var found *storepg.ClaimedWork
		for i := range claimed {
			if claimed[i].NodeRunID == nodeRunID && found == nil {
				found = &claimed[i]
				continue
			}
			f.release(claimed[i].ID)
		}
		if found != nil {
			return *found
		}
		time.Sleep(25 * time.Millisecond)
	}
	f.t.Fatalf("no work item became claimable for node run %s", nodeRunID)
	return storepg.ClaimedWork{}
}

func (f *asyncFixture) release(workID string) {
	f.t.Helper()
	if _, err := f.store.Pool().Exec(f.ctx,
		`UPDATE work_items SET state = 'ready', lease_owner = NULL, lease_expires_at = NULL WHERE id = $1`,
		workID); err != nil {
		f.t.Fatalf("release work item: %v", err)
	}
}

// park is the §12.6 transition the worker performs when an actor answers 202.
func (f *asyncFixture) park() {
	f.t.Helper()
	err := f.store.StartAsyncWait(f.ctx, storepg.StartAsyncWaitInput{
		WorkID:                f.claimed.ID,
		WorkerID:              f.workerID,
		FencingToken:          f.claimed.FencingToken,
		Attempt:               int(f.claimed.Attempt),
		NamespaceID:           f.ns.ID,
		RunID:                 f.run.ID,
		NodeRunID:             f.nodeRunID,
		NodeID:                "work",
		AttemptID:             f.attemptID,
		ActorID:               f.actorID,
		ActorRef:              "actor://company/long-runner@sha256:aaaaaa",
		InvocationID:          "external_" + f.attemptID,
		HeartbeatAfterSeconds: 30,
		SupportsCancellation:  true,
		Deadline:              time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		f.t.Fatalf("StartAsyncWait: %v", err)
	}
	token, err := f.signer.Mint(f.attemptID)
	if err != nil {
		f.t.Fatalf("Mint: %v", err)
	}
	f.token = token
}

func (f *asyncFixture) handle(ev actors.CallbackEvent) actors.CallbackResult {
	f.t.Helper()
	result, err := actors.HandleCallback(f.ctx, f.deps, f.token, ev)
	if err != nil {
		f.t.Fatalf("HandleCallback(%s seq %d): %v", ev.Kind, ev.Sequence, err)
	}
	return result
}

func (f *asyncFixture) workItemState(workID string) (state string, leaseOwner *string) {
	f.t.Helper()
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT state, lease_owner FROM work_items WHERE id = $1`, workID).Scan(&state, &leaseOwner); err != nil {
		f.t.Fatalf("read work item %s: %v", workID, err)
	}
	return state, leaseOwner
}

func (f *asyncFixture) eventTypes() []string {
	f.t.Helper()
	rows, err := f.store.Pool().Query(f.ctx,
		`SELECT event_type FROM events WHERE aggregate_id = $1 ORDER BY sequence`, f.run.ID)
	if err != nil {
		f.t.Fatalf("read events: %v", err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			f.t.Fatalf("scan event: %v", err)
		}
		types = append(types, t)
	}
	return types
}

func (f *asyncFixture) hasEvent(eventType string) bool {
	f.t.Helper()
	for _, t := range f.eventTypes() {
		if t == eventType {
			return true
		}
	}
	return false
}

func completedEvent(id string, sequence int64, summary string) actors.CallbackEvent {
	payload, _ := json.Marshal(actors.CompletedPayload{
		Outcome: "completed",
		Output:  json.RawMessage(fmt.Sprintf(`{"summary":%q}`, summary)),
	})
	return actors.CallbackEvent{EventID: id, Sequence: sequence, Kind: actors.EventCompleted, Payload: payload}
}

// Parking an invocation must release worker capacity (§12.6): the work item
// leaves 'leased' with no owner, so nothing reclaims it and no goroutine
// waits on it — while keeping the fencing token the dispatch held.
func TestStartAsyncWaitReleasesTheClaimWithoutCompletingIt(t *testing.T) {
	f := newAsyncFixture(t)

	state, owner := f.workItemState(f.claimed.ID)
	if state != storepg.WaitingWorkState {
		t.Errorf("work item state = %q, want %q", state, storepg.WaitingWorkState)
	}
	if owner != nil {
		t.Errorf("work item lease_owner = %q, want NULL: the worker released capacity", *owner)
	}

	var nodeRunStatus string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT status FROM node_runs WHERE id = $1`, f.nodeRunID).Scan(&nodeRunStatus); err != nil {
		t.Fatalf("read node run: %v", err)
	}
	if nodeRunStatus != string(engine.NodeRunWaitingExternal) {
		t.Errorf("node run status = %q, want waiting_external", nodeRunStatus)
	}

	// A parked item is invisible to a claimant: this is what "releases worker
	// capacity" has to mean operationally.
	claimed, err := f.store.ClaimWork(f.ctx, f.ns.ID, "other-worker", testLease, 10)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}
	for i := range claimed {
		if claimed[i].ID == f.claimed.ID {
			t.Error("a parked work item was claimable")
		}
		f.release(claimed[i].ID)
	}

	// And it survives a lease sweep, because it holds no lease to expire.
	if _, err := f.store.ReclaimExpired(f.ctx); err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}
	if state, _ := f.workItemState(f.claimed.ID); state != storepg.WaitingWorkState {
		t.Errorf("after ReclaimExpired the work item is %q, want still waiting", state)
	}

	if !f.hasEvent(storepg.TypeAttemptWaitingExternal) {
		t.Errorf("no waiting-external event was recorded; events were %v", f.eventTypes())
	}
	// A deadline timer is the only thing that will ever unstick an actor that
	// never reports, so parking without one would be a silent hang.
	var timers int
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT count(*)::int FROM timers WHERE node_run_id = $1 AND timer_kind = 'deadline' AND status = 'pending'`,
		f.nodeRunID).Scan(&timers); err != nil {
		t.Fatalf("count timers: %v", err)
	}
	if timers != 1 {
		t.Errorf("pending deadline timers = %d, want 1", timers)
	}
}

// A terminal callback commits through the engine's own fenced transaction,
// under the fencing token the dispatch held.
func TestCallbackCompletedCommitsTheRun(t *testing.T) {
	f := newAsyncFixture(t)

	accepted := f.handle(actors.CallbackEvent{EventID: "ev-1", Sequence: 1, Kind: actors.EventAccepted})
	if accepted.Disposition != actors.DispositionRecorded {
		t.Errorf("accepted disposition = %s, want recorded", accepted.Disposition)
	}
	if accepted.Disposition.CommittedState() {
		t.Error("a non-terminal event reported that it committed state")
	}

	result := f.handle(completedEvent("ev-2", 2, "done"))
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("completed disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}
	if result.Completion == nil {
		t.Fatal("a committed callback returned no completion result")
	}
	if result.Completion.RunState != engine.RunCompleted {
		t.Errorf("run state = %s, want completed", result.Completion.RunState)
	}
	if result.Completion.Outcome != "completed" {
		t.Errorf("outcome = %q, want completed", result.Completion.Outcome)
	}

	run, err := f.engine.Store().Run(f.ctx, f.run.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.State != engine.RunCompleted {
		t.Errorf("committed run state = %s, want completed", run.State)
	}
	if !bytes.Contains(run.Output, []byte(`"done"`)) {
		t.Errorf("run output = %s, want the actor's summary", run.Output)
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

// A `completed` callback's §13.2 Usage block is not just decoded off the
// wire and discarded: it lands on the attempt row this callback commits,
// the async twin of what a synchronous InvocationResult's Usage does at
// internal/worker/dispatch.go's completeFromResult (task t1).
func TestCallbackCompletedPersistsUsage(t *testing.T) {
	f := newAsyncFixture(t)

	cost := 0.03
	currency := "USD"
	payload, _ := json.Marshal(actors.CompletedPayload{
		Outcome: "completed",
		Output:  json.RawMessage(`{"summary":"done"}`),
		Usage: &actors.Usage{
			InputTokens:  55,
			OutputTokens: 130,
			Cost:         &cost,
			Currency:     &currency,
		},
	})

	result := f.handle(actors.CallbackEvent{EventID: "ev-usage", Sequence: 1, Kind: actors.EventCompleted, Payload: payload})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	usage := attempts[0].Usage
	if usage == nil {
		t.Fatal("Usage = nil, want the reported §13.2 block persisted on the committed attempt")
	}
	if usage.InputTokens != 55 || usage.OutputTokens != 130 {
		t.Errorf("tokens = %d/%d, want 55/130", usage.InputTokens, usage.OutputTokens)
	}
	if usage.Cost == nil || *usage.Cost != cost {
		t.Errorf("cost = %v, want %v", usage.Cost, cost)
	}
	if usage.Currency == nil || *usage.Currency != currency {
		t.Errorf("currency = %v, want %v", usage.Currency, currency)
	}
}

// A `completed` callback that carries no Usage block at all leaves the
// attempt's usage nil, exactly like completedEvent's plain fixture already
// exercises for every other callback test — this makes the "no fabricated
// zero" claim explicit rather than incidental.
func TestCallbackCompletedWithoutUsageStaysNil(t *testing.T) {
	f := newAsyncFixture(t)

	result := f.handle(completedEvent("ev-no-usage", 1, "done"))
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts[0].Usage != nil {
		t.Errorf("Usage = %+v, want nil for a completed event that reported none", attempts[0].Usage)
	}
}

// A `failed` callback's optional §13.2 Usage block persists on the failed
// attempt row exactly as a completed one's does: a session that burned real
// tokens before failing is billable work, and ADR 0008's amendment to §13.2
// exists so that burn is counted instead of dropped at the protocol boundary.
func TestCallbackFailedPersistsUsage(t *testing.T) {
	f := newAsyncFixture(t)

	cost := 0.07
	currency := "USD"
	payload, _ := json.Marshal(actors.FailedPayload{
		Class:   actors.ClassExecution,
		Message: "the session crashed after doing real work",
		Usage: &actors.Usage{
			InputTokens:  210,
			OutputTokens: 89,
			Cost:         &cost,
			Currency:     &currency,
		},
	})

	result := f.handle(actors.CallbackEvent{
		EventID: "ev-failed-usage", Sequence: 1, Kind: actors.EventFailed, Payload: payload,
	})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("failed disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	usage := attempts[0].Usage
	if usage == nil {
		t.Fatal("Usage = nil, want the reported §13.2 block persisted on the failed attempt")
	}
	if usage.InputTokens != 210 || usage.OutputTokens != 89 {
		t.Errorf("tokens = %d/%d, want 210/89", usage.InputTokens, usage.OutputTokens)
	}
	if usage.Cost == nil || *usage.Cost != cost {
		t.Errorf("cost = %v, want %v", usage.Cost, cost)
	}
	if usage.Currency == nil || *usage.Currency != currency {
		t.Errorf("currency = %v, want %v", usage.Currency, currency)
	}
}

// A `failed` callback that reports no usage leaves the attempt's usage NULL —
// never a fabricated zero block (the migration-0012 stance): a crash that
// reported nothing is an unreported attempt, not a free one.
func TestCallbackFailedWithoutUsageStaysNil(t *testing.T) {
	f := newAsyncFixture(t)

	payload, _ := json.Marshal(actors.FailedPayload{
		Class:   actors.ClassTimeout,
		Message: "no terminal result survived",
	})
	result := f.handle(actors.CallbackEvent{
		EventID: "ev-failed-no-usage", Sequence: 1, Kind: actors.EventFailed, Payload: payload,
	})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("failed disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts[0].Usage != nil {
		t.Errorf("Usage = %+v, want nil for a failed event that reported none", attempts[0].Usage)
	}
}

// §13.4: "repeated callbacks are idempotent". A redelivery of the same event
// id is recorded as a duplicate and changes nothing — including not
// completing the attempt a second time.
func TestCallbackDuplicateEventIsIdempotent(t *testing.T) {
	f := newAsyncFixture(t)

	first := f.handle(completedEvent("ev-terminal", 1, "done"))
	if first.Disposition != actors.DispositionCommitted {
		t.Fatalf("first delivery disposition = %s (%s), want committed", first.Disposition, first.Diagnostic)
	}

	second := f.handle(completedEvent("ev-terminal", 1, "done"))
	if second.Disposition != actors.DispositionDuplicate {
		t.Fatalf("redelivery disposition = %s, want duplicate", second.Disposition)
	}
	if second.Completion != nil {
		t.Error("a duplicate delivery returned a completion result")
	}
	if !f.hasEvent(actors.TypeCallbackDuplicate) {
		t.Errorf("no duplicate diagnostic was recorded; events were %v", f.eventTypes())
	}

	// One attempt row, not two: the engine was never asked twice.
	var attempts int
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT count(*)::int FROM attempts WHERE node_run_id = $1`, f.nodeRunID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts recorded = %d, want 1", attempts)
	}
}

// §13.4's "monotonically increasing actor sequence": an event that does not
// advance the high-water mark is recorded as a diagnostic and changes
// nothing, even when it is a completion.
func TestCallbackSequenceIsMonotonic(t *testing.T) {
	f := newAsyncFixture(t)

	f.handle(actors.CallbackEvent{EventID: "ev-1", Sequence: 1, Kind: actors.EventAccepted})
	f.handle(actors.CallbackEvent{EventID: "ev-5", Sequence: 5, Kind: actors.EventHeartbeat})

	stale := f.handle(actors.CallbackEvent{EventID: "ev-3", Sequence: 3, Kind: actors.EventProgress})
	if stale.Disposition != actors.DispositionOutOfOrder {
		t.Fatalf("reordered event disposition = %s, want out_of_order", stale.Disposition)
	}

	// A reordered TERMINAL event is the dangerous case: it must not commit.
	lateCompletion := f.handle(completedEvent("ev-4", 4, "stale answer"))
	if lateCompletion.Disposition != actors.DispositionOutOfOrder {
		t.Fatalf("reordered completion disposition = %s, want out_of_order", lateCompletion.Disposition)
	}
	run, err := f.engine.Store().Run(f.ctx, f.run.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.State != engine.RunRunning {
		t.Errorf("run state = %s after a reordered completion, want still running", run.State)
	}
	if !f.hasEvent(actors.TypeCallbackOutOfOrder) {
		t.Errorf("no out-of-order diagnostic was recorded; events were %v", f.eventTypes())
	}

	// The sequence still moves forward for a genuinely newer event.
	if got := f.handle(completedEvent("ev-6", 6, "real answer")); got.Disposition != actors.DispositionCommitted {
		t.Fatalf("in-order completion disposition = %s (%s), want committed", got.Disposition, got.Diagnostic)
	}
}

// A `failed` callback records the actor's §13.5 class as a technical status,
// never as a domain outcome (§3.4).
func TestCallbackFailedRecordsATechnicalStatus(t *testing.T) {
	f := newAsyncFixture(t)

	payload, _ := json.Marshal(actors.FailedPayload{
		Class:   actors.ClassExecution,
		Message: "the tool crashed",
	})
	result := f.handle(actors.CallbackEvent{
		EventID: "ev-failed", Sequence: 1, Kind: actors.EventFailed, Payload: payload,
	})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("failed disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}
	if result.Completion.TechStatus != engine.StatusFailed {
		t.Errorf("tech status = %s, want failed", result.Completion.TechStatus)
	}
	if result.Completion.Outcome != "" {
		t.Errorf("outcome = %q, want empty: a technical failure has no domain answer", result.Completion.Outcome)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	if !bytes.Contains(attempts[0].Result, []byte(`"execution"`)) {
		t.Errorf("attempt result = %s, want the §13.5 class recorded for debugging", attempts[0].Result)
	}
}

// A token minted for one attempt cannot report on another, and an unsigned or
// forged token cannot report at all.
func TestCallbackRefusesForeignAndForgedTokens(t *testing.T) {
	f := newAsyncFixture(t)

	other, err := f.signer.Mint("att_someone_else")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, err = actors.HandleCallback(f.ctx, f.deps, other, completedEvent("ev-1", 1, "x"))
	if !errors.Is(err, actors.ErrUnknownAttempt) {
		t.Fatalf("a token for an unknown attempt: got %v, want ErrUnknownAttempt", err)
	}

	_, err = actors.HandleCallback(f.ctx, f.deps, "cnt1.YWJj.99999999999.AAAA", completedEvent("ev-2", 1, "x"))
	if !errors.Is(err, actors.ErrToken) {
		t.Fatalf("a forged token: got %v, want a token refusal", err)
	}

	run, err := f.engine.Store().Run(f.ctx, f.run.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.State != engine.RunRunning {
		t.Errorf("run state = %s, want still running: neither refused callback may commit", run.State)
	}
}

// The HTTP face: §13.1's callback URL, mounted and driven exactly as an actor
// would drive it.
func TestCallbackHandlerOverHTTP(t *testing.T) {
	f := newAsyncFixture(t)

	server := httptest.NewServer(actors.NewCallbackHandler(f.deps))
	defer server.Close()

	post := func(token string, ev actors.CallbackEvent) (int, string) {
		t.Helper()
		body, _ := json.Marshal(ev)
		req, err := http.NewRequest(http.MethodPost,
			server.URL+fmt.Sprintf(actors.CallbackEventsPathFormat, f.attemptID), bytes.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST callback: %v", err)
		}
		defer resp.Body.Close()
		var decoded struct {
			Disposition string `json:"disposition"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&decoded)
		return resp.StatusCode, decoded.Disposition
	}

	if status, _ := post("", completedEvent("ev-1", 1, "x")); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated callback status = %d, want 401", status)
	}
	if status, _ := post("cnt1.YWJj.99999999999.AAAA", completedEvent("ev-1", 1, "x")); status != http.StatusUnauthorized {
		t.Errorf("forged-token callback status = %d, want 401", status)
	}

	status, disposition := post(f.token, actors.CallbackEvent{EventID: "ev-1", Sequence: 1, Kind: actors.EventHeartbeat})
	if status != http.StatusAccepted {
		t.Errorf("heartbeat status = %d, want 202", status)
	}
	if disposition != string(actors.DispositionRecorded) {
		t.Errorf("heartbeat disposition = %q, want recorded", disposition)
	}

	status, disposition = post(f.token, completedEvent("ev-2", 2, "done"))
	if status != http.StatusAccepted {
		t.Errorf("completion status = %d, want 202", status)
	}
	if disposition != string(actors.DispositionCommitted) {
		t.Errorf("completion disposition = %q, want committed", disposition)
	}

	// A duplicate is 202, not an error: answering 4xx would make a conforming
	// actor retry a delivery that was already handled.
	status, disposition = post(f.token, completedEvent("ev-2", 2, "done"))
	if status != http.StatusAccepted {
		t.Errorf("duplicate status = %d, want 202", status)
	}
	if disposition != string(actors.DispositionDuplicate) {
		t.Errorf("duplicate disposition = %q, want duplicate", disposition)
	}
}

// TestCallbackCompletedAttributesActor proves the async attribution chain
// (migration 0015): an invocation parked with the resolved actors-table row
// id commits that id into attempts.actor_id — the fact per-actor stats
// aggregate on. Found live: the t20 success-signal run's attempt carried
// usage but a NULL actor_id, making the whole async fleet invisible to
// GET /v1alpha1/actors/{id}/stats.
func TestCallbackCompletedAttributesActor(t *testing.T) {
	f := newAsyncFixture(t)

	actorID := "actor_attr_" + f.attemptID
	if _, err := f.store.Pool().Exec(f.ctx, `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		VALUES ($1, $2, $3, 1, 'agent', 'nodes.actor/v1alpha1')
	`, actorID, f.ns.ID, "company/attr-"+f.attemptID); err != nil {
		t.Fatalf("insert actor: %v", err)
	}

	// The fixture already parked the invocation; stamp the resolved row id
	// onto it exactly as a post-0015 worker's StartAsyncWait would have —
	// the point under test is the read-back-and-commit half of the chain.
	if _, err := f.store.Pool().Exec(f.ctx, `
		UPDATE actor_invocations SET actor_id = $1 WHERE attempt_id = $2
	`, actorID, f.attemptID); err != nil {
		t.Fatalf("stamp invocation actor_id: %v", err)
	}

	payload, _ := json.Marshal(actors.CompletedPayload{
		Outcome: "completed",
		Output:  json.RawMessage(`{"summary":"done"}`),
	})
	result := f.handle(actors.CallbackEvent{EventID: "ev-attr", Sequence: 1, Kind: actors.EventCompleted, Payload: payload})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	if attempts[0].ActorID != actorID {
		t.Fatalf("attempts[0].ActorID = %q, want %q (async attribution lost)", attempts[0].ActorID, actorID)
	}
}

// A `completed` callback carrying a workspace_measured block (issue #33a)
// folds it into the node output the completion persists, and the downstream
// /nodes/<id>/output binding — here the end node's own output binding, which
// becomes the run result — receives it. This is the async twin of the worker
// test on completeFromResult.
func TestCallbackCompletedFoldsWorkspaceMeasuredIntoNodeOutput(t *testing.T) {
	f := newAsyncFixture(t)

	measured := `{"measured":true,"repo":"/work/repo","reason":null,"branch":"main",` +
		`"head_before":"aaa111","head_after":"bbb222","status_porcelain":" M x.go",` +
		`"changed_files":["x.go"],"diffstat":" x.go | 2 +-"}`
	payload, _ := json.Marshal(actors.CompletedPayload{
		Outcome:           "completed",
		Output:            json.RawMessage(`{"summary":"done"}`),
		WorkspaceMeasured: json.RawMessage(measured),
	})
	result := f.handle(actors.CallbackEvent{EventID: "ev-ws", Sequence: 1, Kind: actors.EventCompleted, Payload: payload})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}

	// The node's persisted output — read through the same NodeOutput
	// statement a /nodes/<id>/output binding resolves with — carries the
	// block…
	nodeOutput, err := f.store.NodeOutput(f.ctx, f.run.ID, "work")
	if err != nil {
		t.Fatalf("read node output: %v", err)
	}
	var folded struct {
		Summary           string `json:"summary"`
		WorkspaceMeasured *struct {
			Measured  bool     `json:"measured"`
			HeadAfter string   `json:"head_after"`
			Changed   []string `json:"changed_files"`
		} `json:"workspace_measured"`
	}
	if err := json.Unmarshal(nodeOutput, &folded); err != nil {
		t.Fatalf("node output is not an object: %v\noutput: %s", err, nodeOutput)
	}
	if folded.Summary != "done" {
		t.Errorf("the actor's own output was disturbed: %s", nodeOutput)
	}
	if folded.WorkspaceMeasured == nil {
		t.Fatalf("node output carries no workspace_measured block: %s", nodeOutput)
	}
	if !folded.WorkspaceMeasured.Measured || folded.WorkspaceMeasured.HeadAfter != "bbb222" ||
		len(folded.WorkspaceMeasured.Changed) != 1 {
		t.Errorf("workspace_measured lost facts in transit: %s", nodeOutput)
	}

	// …and the downstream binding (finish's /nodes/work/output, which is the
	// run result) receives it.
	run, err := f.engine.Store().Run(f.ctx, f.run.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if !bytes.Contains(run.Output, []byte(`"workspace_measured"`)) || !bytes.Contains(run.Output, []byte(`"bbb222"`)) {
		t.Errorf("run output (bound from /nodes/work/output) = %s, want the workspace_measured block", run.Output)
	}

	// Authority stays actor-reported: the landing path wrote NO observed (or
	// any other) evidence record from the block — §10.4's line between a
	// completion claim and verified evidence.
	var observed int
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT count(*) FROM ledger_records WHERE run_id = $1 AND authority = 'observed'`,
		f.run.ID).Scan(&observed); err != nil {
		t.Fatalf("count observed records: %v", err)
	}
	if observed != 0 {
		t.Errorf("observed-authority ledger records = %d, want 0: workspace_measured is an actor claim, not evidence", observed)
	}
}

// The unmeasured shape — measured:false, every fact null — round-trips
// through the callback path exactly as the bridge sent it: never rewritten
// into an empty diff, never dropped (issue #33a acceptance).
func TestCallbackCompletedUnmeasuredBlockRoundTripsVerbatim(t *testing.T) {
	f := newAsyncFixture(t)

	payload, _ := json.Marshal(actors.CompletedPayload{
		Outcome:           "completed",
		Output:            json.RawMessage(`{"summary":"nothing to measure"}`),
		WorkspaceMeasured: json.RawMessage(unmeasuredBlock),
	})
	result := f.handle(actors.CallbackEvent{EventID: "ev-unmeasured", Sequence: 1, Kind: actors.EventCompleted, Payload: payload})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}

	nodeOutput, err := f.store.NodeOutput(f.ctx, f.run.ID, "work")
	if err != nil {
		t.Fatalf("read node output: %v", err)
	}
	var folded map[string]json.RawMessage
	if err := json.Unmarshal(nodeOutput, &folded); err != nil {
		t.Fatalf("node output is not an object: %v", err)
	}
	block, ok := folded["workspace_measured"]
	if !ok {
		t.Fatalf("the unmeasured block was dropped from node output: %s", nodeOutput)
	}
	var sent, got any
	if err := json.Unmarshal([]byte(unmeasuredBlock), &sent); err != nil {
		t.Fatalf("fixture block: %v", err)
	}
	if err := json.Unmarshal(block, &got); err != nil {
		t.Fatalf("persisted block: %v", err)
	}
	if !reflect.DeepEqual(sent, got) {
		t.Errorf("the unmeasured block was altered in transit:\n sent: %s\n got: %s", unmeasuredBlock, block)
	}
}

// A `failed` callback's workspace_measured block lands in the attempt's
// recorded output next to the failure diagnostic: the session failed AND the
// bridge measured what it left behind — two facts, both kept.
func TestCallbackFailedCarriesWorkspaceMeasuredInAttemptResult(t *testing.T) {
	f := newAsyncFixture(t)

	payload, _ := json.Marshal(actors.FailedPayload{
		Class:             actors.ClassExecution,
		Message:           "the session crashed after editing files",
		WorkspaceMeasured: json.RawMessage(unmeasuredBlock),
	})
	result := f.handle(actors.CallbackEvent{
		EventID: "ev-failed-ws", Sequence: 1, Kind: actors.EventFailed, Payload: payload,
	})
	if result.Disposition != actors.DispositionCommitted {
		t.Fatalf("failed disposition = %s (%s), want committed", result.Disposition, result.Diagnostic)
	}

	attempts, err := f.engine.Store().Attempts(f.ctx, f.nodeRunID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	var recorded struct {
		Error *struct {
			Class string `json:"class"`
		} `json:"error"`
		WorkspaceMeasured *struct {
			Measured bool `json:"measured"`
		} `json:"workspace_measured"`
	}
	if err := json.Unmarshal(attempts[0].Result, &recorded); err != nil {
		t.Fatalf("attempt result is not an object: %v\nresult: %s", err, attempts[0].Result)
	}
	if recorded.Error == nil || recorded.Error.Class != "execution" {
		t.Errorf("the failure diagnostic was disturbed by the merge: %s", attempts[0].Result)
	}
	if recorded.WorkspaceMeasured == nil {
		t.Fatalf("the failed attempt's result carries no workspace_measured block: %s", attempts[0].Result)
	}
	if recorded.WorkspaceMeasured.Measured {
		t.Errorf("measured:false was rewritten to true: %s", attempts[0].Result)
	}
}
