package worker

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// A parked (async) completion must map the gate vocabulary's exit codes
// exactly as a synchronous one does. Before the fix this test pins, the
// async contract dropped ExitCodeOutcomes, so a gate node's exit 2 became a
// technical failure instead of measurement_incomplete — found live by the
// t8 combining-loop demonstration, where the same node routed correctly on
// the synchronous path and broke only when the operation parked.
func TestAsyncCompletionKeepsTheGateExitCodeMap(t *testing.T) {
	w := &Worker{opts: Options{CodeRunnerName: "test", CodeRunnerRevision: "sha256:" + sixtyFourHex}}
	node := &nodeSpec{
		ID:   "gate",
		Kind: "code",
		Outcomes: []string{
			"gates_passed", "changes_required", "measurement_incomplete",
		},
	}
	exit := 2
	completion, err := w.runnerCompletionFor(node, runners.Result{
		OperationID: "att_test",
		State:       runners.StateCompleted,
		Exit:        &runners.Exit{Code: &exit},
	})
	if err != nil {
		t.Fatalf("runnerCompletionFor: %v", err)
	}
	if completion.Outcome != "measurement_incomplete" {
		t.Fatalf("outcome = %q (tech %q), want measurement_incomplete — the async path dropped the exit-code map",
			completion.Outcome, completion.TechStatus)
	}
}

const sixtyFourHex = "ea34a34114bce926562cfc9027f68d1f3036e9f70b2cc16ed23b807141679a7f"
