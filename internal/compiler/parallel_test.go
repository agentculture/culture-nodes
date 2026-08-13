package compiler

import (
	"encoding/json"
	"testing"
)

// The positive control for the split/join authoring surface (issue #43,
// parallel-tokens design §8). The deliberate-error fixtures live in
// diagnostics_test.go; this file pins what a CLEAN split/join workflow
// compiles to, because every structural check is only as trustworthy as the
// definition it is willing to accept.

func TestParallelJoinWorkflowCompilesCleanly(t *testing.T) {
	compiled, diags := compileFixture(t, "parallel-join-ok.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatalf("the split/join fixture did not compile: %s", renderDiagnostics(diags))
	}
	// No warnings either: a split/join graph is not inherently suspicious,
	// and a warning here would train authors to ignore the level.
	if len(diags) != 0 {
		t.Errorf("split/join fixture produced diagnostics, want none: %s", renderDiagnostics(diags))
	}
}

// TestParallelJoinNormalizedIR pins the three things the engine reads out of
// the IR for a split/join graph: the kind-implied outcomes (which is how
// `fan.split` and `gather.joined` edges validate at all), the join policy
// block carried verbatim, and the parallel-token limit the fan-out bound is
// enforced against.
func TestParallelJoinNormalizedIR(t *testing.T) {
	compiled, diags := compileFixture(t, "parallel-join-ok.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatalf("the split/join fixture did not compile: %s", renderDiagnostics(diags))
	}

	var ir map[string]any
	if err := json.Unmarshal(compiled.Normalized, &ir); err != nil {
		t.Fatalf("normalized IR is not valid JSON: %v", err)
	}
	spec := ir["spec"].(map[string]any)
	nodes := spec["nodes"].(map[string]any)

	fan, ok := nodes["fan"].(map[string]any)
	if !ok {
		t.Fatal("normalized IR has no node \"fan\"")
	}
	assertOutcomes(t, "fan", fan, []string{"split"})
	if _, carries := fan["join"]; carries {
		t.Error("a parallel node carries a join block in the normalized IR")
	}

	gather, ok := nodes["gather"].(map[string]any)
	if !ok {
		t.Fatal("normalized IR has no node \"gather\"")
	}
	assertOutcomes(t, "gather", gather, []string{"joined"})
	join, ok := gather["join"].(map[string]any)
	if !ok {
		t.Fatal("the join node carries no join block in the normalized IR")
	}
	if join["policy"] != JoinPolicyAll {
		t.Errorf("join.policy = %v, want %q", join["policy"], JoinPolicyAll)
	}
	if _, carries := join["quorum"]; carries {
		t.Error("an `all` policy carried a quorum value into the IR; an omitted quorum must round-trip as omitted")
	}

	limits := spec["limits"].(map[string]any)
	if got := limits["maxParallelTokens"]; got != float64(4) {
		t.Errorf("limits.maxParallelTokens = %v, want 4 — the fan-out bound reads this", got)
	}
}

// TestDefaultMaxParallelTokensRefusesSplitsByDefault pins the opt-in: a
// workflow that never declares maxParallelTokens gets 1, so a split in it
// would be refused at runtime as a bound failure (design §5.1). Parallelism
// is something an author asks for, never something a definition acquires by
// adding a node kind.
func TestDefaultMaxParallelTokensRefusesSplitsByDefault(t *testing.T) {
	if DefaultMaxParallelTokens != 1 {
		t.Fatalf("DefaultMaxParallelTokens = %d, want 1", DefaultMaxParallelTokens)
	}
	compiled, diags := compileFixture(t, "minimal.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatalf("the minimal fixture did not compile: %s", renderDiagnostics(diags))
	}
	var ir map[string]any
	if err := json.Unmarshal(compiled.Normalized, &ir); err != nil {
		t.Fatalf("normalized IR is not valid JSON: %v", err)
	}
	limits := ir["spec"].(map[string]any)["limits"].(map[string]any)
	if got := limits["maxParallelTokens"]; got != float64(1) {
		t.Errorf("an undeclared maxParallelTokens expanded to %v, want 1", got)
	}
}

func assertOutcomes(t *testing.T, nodeID string, node map[string]any, want []string) {
	t.Helper()
	raw, ok := node["outcomes"].([]any)
	if !ok {
		t.Fatalf("node %q carries no resolved outcomes in the normalized IR", nodeID)
	}
	if len(raw) != len(want) {
		t.Fatalf("node %q outcomes = %v, want %v", nodeID, raw, want)
	}
	for i := range want {
		if raw[i] != want[i] {
			t.Fatalf("node %q outcomes = %v, want %v", nodeID, raw, want)
		}
	}
}
