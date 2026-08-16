package runners_test

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/runners"
)

// A merge gate is the case that broke the two-port assumption (task t16,
// issue #101). Its exit status carries THREE domain answers, not two: the
// gates passed, a threshold was missed, or the measurement never happened.
// Collapsing the last two into "nonzero" is exactly the false green the gate
// exists to prevent — "we could not measure the Go suite here" would be
// routed down the same edge as "the Go suite failed", and a repair would be
// asked for a defect nobody observed.

func gateContract() runners.NodeContract {
	c := testContract()
	c.NodeID = "merge-gate"
	c.SuccessOutcome = "gates_passed"
	c.FailureOutcome = ""
	c.ExitCodeOutcomes = map[int]string{
		0: "gates_passed",
		1: "changes_required",
		2: "measurement_incomplete",
	}
	return c
}

// TestExitCodeOutcomesRouteEachDeclaredCodeToItsOwnOutcome is criterion 2's
// mechanical half: three declared domain outcomes, three exit codes, three
// different edges followed — and every one of them a SUCCEEDED attempt,
// because a threshold miss is a domain answer and not an engine failure
// (PRD §3.4).
func TestExitCodeOutcomesRouteEachDeclaredCodeToItsOwnOutcome(t *testing.T) {
	for code, want := range map[int]string{
		0: "gates_passed",
		1: "changes_required",
		2: "measurement_incomplete",
	} {
		completion, err := runners.BuildCompletion(lambdaShapedResult(intPtr(code)), gateContract())
		if err != nil {
			t.Fatalf("BuildCompletion(exit %d): %v", code, err)
		}
		if completion.TechStatus != engine.StatusSucceeded {
			t.Errorf("exit %d: TechStatus = %q, want succeeded — a domain answer is not an engine failure",
				code, completion.TechStatus)
		}
		if completion.Outcome != want {
			t.Errorf("exit %d: Outcome = %q, want %q", code, completion.Outcome, want)
		}
	}
}

// TestExitCodeOutcomesOutrankTheSuccessFailurePair proves the table is
// consulted first. A node that declares both must not have exit 1 silently
// answered by FailureOutcome, which would route a missed threshold and an
// unmeasured gate to the same place.
func TestExitCodeOutcomesOutrankTheSuccessFailurePair(t *testing.T) {
	contract := gateContract()
	contract.FailureOutcome = "failed"

	completion, err := runners.BuildCompletion(lambdaShapedResult(intPtr(2)), contract)
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	if completion.Outcome != "measurement_incomplete" {
		t.Errorf("Outcome = %q, want measurement_incomplete — the declared table outranks the fallback pair",
			completion.Outcome)
	}
}

// TestUndeclaredExitCodeIsATechnicalFailure is the honest floor. The gate
// program publishes exactly three exit codes; anything else is the tool
// crashing, and a crash produced no trustworthy domain measurement to route.
// Absorbing it into `changes_required` would report a threshold miss nobody
// measured.
func TestUndeclaredExitCodeIsATechnicalFailure(t *testing.T) {
	completion, err := runners.BuildCompletion(lambdaShapedResult(intPtr(127)), gateContract())
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	if completion.TechStatus != engine.StatusFailed {
		t.Errorf("TechStatus = %q, want failed", completion.TechStatus)
	}
	if completion.Outcome != "" {
		t.Errorf("Outcome = %q, want no domain outcome at all", completion.Outcome)
	}
}

// TestNullExitStaysUnroutableEvenWithATable: a runner with no exit code to
// report has nothing to look up, and inventing 0 is the fabrication this
// whole package exists to prevent.
func TestNullExitStaysUnroutableEvenWithATable(t *testing.T) {
	completion, err := runners.BuildCompletion(lambdaShapedResult(nil), gateContract())
	if err != nil {
		t.Fatalf("BuildCompletion: %v", err)
	}
	if completion.TechStatus != engine.StatusFailed || completion.Outcome != "" {
		t.Errorf("(%q, %q), want (failed, \"\")", completion.TechStatus, completion.Outcome)
	}
}
