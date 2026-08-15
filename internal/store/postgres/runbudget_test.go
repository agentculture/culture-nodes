package postgres_test

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// The two durable measures the declared economic budget spends against (task
// t11, spec claim c6 / honesty h5): how many NEW provider sessions a run has
// started, and how much input it has sent that the provider did not serve
// from cache.

// newRunBudgetStore returns a store, an engine store over the same pool, and
// the namespace both are scoped to.
func newRunBudgetStore(t *testing.T) (*postgres.Store, *postgres.EngineStore, postgres.Namespace) {
	t.Helper()
	s := requireStore(t)
	ns := mustNamespace(t, s, "run-budget")
	es, err := postgres.NewEngineStore(s, ns.ID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	return s, es, ns
}

// secondNodeRun adds another node run to an existing run, so a run-scoped
// aggregate has more than one node run to aggregate over.
func secondNodeRun(t *testing.T, es *postgres.EngineStore, nsID, runID, tokenID string) string {
	t.Helper()
	id := store.NewULID()
	err := es.InTx(context.Background(), func(ctx context.Context, tx engine.Tx) error {
		return tx.InsertNodeRun(ctx, engine.NodeRun{
			ID: id, NamespaceID: nsID, RunID: runID, TokenID: tokenID,
			NodeID: "second", State: engine.NodeRunReady, VisitCount: 1,
		})
	})
	if err != nil {
		t.Fatalf("insert second node run: %v", err)
	}
	return id
}

// An attempt that reported input tokens but NO cached figure charges its
// input IN FULL. A backend that reports no cache telemetry is not a backend
// with a 0% hit rate, and a budget must not hand out a discount it cannot
// see. The attempt that reported nothing at all charges nothing — and stays
// visible as unmeasured rather than as free.
func TestRunUncachedInputChargesAbsentCacheTelemetryInFull(t *testing.T) {
	s, es, ns := newRunBudgetStore(t)
	ctx := context.Background()
	runID, tokenID, firstNodeRun, _ := seedRun(t, es, ns.ID, "a")
	secondRun := secondNodeRun(t, es, ns.ID, runID, tokenID)

	// Cache telemetry present: 100 in, 40 of it cached -> 60 uncached.
	insertAttempt(t, es, firstNodeRun, 1, engine.StatusSucceeded, &engine.Usage{
		InputTokens: 100, OutputTokens: 10, CachedInputTokens: ptr(int64(40)),
	})
	// Cache telemetry absent: the whole 200 is charged.
	insertAttempt(t, es, firstNodeRun, 2, engine.StatusFailed, &engine.Usage{
		InputTokens: 200, OutputTokens: 20,
	})
	// No usage at all: charged nothing, counted as unmeasured.
	insertAttempt(t, es, secondRun, 1, engine.StatusFailed, nil)
	// A second node run's reported usage still belongs to the same run.
	insertAttempt(t, es, secondRun, 2, engine.StatusSucceeded, &engine.Usage{
		InputTokens: 30, OutputTokens: 5, CachedInputTokens: ptr(int64(30)),
	})

	spend, err := s.RunUncachedInput(ctx, runID)
	if err != nil {
		t.Fatalf("RunUncachedInput: %v", err)
	}
	if spend.Tokens != 260 {
		t.Errorf("uncached tokens = %d, want 260 (60 measured + 200 unmeasured-cache + 0 fully cached)", spend.Tokens)
	}
	if spend.AttemptsWithoutCacheTelemetry != 1 {
		t.Errorf("AttemptsWithoutCacheTelemetry = %d, want 1", spend.AttemptsWithoutCacheTelemetry)
	}
	if spend.AttemptsReported != 3 {
		t.Errorf("AttemptsReported = %d, want 3", spend.AttemptsReported)
	}
	if spend.AttemptsNotReported != 1 {
		t.Errorf("AttemptsNotReported = %d, want 1: unmeasured spend must stay visible, never read as zero", spend.AttemptsNotReported)
	}
}

// insertCorrection appends what a late-callback reconciliation leaves: an
// attempt row that names the record it corrects (migrations/0028, ADR 0012
// §1). It is the same InsertAttempt path an ordinary dispatch takes — the
// only difference is the `supersedes` link, which is exactly the thing the
// budget has to read.
func insertCorrection(
	t *testing.T, es *postgres.EngineStore, nodeRunID string, n int,
	status engine.TechStatus, usage *engine.Usage, supersedes string,
) string {
	t.Helper()
	id := store.NewULID()
	err := es.InTx(context.Background(), func(ctx context.Context, tx engine.Tx) error {
		return tx.InsertAttempt(ctx, engine.Attempt{
			ID: id, NodeRunID: nodeRunID, Number: n, Status: status,
			Usage: usage, Supersedes: &supersedes,
		})
	})
	if err != nil {
		t.Fatalf("insert correction %d on node run %s: %v", n, nodeRunID, err)
	}
	return id
}

// ADR 0012 §3's reader rule is not optional for the budget: a row that
// another row supersedes is superseded history, and every aggregate over
// `attempts` drops it. The budget is an aggregate over `attempts` — and one
// that STOPS A RUN, so counting a superseded row twice does not merely
// misreport, it spends the author's declared ceiling on a session that
// happened once and makes `budget.maxUncachedInput` trip early.
//
// Both flavours of reconciliation are here, because they break the measure
// differently. A deadline's timed-out record reported no usage, so counting
// it beside its correction inflates the COUNTS (one dispatch read as one
// reported attempt plus one unreported one). A completion whose usage a late
// report corrects reported real tokens, so counting it beside its correction
// inflates the SUM as well.
func TestRunUncachedInputExcludesSupersededAttempts(t *testing.T) {
	s, es, ns := newRunBudgetStore(t)
	ctx := context.Background()
	runID, tokenID, deadlineNodeRun, _ := seedRun(t, es, ns.ID, "a")
	correctedNodeRun := secondNodeRun(t, es, ns.ID, runID, tokenID)

	// The deadline expired and recorded a timed-out attempt that could report
	// nothing; the session it bounded then reported 200 uncached input tokens.
	timedOut := insertAttempt(t, es, deadlineNodeRun, 1, engine.StatusTimedOut, nil)
	insertCorrection(t, es, deadlineNodeRun, 2, engine.StatusFailed,
		&engine.Usage{InputTokens: 200, OutputTokens: 20}, timedOut)

	// A dispatch that failed reporting 500 input tokens, corrected by the
	// session's own later report of 100 in, 40 of them cached -> 60 uncached.
	failed := insertAttempt(t, es, correctedNodeRun, 1, engine.StatusFailed,
		&engine.Usage{InputTokens: 500, OutputTokens: 50})
	insertCorrection(t, es, correctedNodeRun, 2, engine.StatusFailed,
		&engine.Usage{InputTokens: 100, OutputTokens: 10, CachedInputTokens: ptr(int64(40))}, failed)

	spend, err := s.RunUncachedInput(ctx, runID)
	if err != nil {
		t.Fatalf("RunUncachedInput: %v", err)
	}
	if spend.Tokens != 260 {
		t.Errorf("uncached tokens = %d, want 260 (200 + 60): a superseded row's tokens are the correction's tokens counted twice", spend.Tokens)
	}
	if spend.AttemptsWithoutCacheTelemetry != 1 {
		t.Errorf("AttemptsWithoutCacheTelemetry = %d, want 1: only the correction that reported no cached figure", spend.AttemptsWithoutCacheTelemetry)
	}
	if spend.AttemptsReported != 2 {
		t.Errorf("AttemptsReported = %d, want 2: two dispatches ran, each with one current record", spend.AttemptsReported)
	}
	if spend.AttemptsNotReported != 0 {
		t.Errorf("AttemptsNotReported = %d, want 0: the record that reported nothing was superseded by one that did", spend.AttemptsNotReported)
	}
}

func TestRunUncachedInputOnARunThatSpentNothing(t *testing.T) {
	s, es, ns := newRunBudgetStore(t)
	runID, _, _, _ := seedRun(t, es, ns.ID, "a")

	spend, err := s.RunUncachedInput(context.Background(), runID)
	if err != nil {
		t.Fatalf("RunUncachedInput: %v", err)
	}
	if spend.Tokens != 0 || spend.AttemptsReported != 0 || spend.AttemptsNotReported != 0 {
		t.Errorf("spend = %+v, want the zero measure for a run with no attempts", spend)
	}
}

// The session ledger: one row per COLD START, counted per run, and idempotent
// on the protocol attempt id so a re-entered dispatch cannot charge twice.
func TestRunSessionStartsAreCountedAndIdempotent(t *testing.T) {
	s, es, ns := newRunBudgetStore(t)
	ctx := context.Background()
	runID, _, nodeRunID, _ := seedRun(t, es, ns.ID, "a")

	if got, err := s.RunSessionStarts(ctx, runID); err != nil || got != 0 {
		t.Fatalf("RunSessionStarts on a fresh run = %d, %v; want 0, nil", got, err)
	}

	start := postgres.SessionStart{
		AttemptID:   "att_one",
		NamespaceID: ns.ID,
		RunID:       runID,
		NodeRunID:   nodeRunID,
		NodeKey:     "a",
		ActorRef:    "actor://company/analyzer",
	}
	if err := s.RecordSessionStart(ctx, start); err != nil {
		t.Fatalf("RecordSessionStart: %v", err)
	}
	// Same protocol attempt id: the same session, recorded again.
	if err := s.RecordSessionStart(ctx, start); err != nil {
		t.Fatalf("RecordSessionStart (repeat): %v", err)
	}
	if got, err := s.RunSessionStarts(ctx, runID); err != nil || got != 1 {
		t.Fatalf("RunSessionStarts after a repeated record = %d, %v; want 1, nil", got, err)
	}

	second := start
	second.AttemptID = "att_two"
	if err := s.RecordSessionStart(ctx, second); err != nil {
		t.Fatalf("RecordSessionStart (second): %v", err)
	}
	if got, err := s.RunSessionStarts(ctx, runID); err != nil || got != 2 {
		t.Fatalf("RunSessionStarts after a second cold start = %d, %v; want 2, nil", got, err)
	}

	// Another run's sessions are not this run's.
	otherRun, _, _, _ := seedRun(t, es, ns.ID, "a")
	if got, err := s.RunSessionStarts(ctx, otherRun); err != nil || got != 0 {
		t.Fatalf("RunSessionStarts for an unrelated run = %d, %v; want 0, nil", got, err)
	}
}
