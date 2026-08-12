package actors_test

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/actors"
)

// Usage.ToEngine is the one seam that lets internal/engine persist the §13.2
// telemetry block without importing internal/actors (actors already imports
// engine, so the reverse would cycle). Both completion paths --
// internal/worker/dispatch.go's synchronous completeFromResult and this
// package's own async commitTerminal -- convert through it, so its
// nil-in/nil-out and independent-nullability behavior is what both paths
// rest on.
func TestUsageToEngineConvertsNil(t *testing.T) {
	var u *actors.Usage
	if got := u.ToEngine(); got != nil {
		t.Errorf("ToEngine() on a nil Usage = %+v, want nil", got)
	}
}

func TestUsageToEngineCopiesFieldsAndPointerNullability(t *testing.T) {
	cost := 1.25
	currency := "USD"

	u := &actors.Usage{InputTokens: 7, OutputTokens: 9, Cost: &cost, Currency: &currency}
	got := u.ToEngine()
	if got == nil {
		t.Fatal("ToEngine() = nil, want a converted block")
	}
	if got.InputTokens != 7 || got.OutputTokens != 9 {
		t.Errorf("tokens = %d/%d, want 7/9", got.InputTokens, got.OutputTokens)
	}
	if got.Cost == nil || *got.Cost != cost {
		t.Errorf("cost = %v, want %v", got.Cost, cost)
	}
	if got.Currency == nil || *got.Currency != currency {
		t.Errorf("currency = %v, want %v", got.Currency, currency)
	}

	priced := &actors.Usage{InputTokens: 1, OutputTokens: 2}
	gotUnpriced := priced.ToEngine()
	if gotUnpriced == nil {
		t.Fatal("ToEngine() = nil, want a converted block")
	}
	if gotUnpriced.Cost != nil {
		t.Errorf("cost = %v, want nil (not reported, not fabricated)", *gotUnpriced.Cost)
	}
	if gotUnpriced.Currency != nil {
		t.Errorf("currency = %v, want nil", *gotUnpriced.Currency)
	}
}
