package api_test

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
)

func TestRunDetailExposesAttemptAttribution(t *testing.T) {
	f := newFixture(t)
	run, nodeRunID := createMinimalRun(t, f)
	model := "unknown:colleague-backend-cannot-report"
	cached, reasoning := int64(30), int64(12)
	cost, currency := 1.25, "USD"
	termination, continuation := "end_turn", "session://opaque"
	es, err := storepg.NewEngineStore(f.store, f.nsID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}
	err = es.InTx(context.Background(), func(ctx context.Context, tx engine.Tx) error {
		return tx.InsertAttempt(ctx, engine.Attempt{
			ID: store.NewULID(), NodeRunID: nodeRunID, Number: 1,
			Status: engine.StatusSucceeded,
			Usage: &engine.Usage{
				InputTokens: 100, OutputTokens: 40, Cost: &cost, Currency: &currency,
				CachedInputTokens: &cached, ReasoningTokens: &reasoning, Model: &model,
			},
			TerminationReason: &termination, ContinuationRef: &continuation,
		})
	})
	if err != nil {
		t.Fatalf("seed attributed attempt: %v", err)
	}

	attempt := getRunView(t, f, run.ID).NodeRuns[0].Attempts[0]
	if attempt.Usage == nil {
		t.Fatal("Usage = nil, want the reported per-attempt usage block")
	}
	if attempt.Usage.UsageModel == nil || *attempt.Usage.UsageModel != model {
		t.Fatalf("Usage.UsageModel = %v, want %q", attempt.Usage.UsageModel, model)
	}
	if attempt.Usage.InputTokens != 100 || attempt.Usage.OutputTokens != 40 {
		t.Errorf("Usage tokens = %d/%d, want 100/40", attempt.Usage.InputTokens, attempt.Usage.OutputTokens)
	}
	if attempt.TerminationReason != termination || attempt.ContinuationRef != continuation {
		t.Errorf("termination/continuation = %q/%q, want %q/%q", attempt.TerminationReason, attempt.ContinuationRef, termination, continuation)
	}
}
