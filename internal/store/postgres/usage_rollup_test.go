package postgres_test

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// insertAttempt is a small test helper: it records one attempt on
// nodeRunID with attempt number n, the given status, and an optional Usage
// block (nil means "reported no usage"), inside its own transaction —
// mirroring exactly what a real completion writes (InsertAttempt), just
// without driving the full engine state machine, since these tests are
// about the *aggregation* the rollup performs, not retry scheduling itself
// (that is internal/engine/complete_test.go's job).
func insertAttempt(t *testing.T, es *postgres.EngineStore, nodeRunID string, n int, status engine.TechStatus, usage *engine.Usage) string {
	t.Helper()
	id := store.NewULID()
	err := es.InTx(context.Background(), func(ctx context.Context, tx engine.Tx) error {
		return tx.InsertAttempt(ctx, engine.Attempt{
			ID: id, NodeRunID: nodeRunID, Number: n, Status: status, Usage: usage,
		})
	})
	if err != nil {
		t.Fatalf("insert attempt %d on node run %s: %v", n, nodeRunID, err)
	}
	return id
}

func ptr[T any](v T) *T { return &v }

// TestNodeRunUsageSumsAllAttemptsIncludingFailed is the retry-burn test
// task t2's acceptance explicitly asks for: a node run with a failed
// attempt that still burned tokens, followed by a succeeded attempt that
// also burned tokens, must report BOTH attempts' tokens summed together —
// retry burn is real spend, not discardable technical noise.
func TestNodeRunUsageSumsAllAttemptsIncludingFailed(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()
	_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "a")

	insertAttempt(t, es, nodeRunID, 1, engine.StatusFailed, &engine.Usage{InputTokens: 100, OutputTokens: 50})
	insertAttempt(t, es, nodeRunID, 2, engine.StatusSucceeded, &engine.Usage{InputTokens: 40, OutputTokens: 20})

	rollup, err := es.NodeRunUsage(ctx, nodeRunID)
	if err != nil {
		t.Fatalf("NodeRunUsage: %v", err)
	}
	if rollup.InputTokens != 140 || rollup.OutputTokens != 70 {
		t.Fatalf("tokens = %d/%d, want 140/70 (failed attempt's usage must still be counted)", rollup.InputTokens, rollup.OutputTokens)
	}
	if rollup.AttemptsReported != 2 {
		t.Errorf("AttemptsReported = %d, want 2", rollup.AttemptsReported)
	}
	if rollup.AttemptsNotReported != 0 {
		t.Errorf("AttemptsNotReported = %d, want 0", rollup.AttemptsNotReported)
	}
}

// TestNodeRunUsageExcludesUnreportedFromSumsAndCountsSeparately proves an
// attempt with no Usage block at all contributes nothing to the token
// sums (never folded in as a zero) and is instead counted in
// AttemptsNotReported, distinct from the attempt(s) that did report.
func TestNodeRunUsageExcludesUnreportedFromSumsAndCountsSeparately(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()
	_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "a")

	insertAttempt(t, es, nodeRunID, 1, engine.StatusFailed, nil) // reported nothing
	insertAttempt(t, es, nodeRunID, 2, engine.StatusSucceeded, &engine.Usage{InputTokens: 10, OutputTokens: 5})

	rollup, err := es.NodeRunUsage(ctx, nodeRunID)
	if err != nil {
		t.Fatalf("NodeRunUsage: %v", err)
	}
	if rollup.InputTokens != 10 || rollup.OutputTokens != 5 {
		t.Fatalf("tokens = %d/%d, want 10/5 (the unreported attempt must contribute nothing)", rollup.InputTokens, rollup.OutputTokens)
	}
	if rollup.AttemptsReported != 1 {
		t.Errorf("AttemptsReported = %d, want 1", rollup.AttemptsReported)
	}
	if rollup.AttemptsNotReported != 1 {
		t.Errorf("AttemptsNotReported = %d, want 1", rollup.AttemptsNotReported)
	}
}

// TestNodeRunUsageNoAttemptsReportedIsDistinctFromZeroTokensReported proves
// the rollup can tell "nobody reported usage" (AttemptsReported == 0) apart
// from "an attempt reported usage and it happened to be zero tokens"
// (AttemptsReported == 1, InputTokens == 0) — task t2's acceptance
// criterion 2 in full: these must never collapse into the same shape.
func TestNodeRunUsageNoAttemptsReportedIsDistinctFromZeroTokensReported(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()

	t.Run("no attempts reported usage", func(t *testing.T) {
		_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "a")
		insertAttempt(t, es, nodeRunID, 1, engine.StatusFailed, nil)
		insertAttempt(t, es, nodeRunID, 2, engine.StatusCancelled, nil)

		rollup, err := es.NodeRunUsage(ctx, nodeRunID)
		if err != nil {
			t.Fatalf("NodeRunUsage: %v", err)
		}
		if rollup.AttemptsReported != 0 || rollup.AttemptsNotReported != 2 {
			t.Fatalf("reported/not_reported = %d/%d, want 0/2", rollup.AttemptsReported, rollup.AttemptsNotReported)
		}
		if rollup.InputTokens != 0 || rollup.OutputTokens != 0 {
			t.Fatalf("tokens = %d/%d, want 0/0 (sum of nothing)", rollup.InputTokens, rollup.OutputTokens)
		}
	})

	t.Run("zero tokens explicitly reported", func(t *testing.T) {
		_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "b")
		insertAttempt(t, es, nodeRunID, 1, engine.StatusSucceeded, &engine.Usage{InputTokens: 0, OutputTokens: 0})

		rollup, err := es.NodeRunUsage(ctx, nodeRunID)
		if err != nil {
			t.Fatalf("NodeRunUsage: %v", err)
		}
		if rollup.AttemptsReported != 1 || rollup.AttemptsNotReported != 0 {
			t.Fatalf("reported/not_reported = %d/%d, want 1/0 (an attempt DID report, it just reported zero)", rollup.AttemptsReported, rollup.AttemptsNotReported)
		}
		if rollup.InputTokens != 0 || rollup.OutputTokens != 0 {
			t.Fatalf("tokens = %d/%d, want 0/0", rollup.InputTokens, rollup.OutputTokens)
		}
	})
}

// TestNodeRunUsageNeverSumsCostAcrossCurrencies proves the rollup exposes
// per-currency sums rather than ever adding, say, USD to JPY, and that a
// cost reported with no currency lands in its own "" bucket rather than
// being dropped.
func TestNodeRunUsageNeverSumsCostAcrossCurrencies(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()

	t.Run("single coherent currency", func(t *testing.T) {
		_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "a")
		insertAttempt(t, es, nodeRunID, 1, engine.StatusSucceeded,
			&engine.Usage{InputTokens: 10, OutputTokens: 5, Cost: ptr(0.01), Currency: ptr("USD")})
		insertAttempt(t, es, nodeRunID, 2, engine.StatusFailed,
			&engine.Usage{InputTokens: 1, OutputTokens: 1, Cost: ptr(0.02), Currency: ptr("USD")})

		rollup, err := es.NodeRunUsage(ctx, nodeRunID)
		if err != nil {
			t.Fatalf("NodeRunUsage: %v", err)
		}
		if len(rollup.Cost) != 1 {
			t.Fatalf("Cost = %+v, want exactly one USD entry", rollup.Cost)
		}
		if rollup.Cost[0].Currency != "USD" || rollup.Cost[0].Cost < 0.0299 || rollup.Cost[0].Cost > 0.0301 {
			t.Fatalf("Cost[0] = %+v, want {USD ~0.03}", rollup.Cost[0])
		}
	})

	t.Run("mixed currencies stay separate, never summed", func(t *testing.T) {
		_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "b")
		insertAttempt(t, es, nodeRunID, 1, engine.StatusSucceeded,
			&engine.Usage{InputTokens: 10, OutputTokens: 5, Cost: ptr(1.0), Currency: ptr("USD")})
		insertAttempt(t, es, nodeRunID, 2, engine.StatusSucceeded,
			&engine.Usage{InputTokens: 10, OutputTokens: 5, Cost: ptr(100.0), Currency: ptr("JPY")})

		rollup, err := es.NodeRunUsage(ctx, nodeRunID)
		if err != nil {
			t.Fatalf("NodeRunUsage: %v", err)
		}
		if len(rollup.Cost) != 2 {
			t.Fatalf("Cost = %+v, want two entries (USD and JPY kept apart)", rollup.Cost)
		}
		byCurrency := map[string]float64{}
		for _, cc := range rollup.Cost {
			byCurrency[cc.Currency] = cc.Cost
		}
		if byCurrency["USD"] != 1.0 || byCurrency["JPY"] != 100.0 {
			t.Fatalf("byCurrency = %+v, want USD=1 JPY=100 (never added together)", byCurrency)
		}
	})

	t.Run("cost reported with no currency gets its own bucket", func(t *testing.T) {
		_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "c")
		insertAttempt(t, es, nodeRunID, 1, engine.StatusSucceeded,
			&engine.Usage{InputTokens: 10, OutputTokens: 5, Cost: ptr(2.5)}) // Cost set, Currency nil

		rollup, err := es.NodeRunUsage(ctx, nodeRunID)
		if err != nil {
			t.Fatalf("NodeRunUsage: %v", err)
		}
		if len(rollup.Cost) != 1 || rollup.Cost[0].Currency != "" || rollup.Cost[0].Cost != 2.5 {
			t.Fatalf("Cost = %+v, want one entry {\"\" 2.5}", rollup.Cost)
		}
	})

	t.Run("no attempt reported a cost at all", func(t *testing.T) {
		_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "d")
		insertAttempt(t, es, nodeRunID, 1, engine.StatusSucceeded, &engine.Usage{InputTokens: 10, OutputTokens: 5})

		rollup, err := es.NodeRunUsage(ctx, nodeRunID)
		if err != nil {
			t.Fatalf("NodeRunUsage: %v", err)
		}
		if len(rollup.Cost) != 0 {
			t.Fatalf("Cost = %+v, want empty (no attempt priced its work)", rollup.Cost)
		}
	})
}

// TestRunUsageSumsAcrossNodeRuns proves RunUsage aggregates every node
// run's attempts belonging to the run, not just one node run's — the
// run-level rollup task t2 exposes on run detail.
func TestRunUsageSumsAcrossNodeRuns(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()
	runID, tokenID, firstNodeRunID, _ := seedRun(t, es, ns.ID, "a")

	insertAttempt(t, es, firstNodeRunID, 1, engine.StatusFailed, &engine.Usage{InputTokens: 5, OutputTokens: 5})
	insertAttempt(t, es, firstNodeRunID, 2, engine.StatusSucceeded, &engine.Usage{InputTokens: 7, OutputTokens: 3})

	secondNodeRunID := store.NewULID()
	err := es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
		return tx.InsertNodeRun(ctx, engine.NodeRun{
			ID: secondNodeRunID, NamespaceID: ns.ID, RunID: runID, TokenID: tokenID,
			NodeID: "b", State: engine.NodeRunReady, VisitCount: 1,
		})
	})
	if err != nil {
		t.Fatalf("insert second node run: %v", err)
	}
	insertAttempt(t, es, secondNodeRunID, 1, engine.StatusSucceeded, &engine.Usage{InputTokens: 2, OutputTokens: 1})
	insertAttempt(t, es, secondNodeRunID, 2, engine.StatusCancelled, nil) // unreported, must not zero out the sum

	rollup, err := es.RunUsage(ctx, runID)
	if err != nil {
		t.Fatalf("RunUsage: %v", err)
	}
	if rollup.InputTokens != 14 || rollup.OutputTokens != 9 {
		t.Fatalf("tokens = %d/%d, want 14/9 (5+7+2 / 5+3+1 across both node runs)", rollup.InputTokens, rollup.OutputTokens)
	}
	if rollup.AttemptsReported != 3 {
		t.Errorf("AttemptsReported = %d, want 3", rollup.AttemptsReported)
	}
	if rollup.AttemptsNotReported != 1 {
		t.Errorf("AttemptsNotReported = %d, want 1", rollup.AttemptsNotReported)
	}

	// The first node run's own rollup must still equal just its own two
	// attempts -- RunUsage is not smuggled into NodeRunUsage's answer.
	firstOnly, err := es.NodeRunUsage(ctx, firstNodeRunID)
	if err != nil {
		t.Fatalf("NodeRunUsage: %v", err)
	}
	if firstOnly.InputTokens != 12 || firstOnly.OutputTokens != 8 {
		t.Fatalf("first node run tokens = %d/%d, want 12/8", firstOnly.InputTokens, firstOnly.OutputTokens)
	}
}

// TestNodeRunUsagesBatchMatchesPerNodeRunLookup proves the batched
// NodeRunUsages (used by the cross-run node-runs listing) returns exactly
// the same rollup per node run as calling NodeRunUsage individually, and
// that a node run with no attempts at all is simply absent from the map
// rather than causing an error.
func TestNodeRunUsagesBatchMatchesPerNodeRunLookup(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()

	_, _, withUsageID, _ := seedRun(t, es, ns.ID, "a")
	insertAttempt(t, es, withUsageID, 1, engine.StatusFailed, &engine.Usage{InputTokens: 3, OutputTokens: 1, Cost: ptr(0.5), Currency: ptr("USD")})
	insertAttempt(t, es, withUsageID, 2, engine.StatusSucceeded, &engine.Usage{InputTokens: 4, OutputTokens: 2, Cost: ptr(0.5), Currency: ptr("USD")})

	_, _, unreportedID, _ := seedRun(t, es, ns.ID, "b")
	insertAttempt(t, es, unreportedID, 1, engine.StatusFailed, nil)

	_, _, neverDispatchedID, _ := seedRun(t, es, ns.ID, "c")

	batch, err := es.NodeRunUsages(ctx, []string{withUsageID, unreportedID, neverDispatchedID})
	if err != nil {
		t.Fatalf("NodeRunUsages: %v", err)
	}

	withUsage := batch[withUsageID]
	if withUsage.InputTokens != 7 || withUsage.OutputTokens != 3 || withUsage.AttemptsReported != 2 {
		t.Fatalf("withUsage = %+v, want tokens 7/3 reported=2", withUsage)
	}
	if len(withUsage.Cost) != 1 || withUsage.Cost[0].Currency != "USD" || withUsage.Cost[0].Cost != 1.0 {
		t.Fatalf("withUsage.Cost = %+v, want [{USD 1.0}]", withUsage.Cost)
	}

	unreported := batch[unreportedID]
	if unreported.AttemptsReported != 0 || unreported.AttemptsNotReported != 1 {
		t.Fatalf("unreported = %+v, want reported=0 not_reported=1", unreported)
	}

	if _, ok := batch[neverDispatchedID]; ok {
		t.Fatalf("a node run with zero attempts must be absent from the batch map, got %+v", batch[neverDispatchedID])
	}

	// Cross-check against the single-id path.
	single, err := es.NodeRunUsage(ctx, withUsageID)
	if err != nil {
		t.Fatalf("NodeRunUsage: %v", err)
	}
	if single.InputTokens != withUsage.InputTokens || single.OutputTokens != withUsage.OutputTokens {
		t.Fatalf("batch and single-id rollups disagree: batch=%+v single=%+v", withUsage, single)
	}
}
