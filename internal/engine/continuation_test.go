package engine

import (
	"testing"
	"time"

	"github.com/google/cel-go/cel"
)

func TestContinuationConditionAndIndependentBounds(t *testing.T) {
	env, err := newCELEnv()
	if err != nil {
		t.Fatal(err)
	}
	p1, err := compileGuard(env, `node.state == "incomplete"`)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := compileGuard(env, `budget.remaining_sessions > 0`)
	if err != nil {
		t.Fatal(err)
	}
	n := &Node{Continue: &Continuation{While: []cel.Program{p1, p2}, Bounds: ContinuationBounds{
		MaxContinuations: 3, MaxWallClock: 2 * time.Hour, MaxSessions: 4,
	}, OnExhausted: "needs_human"}}

	// The NodeState values below are what a caller MEASURED (see
	// ContinuationNodeState and issue #95); a caller that measured nothing
	// gets ErrContinuationUndecidable instead, which
	// continuation_undecidable_test.go covers.
	got, err := n.DecideContinuation(ContinuationState{NodeState: "incomplete", RemainingSessions: 2, Continuations: 1, Sessions: 2, WallClock: time.Hour})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Continue || got.Outcome != "" {
		t.Fatalf("decision = %+v, want continue", got)
	}

	// Deadline is intentionally absent from ContinuationState: technical
	// gate-retry/deadline exhaustion and continuation bounds are independent.
	got, err = n.DecideContinuation(ContinuationState{NodeState: "incomplete", RemainingSessions: 2, Continuations: 3, Sessions: 2, WallClock: time.Hour})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Continue || got.Outcome != "needs_human" || got.EngineFailure {
		t.Fatalf("decision = %+v, want routed domain exhaustion", got)
	}
	got, err = n.DecideContinuation(ContinuationState{NodeState: "complete", RemainingSessions: 2})
	if err != nil {
		t.Fatalf("a cleanly false condition must not be an error: %v", err)
	}
	if got.Continue || got.Outcome != "" || got.EngineFailure {
		t.Fatalf("false CEL condition = %+v, want stop without failure", got)
	}
}
