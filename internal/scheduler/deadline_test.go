package scheduler_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/actors"
	"github.com/agentculture/culture-nodes/internal/compiler"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/scheduler"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Task t2's gap: docs/deliveries/2026-08-08-culture-nodes-app-design.md lists
// "`waiting_external` deadline timers don't fail attempts" as remaining
// work. scheduler.go's applyEffect used to treat TimerKindDeadline as
// nothing but an outbox insert; these tests pin the behavior that closes
// that gap -- a fired deadline timer must fail the still-parked attempt
// through the engine's own §12.5 transaction (so its failure edge routes
// exactly like a worker-reported timeout would), and must NOT touch an
// attempt that already completed by the time the timer catches up with it.

// --- fixture ----------------------------------------------------------

// deadlineFixture builds one run of testdata/deadline.workflow.yaml through
// a real engine and claims its entry ("build") node run's work item
// through the real claiming path -- the same discipline
// internal/engine/harness_test.go documents: a test that hand-wrote
// work_items/node_runs rows would prove this package's new code against a
// fixture, not against the store and engine it actually runs with.
type deadlineFixture struct {
	t     *testing.T
	ctx   context.Context
	store *postgres.Store
	ns    postgres.Namespace
	eng   *engine.Engine

	runID          string
	buildNodeRunID string
	buildTokenID   string
	claimed        postgres.ClaimedWork
}

func newDeadlineFixture(t *testing.T, s *postgres.Store) *deadlineFixture {
	t.Helper()
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "test-scheduler-deadline-fixture")

	path := filepath.Join("testdata", "deadline.workflow.yaml")
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

	// Retry delays are zeroed the same way internal/engine's own harness
	// zeroes them: this fixture's workflow declares maxAttempts 1 for
	// "build" anyway, so no retry is ever scheduled, but a zero base keeps
	// the fixture honest about what it is and is not testing.
	eng, err := postgres.NewEngine(s, ns.ID, engine.WithRetryDelays(0, 0))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	run, err := eng.CreateRun(ctx, cw, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	var buildNodeRunID, buildTokenID string
	if err := s.Pool().QueryRow(ctx,
		`SELECT id, token_id FROM node_runs WHERE run_id = $1 AND node_key = 'build'`, run.ID,
	).Scan(&buildNodeRunID, &buildTokenID); err != nil {
		t.Fatalf("find build node run: %v", err)
	}

	claimed, err := s.ClaimWork(ctx, ns.ID, "test-worker", 2*time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimWork: %v", err)
	}
	var build *postgres.ClaimedWork
	for i := range claimed {
		if claimed[i].NodeRunID == buildNodeRunID {
			build = &claimed[i]
			continue
		}
		// Anything else this claim swept up is not this fixture's; hand it
		// straight back so it does not shadow another test.
		if _, err := s.Pool().Exec(ctx,
			`UPDATE work_items SET state = 'ready', lease_owner = NULL, lease_expires_at = NULL WHERE id = $1`,
			claimed[i].ID,
		); err != nil {
			t.Fatalf("release stray claim %s: %v", claimed[i].ID, err)
		}
	}
	if build == nil {
		t.Fatalf("build node run %s's work item did not come back claimable", buildNodeRunID)
	}

	return &deadlineFixture{
		t: t, ctx: ctx, store: s, ns: ns, eng: eng,
		runID: run.ID, buildNodeRunID: buildNodeRunID, buildTokenID: buildTokenID,
		claimed: *build,
	}
}

// startAsyncWait parks the claimed "build" attempt exactly as an async
// dispatch's 202 would (internal/worker/doc.go's "asynchronous dispatch"
// section), scheduling a deadline timer for deadline. It returns the
// actors.PendingInvocation the caller needs to simulate a later callback
// (ResumeWaitingWork + CompleteAttempt) the same way
// internal/actors.commitTerminal would.
func (f *deadlineFixture) startAsyncWait(deadline time.Time) actors.PendingInvocation {
	f.t.Helper()

	attemptID := "att_" + store.NewULID()
	invocationID := "inv_" + store.NewULID()
	const actorRef = "actor://company/builder@sha256:111111"

	in := postgres.StartAsyncWaitInput{
		WorkID:       f.claimed.ID,
		WorkerID:     "test-worker",
		FencingToken: f.claimed.FencingToken,
		Attempt:      int(f.claimed.Attempt),

		NamespaceID: f.ns.ID,
		RunID:       f.runID,
		NodeRunID:   f.buildNodeRunID,
		TokenID:     f.buildTokenID,
		NodeID:      "build",

		AttemptID:             attemptID,
		ActorRef:              actorRef,
		InvocationID:          invocationID,
		HeartbeatAfterSeconds: 0,
		SupportsCancellation:  false,

		Deadline: deadline,
	}
	if err := f.store.StartAsyncWait(f.ctx, in); err != nil {
		f.t.Fatalf("StartAsyncWait: %v", err)
	}

	return actors.PendingInvocation{
		AttemptID:    attemptID,
		NamespaceID:  f.ns.ID,
		RunID:        f.runID,
		NodeRunID:    f.buildNodeRunID,
		TokenID:      f.buildTokenID,
		NodeID:       "build",
		WorkID:       f.claimed.ID,
		WorkerID:     "test-worker",
		FencingToken: f.claimed.FencingToken,
		Attempt:      int(f.claimed.Attempt),
		ActorRef:     actorRef,
		InvocationID: invocationID,
	}
}

// --- read helpers -------------------------------------------------------

func mustNodeRunStatus(t *testing.T, s *postgres.Store, nodeRunID string) (status, outcome string) {
	t.Helper()
	var outcomeText pgtype.Text
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT status, outcome FROM node_runs WHERE id = $1`, nodeRunID,
	).Scan(&status, &outcomeText); err != nil {
		t.Fatalf("mustNodeRunStatus: %v", err)
	}
	if outcomeText.Valid {
		outcome = outcomeText.String
	}
	return status, outcome
}

func mustRunState(t *testing.T, s *postgres.Store, runID string) string {
	t.Helper()
	var state string
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT status FROM runs WHERE id = $1`, runID,
	).Scan(&state); err != nil {
		t.Fatalf("mustRunState: %v", err)
	}
	return state
}

func nodeRunExists(t *testing.T, s *postgres.Store, runID, nodeKey string) bool {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM node_runs WHERE run_id = $1 AND node_key = $2`, runID, nodeKey,
	).Scan(&n); err != nil {
		t.Fatalf("nodeRunExists: %v", err)
	}
	return n > 0
}

func attemptCountAndLatestStatus(t *testing.T, s *postgres.Store, nodeRunID string) (count int, latestStatus string) {
	t.Helper()
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM attempts WHERE node_run_id = $1`, nodeRunID,
	).Scan(&count); err != nil {
		t.Fatalf("attemptCountAndLatestStatus: count: %v", err)
	}
	if count == 0 {
		return 0, ""
	}
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT status FROM attempts WHERE node_run_id = $1 ORDER BY attempt_number DESC LIMIT 1`, nodeRunID,
	).Scan(&latestStatus); err != nil {
		t.Fatalf("attemptCountAndLatestStatus: status: %v", err)
	}
	return count, latestStatus
}

func mustInvocationState(t *testing.T, s *postgres.Store, attemptID string) string {
	t.Helper()
	var state string
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT state FROM actor_invocations WHERE attempt_id = $1`, attemptID,
	).Scan(&state); err != nil {
		t.Fatalf("mustInvocationState: %v", err)
	}
	return state
}

func runEventTypes(t *testing.T, s *postgres.Store, runID string) []string {
	t.Helper()
	rows, err := s.Pool().Query(context.Background(),
		`SELECT event_type FROM events WHERE aggregate_id = $1 ORDER BY sequence`, runID)
	if err != nil {
		t.Fatalf("runEventTypes: %v", err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var et string
		if err := rows.Scan(&et); err != nil {
			t.Fatalf("runEventTypes: scan: %v", err)
		}
		types = append(types, et)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("runEventTypes: %v", err)
	}
	return types
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// --- tests ----------------------------------------------------------------

// TestSchedulerFiresDeadlineTimerFailsWaitingExternalAttemptAndRoutesEdge is
// acceptance criterion 1: an attempt whose waiting_external deadline passes
// is failed by the scheduler with an appended run event, and the run
// routes its failure edge -- the same edge a worker-reported
// engine.StatusTimedOut completion would follow (see
// internal/engine/technical_test.go's TestTechnicalStatusFollowsItsOwnEdge,
// which this fixture's workflow deliberately mirrors).
func TestSchedulerFiresDeadlineTimerFailsWaitingExternalAttemptAndRoutesEdge(t *testing.T) {
	s := requireStore(t)
	f := newDeadlineFixture(t, s)
	inv := f.startAsyncWait(time.Now().Add(-time.Second))

	sch := scheduler.New(s, scheduler.Options{TickInterval: 25 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sch.Run(runCtx) }()

	waitFor(t, 5*time.Second, func() bool {
		status, _ := mustNodeRunStatus(t, s, f.buildNodeRunID)
		return status == "failed"
	})

	status, outcome := mustNodeRunStatus(t, s, f.buildNodeRunID)
	if status != "failed" {
		t.Fatalf("build node run status = %q, want %q", status, "failed")
	}
	if outcome != "timed_out" {
		t.Fatalf("build node run outcome = %q, want %q", outcome, "timed_out")
	}

	if count, latest := attemptCountAndLatestStatus(t, s, f.buildNodeRunID); count != 1 || latest != "timed_out" {
		t.Fatalf("build attempts = %d (latest status %q), want 1 attempt with status %q", count, latest, "timed_out")
	}

	// The failure edge routed: build declares maxAttempts 1 (no retry) and
	// the workflow's build.timed_out edge targets repair, so a repair node
	// run must now exist for this run -- the same routing
	// TestTechnicalStatusFollowsItsOwnEdge proves for a worker-reported
	// timeout.
	if !nodeRunExists(t, s, f.runID, "repair") {
		t.Fatalf("no repair node run was created; build.timed_out did not route its edge")
	}

	// The run itself did not fail -- only the node run did, and the token
	// moved on to repair.
	if got := mustRunState(t, s, f.runID); got != "running" {
		t.Fatalf("run state = %q, want %q (only the node run failed; the run kept going)", got, "running")
	}

	// A run event was appended for the failure (PRD §12.5 step 7, through
	// the engine's own transactional outbox -- see engineQueries.AppendEvent).
	events := runEventTypes(t, s, f.runID)
	if !containsString(events, engine.TypeAttemptCompleted) {
		t.Errorf("run events = %v, want one of type %s", events, engine.TypeAttemptCompleted)
	}
	if !containsString(events, engine.TypeNodeRunFailed) {
		t.Errorf("run events = %v, want one of type %s", events, engine.TypeNodeRunFailed)
	}
	if !containsString(events, engine.TypeTokenTransitioned) {
		t.Errorf("run events = %v, want one of type %s (the token advanced to repair)", events, engine.TypeTokenTransitioned)
	}

	if got := mustInvocationState(t, s, inv.AttemptID); got != actors.InvocationCompleted {
		t.Errorf("invocation state = %q, want %q", got, actors.InvocationCompleted)
	}
}

// TestSchedulerFiresDeadlineTimerAfterAttemptAlreadyCompletedIsANoOp is
// acceptance criterion 2: a completed attempt never receives a late
// deadline failure. Nothing in this codebase cancels a deadline timer on a
// normal completion (internal/store/postgres.CancelTimer has no production
// caller), so a deadline timer scheduled for an attempt that goes on to
// complete normally, well before its deadline, is exactly the case that
// must resolve as a harmless no-op when it eventually fires.
func TestSchedulerFiresDeadlineTimerAfterAttemptAlreadyCompletedIsANoOp(t *testing.T) {
	s := requireStore(t)
	f := newDeadlineFixture(t, s)
	inv := f.startAsyncWait(time.Now().Add(-time.Second))

	// Simulate the actor's terminal callback arriving before the (already
	// due) deadline timer ever gets a chance to fire -- the exact two-step
	// shape internal/actors.commitTerminal uses: resume the parked work
	// item under its dispatch fencing tuple, then commit through the
	// engine.
	cs, err := postgres.NewCallbackStore(s, f.ns.ID)
	if err != nil {
		t.Fatalf("NewCallbackStore: %v", err)
	}
	if err := cs.ResumeWaitingWork(context.Background(), inv, time.Minute); err != nil {
		t.Fatalf("ResumeWaitingWork: %v", err)
	}
	result, err := f.eng.CompleteAttempt(context.Background(), engine.CompletionRequest{
		WorkID:       inv.WorkID,
		WorkerID:     inv.WorkerID,
		FencingToken: inv.FencingToken,
		Attempt:      inv.Attempt,
		TechStatus:   engine.StatusSucceeded,
		Outcome:      "completed",
		Output:       json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("CompleteAttempt: %v", err)
	}
	if result.NodeRunState != engine.NodeRunCompleted {
		t.Fatalf("build node run state after normal completion = %s, want %s", result.NodeRunState, engine.NodeRunCompleted)
	}
	if err := cs.CloseInvocation(context.Background(), inv.AttemptID, actors.InvocationCompleted); err != nil {
		t.Fatalf("CloseInvocation: %v", err)
	}
	eventsBefore := runEventTypes(t, s, f.runID)

	// The deadline timer StartAsyncWait scheduled is still 'pending' -- it
	// was never cancelled -- and is already due. Let the scheduler fire it.
	sch := scheduler.New(s, scheduler.Options{TickInterval: 25 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sch.Run(runCtx) }()

	waitFor(t, 5*time.Second, func() bool {
		return sch.Health().Status == scheduler.StatusActive && !sch.Health().LastTick.IsZero()
	})
	// Give the (already-due) deadline timer several ticks' worth of time to
	// be claimed and fired.
	time.Sleep(300 * time.Millisecond)
	cancel()

	// Nothing about the already-completed attempt changed: same status,
	// same outcome, no second attempt row, no new run events, invocation
	// still closed the way the normal completion left it.
	status, outcome := mustNodeRunStatus(t, s, f.buildNodeRunID)
	if status != "completed" {
		t.Fatalf("build node run status after late deadline fire = %q, want unchanged %q", status, "completed")
	}
	if outcome != "completed" {
		t.Fatalf("build node run outcome after late deadline fire = %q, want unchanged %q", outcome, "completed")
	}
	if count, latest := attemptCountAndLatestStatus(t, s, f.buildNodeRunID); count != 1 || latest != "succeeded" {
		t.Fatalf("build attempts after late deadline fire = %d (latest status %q), want exactly 1 attempt with status %q (no late failure attempt)", count, latest, "succeeded")
	}
	if got := mustInvocationState(t, s, inv.AttemptID); got != actors.InvocationCompleted {
		t.Errorf("invocation state after late deadline fire = %q, want unchanged %q", got, actors.InvocationCompleted)
	}
	if got := runEventTypes(t, s, f.runID); len(got) != len(eventsBefore) {
		t.Fatalf("run events after late deadline fire = %v, want unchanged %v (no event from the late timer)", got, eventsBefore)
	}
}
