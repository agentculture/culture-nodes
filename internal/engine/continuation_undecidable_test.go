package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/google/cel-go/cel"
)

// Issues #95 and #105 are two symptoms of the same eight lines, so they are
// one fix in DecideContinuation with a test apiece here.
//
//   - #105: a continue.while that ERRORS returned the identical zero
//     ContinuationDecision a cleanly-false one did. Nothing downstream could
//     tell "the node said stop" from "nobody could evaluate the question",
//     and because the zero value carries no outcome, onExhausted never fired
//     either. The stop was silent by construction.
//   - #95: the scheduler bound node.state to the literal "incomplete", so the
//     canonical condition node.state == "incomplete" was true in every run for
//     every node — a predicate that compiled, evaluated, and decided nothing.
//     The fix is to bind the node run's real durable state; the half that
//     belongs HERE is that an UNKNOWN state must be undecidable rather than
//     silently answered, so no caller can ever fabricate one again by omission.
//
// Both cases now return ErrContinuationUndecidable. See
// internal/scheduler/scheduler.go for what the one caller does with it (it
// fails closed AND records the fact, rather than propagating an error that
// would retry the same deterministic failure every tick forever).

func mustGuard(t *testing.T, expression string) cel.Program {
	t.Helper()
	env, err := newCELEnv()
	if err != nil {
		t.Fatal(err)
	}
	program, err := compileGuard(env, expression)
	if err != nil {
		t.Fatalf("compile %q: %v", expression, err)
	}
	return program
}

// #105. A deliberately broken expression — it compiles (the CEL variables are
// dynamically typed), and fails at evaluation.
func TestContinuationEvaluationErrorIsNotAFalseCondition(t *testing.T) {
	n := &Node{Continue: &Continuation{
		While:       []cel.Program{mustGuard(t, `input.failed_gate_count > 0`)},
		Bounds:      ContinuationBounds{MaxContinuations: 3, MaxWallClock: 2 * time.Hour},
		OnExhausted: "needs_human",
	}}

	got, err := n.DecideContinuation(ContinuationState{
		NodeState: "incomplete", RemainingSessions: 2, Continuations: 1, WallClock: time.Hour,
	})
	if err == nil {
		t.Fatal("an errored continue.while returned no error: an evaluation that " +
			"produced no answer is being reported as the answer \"do not continue\"")
	}
	if !errors.Is(err, ErrContinuationUndecidable) {
		t.Fatalf("error = %v, want it to wrap ErrContinuationUndecidable", err)
	}
	if got.Continue {
		t.Fatalf("decision = %+v, want no continuation from an undecidable condition", got)
	}
	if !got.EngineFailure {
		t.Fatalf("decision = %+v, want EngineFailure set: a caller that ignores the "+
			"error must still not read this as a domain decision", got)
	}
	// The distinction the whole issue rests on: this must not look like the
	// ordinary, expected case. Same bounds, same call, a condition that
	// evaluates cleanly to false.
	stopper := &Node{Continue: &Continuation{
		While:       []cel.Program{mustGuard(t, `node.state == "incomplete"`)},
		Bounds:      ContinuationBounds{MaxContinuations: 3, MaxWallClock: 2 * time.Hour},
		OnExhausted: "needs_human",
	}}
	stop, stopErr := stopper.DecideContinuation(ContinuationState{NodeState: "complete"})
	if stopErr != nil {
		t.Fatalf("a cleanly false condition returned an error: %v", stopErr)
	}
	if stop == got {
		t.Fatalf("an undecidable condition and a false one both produced %+v — "+
			"indistinguishable, which is exactly #105", got)
	}
}

// #105, second half: a condition that evaluates cleanly to a NON-boolean is
// also a failure to decide, not a decision.
func TestContinuationNonBooleanConditionIsUndecidable(t *testing.T) {
	n := &Node{Continue: &Continuation{
		While:  []cel.Program{mustGuard(t, `budget.remaining_sessions`)},
		Bounds: ContinuationBounds{MaxWallClock: time.Hour},
	}}

	got, err := n.DecideContinuation(ContinuationState{NodeState: "incomplete", RemainingSessions: 2})
	if !errors.Is(err, ErrContinuationUndecidable) {
		t.Fatalf("decision = %+v, err = %v; want ErrContinuationUndecidable", got, err)
	}
}

// #95. The scheduler used to hand DecideContinuation a hardcoded
// NodeState: "incomplete". Nothing here could have caught that, because an
// empty NodeState evaluated just as quietly: `"" == "incomplete"` is false,
// and false is a legitimate answer. So a caller with no measured state got a
// decision that looked exactly like a node reporting it was done.
//
// Now an unmeasured state is ABSENT from the activation, so a condition that
// reads it says so.
func TestContinuationRefusesToDecideOnAnUnmeasuredNodeState(t *testing.T) {
	n := &Node{Continue: &Continuation{
		While:       []cel.Program{mustGuard(t, `node.state == "incomplete"`)},
		Bounds:      ContinuationBounds{MaxSessions: 4, MaxWallClock: 2 * time.Hour},
		OnExhausted: "needs_human",
	}}

	got, err := n.DecideContinuation(ContinuationState{RemainingSessions: 2, Sessions: 1})
	if err == nil {
		t.Fatalf("decision = %+v with no error: an unmeasured node.state was answered "+
			"rather than refused, so a caller can still fabricate one by omission", got)
	}
	if !errors.Is(err, ErrContinuationUndecidable) {
		t.Fatalf("error = %v, want it to wrap ErrContinuationUndecidable", err)
	}

	// A condition that does not read node.state is unaffected: the point is
	// that the engine stops answering questions about state it was not given,
	// not that it stops evaluating.
	budgetOnly := &Node{Continue: &Continuation{
		While:  []cel.Program{mustGuard(t, `budget.remaining_sessions > 0`)},
		Bounds: ContinuationBounds{MaxSessions: 4, MaxWallClock: 2 * time.Hour},
	}}
	decision, err := budgetOnly.DecideContinuation(ContinuationState{RemainingSessions: 2, Sessions: 1})
	if err != nil || !decision.Continue {
		t.Fatalf("budget-only condition: decision = %+v, err = %v; want a clean continuation",
			decision, err)
	}
}

// #95's other half: what a MEASURED state is. The scheduler reads
// node_runs.status; this is the mapping from that lifecycle onto the
// vocabulary `continue.while` conditions are written against.
func TestContinuationNodeStateMapsTheDurableNodeRunLifecycle(t *testing.T) {
	for _, tc := range []struct {
		status NodeRunState
		want   string
	}{
		{NodeRunRunning, "incomplete"},
		{NodeRunWaitingExternal, "incomplete"},
		{NodeRunWaitingHuman, "incomplete"},
		{NodeRunReady, "incomplete"},
		{NodeRunCompleted, "complete"},
		{NodeRunFailed, "complete"},
		{NodeRunCancelled, "complete"},
		// An unread row is not a state. It must stay empty so the caller
		// gets an undecidable condition instead of a plausible guess.
		{"", ""},
	} {
		if got := ContinuationNodeState(tc.status); got != tc.want {
			t.Errorf("ContinuationNodeState(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}
