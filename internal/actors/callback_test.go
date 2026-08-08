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
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
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
}

func newAsyncFixture(t *testing.T) *asyncFixture {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()

	ns := pgtest.MustNamespace(t, s, "actors")
	eng, err := storepg.NewEngine(s, ns.ID, engine.WithRetryDelays(0, 0))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	callbacks, err := storepg.NewCallbackStore(s, ns.ID)
	if err != nil {
		t.Fatalf("NewCallbackStore: %v", err)
	}
	signer, err := actors.NewTokenSigner([]byte(testSecret))
	if err != nil {
		t.Fatalf("NewTokenSigner: %v", err)
	}

	f := &asyncFixture{
		t:         t,
		ctx:       ctx,
		store:     s,
		ns:        ns,
		engine:    eng,
		callbacks: callbacks,
		signer:    signer,
		deps: actors.CallbackDeps{
			Store:  callbacks,
			Engine: eng,
			Signer: signer,
		},
		cw:       compileFixture(t, "async.workflow.yaml"),
		workerID: "worker-" + t.Name(),
	}

	run, err := eng.CreateRun(ctx, f.cw, json.RawMessage(`{"subject":"async"}`))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f.run = run
	f.nodeRunID = f.readyNodeRun(run.ID)
	f.claimed = f.claim(f.workerID, f.nodeRunID)
	f.attemptID = "att_" + f.claimed.ID
	f.park()
	return f
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
// handing back anything else it happened to claim (ClaimWork is namespace-
// wide, so a parallel test's item must not be left leased).
func (f *asyncFixture) claim(workerID, nodeRunID string) storepg.ClaimedWork {
	f.t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		claimed, err := f.store.ClaimWork(f.ctx, workerID, testLease, 20)
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
	claimed, err := f.store.ClaimWork(f.ctx, "other-worker", testLease, 10)
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

// §13.4's closing rule, and the reason the whole fencing tuple is stored:
// "completion after cancellation or attempt replacement is recorded as a late
// diagnostic event but cannot commit workflow state."
func TestCallbackAfterNewerAttemptIsLateAndCommitsNothing(t *testing.T) {
	f := newAsyncFixture(t)

	// A fired deadline timer returns a parked item to 'ready' (the
	// scheduler's wait/retry effect targets `state <> 'completed'`), and a
	// second worker then claims it — which bumps the fencing token and the
	// attempt number out from under the first invocation.
	if _, err := f.store.Pool().Exec(f.ctx,
		`UPDATE work_items SET state = 'ready', available_at = now(), state_version = state_version + 1 WHERE id = $1`,
		f.claimed.ID); err != nil {
		t.Fatalf("simulate deadline timer effect: %v", err)
	}
	reclaimed := f.claim("second-worker", f.nodeRunID)
	if reclaimed.FencingToken <= f.claimed.FencingToken {
		t.Fatalf("reclaim did not advance the fencing token: %d then %d",
			f.claimed.FencingToken, reclaimed.FencingToken)
	}

	// Now the original actor finally reports.
	result := f.handle(completedEvent("ev-late", 1, "an hour late"))
	if result.Disposition != actors.DispositionLate {
		t.Fatalf("late completion disposition = %s (%s), want late", result.Disposition, result.Diagnostic)
	}
	if result.Completion != nil {
		t.Error("a late completion returned a committed result")
	}
	if result.Diagnostic == "" {
		t.Error("a late completion carried no diagnostic explaining why")
	}

	if !f.hasEvent(actors.TypeCallbackLate) {
		t.Errorf("no late diagnostic event was recorded; events were %v", f.eventTypes())
	}

	// Nothing committed: no attempt row, the run is still running, and the
	// newer claim still holds the work item.
	var attempts int
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT count(*)::int FROM attempts WHERE node_run_id = $1`, f.nodeRunID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 0 {
		t.Errorf("attempts recorded = %d, want 0: a late completion writes no attempt", attempts)
	}
	run, err := f.engine.Store().Run(f.ctx, f.run.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.State != engine.RunRunning {
		t.Errorf("run state = %s, want still running", run.State)
	}
	state, owner := f.workItemState(f.claimed.ID)
	if state != "leased" || owner == nil || *owner != "second-worker" {
		t.Errorf("work item is %q owned by %v, want leased by second-worker", state, owner)
	}

	// The superseded invocation is closed, so an operator listing waiting
	// invocations does not see one that will never be answered.
	var invocationState string
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT state FROM actor_invocations WHERE attempt_id = $1`, f.attemptID).Scan(&invocationState); err != nil {
		t.Fatalf("read invocation: %v", err)
	}
	if invocationState != actors.InvocationSuperseded {
		t.Errorf("invocation state = %q, want superseded", invocationState)
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
