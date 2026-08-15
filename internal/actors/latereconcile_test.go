package actors_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/engine"
	idstore "github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Late-callback reconciliation (task t11, spec claims c37/c39).
//
// The case these tests pin is the one the 2026-08-14 cycle paid for: a node
// run's deadline expires, the engine records a `timed_out` attempt and gives
// up, and the actor session — which never stopped — finally reports back with
// what it actually did. Before t11 that report left NO attempt row at all
// (internal/actors/callback.go's late() recorded a TypeCallbackLate event and
// nothing else), so the tokens, the model, the termination reason and the
// preserve branch of a session that really ran were readable only by parsing
// a diagnostic event body — which is to say, not readable at all by anything
// that reads the attempts table.
//
// The representation is a SUPERSEDING attempt row (PRD §10.4: records are
// immutable, corrections append with `supersedes`), and the whole difficulty
// is that it must not lie in the other direction: `attempts` is uniquely
// keyed on (node_run_id, attempt_number), so the correction has to take its
// OWN number, and per-actor retry burn counts every attempt row regardless
// of outcome (internal/store/postgres/actorstats.go). A naive second row
// would therefore charge the actor for two dispatch attempts where the
// deployment only ever paid for one — re-creating, one layer down, exactly
// the actor-scoring distortion issue #82 exists to remove.

// deadlineExpiry does to the fixture what internal/scheduler's
// failWaitingExternal does to a live run whose deadline timer fires: resume
// the parked work item under the dispatch's own fencing tuple, complete the
// attempt as `timed_out`, and close the wait record. It deliberately drives
// the real store and the real engine rather than hand-writing an attempts
// row — the number, the fencing token and the actor attribution have to be
// the ones the runtime assigns, or the reconciliation is being tested
// against a fixture instead of against the deadline path.
func (f *asyncFixture) deadlineExpiry() {
	f.t.Helper()
	if err := f.callbacks.ResumeWaitingWork(f.ctx, f.pendingInvocation(), actors.DefaultResumeLease); err != nil {
		f.t.Fatalf("deadlineExpiry: resume parked work: %v", err)
	}
	if _, err := f.engine.CompleteAttempt(f.ctx, engine.CompletionRequest{
		WorkID:       f.claimed.ID,
		WorkerID:     f.workerID,
		FencingToken: f.claimed.FencingToken,
		Attempt:      int(f.claimed.Attempt),
		ActorID:      f.actorID,
		TechStatus:   engine.StatusTimedOut,
		Output:       json.RawMessage(`{"error":{"class":"timeout","detail":"deadline timer expired before a terminal callback arrived"}}`),
	}); err != nil {
		f.t.Fatalf("deadlineExpiry: complete attempt as timed out: %v", err)
	}
	if err := f.callbacks.CloseInvocation(f.ctx, f.attemptID, actors.InvocationCompleted); err != nil {
		f.t.Fatalf("deadlineExpiry: close wait record: %v", err)
	}
}

func (f *asyncFixture) pendingInvocation() actors.PendingInvocation {
	f.t.Helper()
	inv, err := f.callbacks.Invocation(f.ctx, f.attemptID)
	if err != nil {
		f.t.Fatalf("load pending invocation %s: %v", f.attemptID, err)
	}
	return inv
}

// attemptRow is one attempts row, read back through the columns this task
// cares about. Everything is a pointer because "the column is NULL" is a
// distinct fact from any value it could hold — the whole point of the
// reconciliation is that NULL stops being the honest answer.
type attemptRow struct {
	ID                string
	Number            int
	Status            string
	ActorID           *string
	FencingToken      *int64
	UsageInputTokens  *int64
	UsageOutputTokens *int64
	UsageModel        *string
	TerminationReason *string
	ContinuationRef   *string
	PreserveBranch    *string
	PreservePushed    *bool
	PreserveRemote    *string
	Supersedes        *string
}

func (f *asyncFixture) attemptRows() []attemptRow {
	f.t.Helper()
	rows, err := f.store.Pool().Query(f.ctx, `
		SELECT id, attempt_number, status, actor_id, fencing_token,
		       usage_input_tokens, usage_output_tokens, usage_model,
		       termination_reason, continuation_ref,
		       preserve_branch, preserve_pushed, preserve_remote, supersedes
		FROM attempts
		WHERE node_run_id = $1
		ORDER BY attempt_number
	`, f.nodeRunID)
	if err != nil {
		f.t.Fatalf("read attempts: %v", err)
	}
	defer rows.Close()

	var out []attemptRow
	for rows.Next() {
		var a attemptRow
		if err := rows.Scan(&a.ID, &a.Number, &a.Status, &a.ActorID, &a.FencingToken,
			&a.UsageInputTokens, &a.UsageOutputTokens, &a.UsageModel,
			&a.TerminationReason, &a.ContinuationRef,
			&a.PreserveBranch, &a.PreservePushed, &a.PreserveRemote, &a.Supersedes,
		); err != nil {
			f.t.Fatalf("scan attempt: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("read attempts: %v", err)
	}
	return out
}

func (f *asyncFixture) retryBurnAttempts(actorID string) int {
	f.t.Helper()
	es, err := storepg.NewEngineStore(f.store, f.ns.ID)
	if err != nil {
		f.t.Fatalf("NewEngineStore: %v", err)
	}
	stats, err := es.ActorStats(f.ctx, actorID)
	if err != nil {
		f.t.Fatalf("ActorStats: %v", err)
	}
	return stats.Total.RetryBurn.Attempts
}

// mustRegisterActor inserts one agent actor row, the identity a dispatch
// attributes an attempt to (migration 0015's actor_invocations.actor_id, and
// through it attempts.actor_id).
func mustRegisterActor(t *testing.T, s *storepg.Store, namespaceID string) string {
	t.Helper()
	actorID := "actor-row-" + idstore.NewULID()
	if _, err := s.Pool().Exec(context.Background(), `
		INSERT INTO actors (id, namespace_id, actor_key, revision, kind, protocol)
		VALUES ($1, $2, $3, 1, 'agent', 'internal')
	`, actorID, namespaceID, actorID); err != nil {
		t.Fatalf("register actor: %v", err)
	}
	return actorID
}

func ptr[T any](v T) *T { return &v }

// lateFailedEvent is what a bridge sends after its session was SIGTERMed by
// the deadline's cancel and its preserve-on-failure path ran: a §13.4 failed
// event carrying the tokens actually burned, the model that burned them, the
// provider's termination reason, and the branch the bridge committed the
// work onto. Task t13 pins the bridge half of this; here it is the input.
func lateFailedEvent(id string, sequence int64) actors.CallbackEvent {
	payload, _ := json.Marshal(actors.FailedPayload{
		Class:   actors.ClassTimeout,
		Message: "session cancelled by the control plane deadline",
		Usage: &actors.Usage{
			InputTokens:  4321,
			OutputTokens: 1234,
			Model:        ptr("claude-opus-5[1m]"),
		},
		TerminationReason: ptr("cancelled"),
		Preserve: &actors.Preserve{
			Attempted: true,
			Committed: true,
			Branch:    ptr("preserve/att-late-reconcile"),
			Commit:    ptr("3f9bd3c"),
			Pushed:    true,
			Remote:    ptr("origin"),
		},
	})
	return actors.CallbackEvent{EventID: id, Sequence: sequence, Kind: actors.EventFailed, Payload: payload}
}

// A terminal callback that arrives after the node run's deadline already
// timed the attempt out must leave a record in the attempts table — not only
// a TypeCallbackLate event whose body an operator would have to parse. The
// session ran, burned tokens on a named model, ended for a reason the
// provider stated, and left its work on a preserve branch; every one of
// those is a fact about the ATTEMPT, and the attempts table is where the run
// explains itself.
func TestLateCallbackAfterDeadlineRecordsSupersedingAttempt(t *testing.T) {
	f := newAsyncFixtureForActor(t)
	actorID := f.actorID

	f.deadlineExpiry()

	before := f.attemptRows()
	if len(before) != 1 {
		t.Fatalf("after the deadline there are %d attempt rows, want 1 (the timed-out attempt)", len(before))
	}
	timedOut := before[0]
	if timedOut.Status != string(engine.StatusTimedOut) {
		t.Fatalf("the deadline's attempt status = %q, want %q", timedOut.Status, engine.StatusTimedOut)
	}
	if timedOut.UsageModel != nil {
		t.Fatalf("the timed-out attempt already names a model (%q); this test's premise is that it cannot", *timedOut.UsageModel)
	}
	if timedOut.Supersedes != nil {
		t.Fatalf("the timed-out attempt supersedes %q; a fresh dispatch supersedes nothing", *timedOut.Supersedes)
	}

	// The actor finally reports. §13.4 still refuses to let it commit
	// workflow state — that part is unchanged and deliberate.
	result := f.handle(lateFailedEvent("ev-late-reconcile", 1))
	if result.Disposition != actors.DispositionLate {
		t.Fatalf("disposition = %s (%s), want late", result.Disposition, result.Diagnostic)
	}
	if result.Completion != nil {
		t.Error("a late completion committed a result; §13.4 forbids it")
	}
	if !f.hasEvent(actors.TypeCallbackLate) {
		t.Errorf("no late diagnostic event was recorded; events were %v", f.eventTypes())
	}

	after := f.attemptRows()
	if len(after) != 2 {
		t.Fatalf("attempt rows after reconciliation = %d, want 2 (the timed-out record and the correction that supersedes it)", len(after))
	}
	superseding := after[1]

	// The correction points at the record it corrects. Nothing is rewritten:
	// the timed-out row still says exactly what it said before.
	if superseding.Supersedes == nil {
		t.Fatal("the superseding attempt's supersedes column is NULL; the correction names no record")
	}
	if *superseding.Supersedes != timedOut.ID {
		t.Errorf("supersedes = %q, want the timed-out attempt %q", *superseding.Supersedes, timedOut.ID)
	}
	if got := f.attemptRows()[0]; got.Status != timedOut.Status || got.Supersedes != nil {
		t.Errorf("the timed-out record changed under the correction: status %q, supersedes %v", got.Status, got.Supersedes)
	}
	if superseding.Number == timedOut.Number {
		t.Errorf("the correction reused attempt_number %d; attempts_node_run_attempt_number_key forbids it", superseding.Number)
	}

	// Everything the session actually reported is now readable FROM THE
	// ATTEMPTS TABLE.
	if superseding.UsageModel == nil || *superseding.UsageModel != "claude-opus-5[1m]" {
		t.Errorf("usage_model = %v, want the model the late report named", derefString(superseding.UsageModel))
	}
	if superseding.UsageInputTokens == nil || *superseding.UsageInputTokens != 4321 {
		t.Errorf("usage_input_tokens = %v, want 4321", derefInt64(superseding.UsageInputTokens))
	}
	if superseding.UsageOutputTokens == nil || *superseding.UsageOutputTokens != 1234 {
		t.Errorf("usage_output_tokens = %v, want 1234", derefInt64(superseding.UsageOutputTokens))
	}
	if superseding.TerminationReason == nil || *superseding.TerminationReason != "cancelled" {
		t.Errorf("termination_reason = %v, want \"cancelled\"", derefString(superseding.TerminationReason))
	}
	if superseding.PreserveBranch == nil || *superseding.PreserveBranch != "preserve/att-late-reconcile" {
		t.Errorf("preserve_branch = %v, want the branch the bridge committed", derefString(superseding.PreserveBranch))
	}
	if superseding.PreservePushed == nil || !*superseding.PreservePushed {
		t.Errorf("preserve_pushed = %v, want true", superseding.PreservePushed)
	}
	if superseding.PreserveRemote == nil || *superseding.PreserveRemote != "origin" {
		t.Errorf("preserve_remote = %v, want \"origin\"", derefString(superseding.PreserveRemote))
	}
	if superseding.ActorID == nil || *superseding.ActorID != actorID {
		t.Errorf("actor_id = %v, want the dispatch's actor %q", derefString(superseding.ActorID), actorID)
	}
	if superseding.FencingToken == nil || *superseding.FencingToken != f.claimed.FencingToken {
		t.Errorf("fencing_token = %v, want the dispatch's own token %d", superseding.FencingToken, f.claimed.FencingToken)
	}
}

// The correction must not charge the actor for a second dispatch. Retry burn
// counts every attempt row regardless of outcome, and one deadline
// reconciliation describes ONE session — so the number an operator reads
// from GET /v1alpha1/actors/{id}/stats has to be identical before and after.
func TestDeadlineReconciliationDoesNotInflateRetryBurn(t *testing.T) {
	f := newAsyncFixtureForActor(t)
	actorID := f.actorID

	f.deadlineExpiry()

	before := f.retryBurnAttempts(actorID)
	if before != 1 {
		t.Fatalf("retry burn after the deadline = %d, want 1: the timed-out dispatch is one attempt this actor made", before)
	}

	if result := f.handle(lateFailedEvent("ev-late-burn", 1)); result.Disposition != actors.DispositionLate {
		t.Fatalf("disposition = %s (%s), want late", result.Disposition, result.Diagnostic)
	}

	if after := f.retryBurnAttempts(actorID); after != before {
		t.Errorf("retry burn attempts = %d after reconciliation, was %d before: one late report is not a second try",
			after, before)
	}
}

// The usage the reconciliation carries has to reach the actor's rollup, and
// exactly once: the superseded record reported no tokens, the correction
// reports the real ones, and an operator summing them must see one attempt
// that reported rather than one reported plus one not-reported.
func TestDeadlineReconciliationAttributesUsageExactlyOnce(t *testing.T) {
	f := newAsyncFixtureForActor(t)
	actorID := f.actorID

	f.deadlineExpiry()

	if result := f.handle(lateFailedEvent("ev-late-usage", 1)); result.Disposition != actors.DispositionLate {
		t.Fatalf("disposition = %s (%s), want late", result.Disposition, result.Diagnostic)
	}

	es, err := storepg.NewEngineStore(f.store, f.ns.ID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	stats, err := es.ActorStats(f.ctx, actorID)
	if err != nil {
		t.Fatalf("ActorStats: %v", err)
	}
	usage := stats.Total.Usage
	if usage.InputTokens != 4321 || usage.OutputTokens != 1234 {
		t.Errorf("usage totals = %d in / %d out, want 4321/1234", usage.InputTokens, usage.OutputTokens)
	}
	if usage.AttemptsReported != 1 {
		t.Errorf("attempts reported = %d, want 1", usage.AttemptsReported)
	}
	if usage.AttemptsNotReported != 0 {
		t.Errorf("attempts not reported = %d, want 0: the record that reported nothing was superseded by one that did",
			usage.AttemptsNotReported)
	}
}

func derefString(s *string) string {
	if s == nil {
		return "<NULL>"
	}
	return fmt.Sprintf("%q", *s)
}

func derefInt64(v *int64) string {
	if v == nil {
		return "<NULL>"
	}
	return fmt.Sprintf("%d", *v)
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

	// Nothing about the RUN moved: it is still running and the newer claim
	// still holds the work item.
	//
	// The report itself is recorded, though — this is the one thing task t11
	// changed here, and it changed it deliberately. The reclaiming worker's
	// dispatch had not completed when this late report arrived, so no attempt
	// row exists under the ORIGINAL fencing tuple for the correction to
	// supersede; it is appended with `supersedes` NULL, standing on its own
	// as the record of a session that genuinely ran (ADR 0012's
	// consequences). What must not appear is a committed completion, and
	// that is asserted above and below.
	var attempts, supersedingRows int
	if err := f.store.Pool().QueryRow(f.ctx,
		`SELECT count(*)::int, count(*) FILTER (WHERE supersedes IS NOT NULL)::int
		 FROM attempts WHERE node_run_id = $1`, f.nodeRunID).Scan(&attempts, &supersedingRows); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts recorded = %d, want 1: the late report leaves its own record", attempts)
	}
	if supersedingRows != 0 {
		t.Errorf("superseding rows = %d, want 0: there was no earlier record under this dispatch to correct", supersedingRows)
	}
	if result.SupersedingAttemptID == "" {
		t.Error("the late result named no attempt record")
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
