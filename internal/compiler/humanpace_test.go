package compiler

import (
	"encoding/json"
	"testing"
	"time"
)

// Human-timescale deadlines are authorable per node (issue #38, spec claim
// c28, task t11). The async wait bound is the node's policy.timeout — see
// internal/worker.deadlineFor — so a human-actor node parked for days needs
// the compiler to carry a week-scale timeout through unclamped:
//
//   - the embedded workflow schema's duration pattern (^([0-9]+(ms|s|m|h))+$)
//     must accept hour-denominated week-scale literals — Compile validates the
//     document against that schema, so a clean compile IS the schema proof;
//   - checkDuration parses with time.ParseDuration, which likewise tops out
//     at the hour unit, so 168h (a week) and 336h (two weeks) are the
//     canonical spellings;
//   - the 900-second runner timeout cap (CodePolicyTimeoutOverCap) is a
//     code-node bound — checkRunnerCaps returns before it for every other
//     kind — and must not leak onto agent nodes.
func TestWeekScaleTimeoutsCompileCleanAndSurviveNormalization(t *testing.T) {
	compiled, diags := compileFixture(t, "humanpace.workflow.yaml", FormatYAML)
	if compiled == nil {
		t.Fatalf("humanpace fixture did not compile:\n%s", renderDiagnostics(diags))
	}
	for _, d := range diags {
		if d.Level == LevelError {
			t.Fatalf("week-scale durations produced an error diagnostic %s at %s: %s", d.Code, d.Path, d.Message)
		}
		if d.Code == CodePolicyTimeoutOverCap {
			t.Fatalf("the runner's %ds cap fired on an agent node: %s", RunnerMaxTimeoutSeconds, d.Message)
		}
	}

	// The normalized IR — the content-addressed document a run pins — must
	// carry the authored values verbatim, not a default or a clamp.
	var ir struct {
		Spec struct {
			Limits struct {
				MaxDuration string `json:"maxDuration"`
			} `json:"limits"`
			Nodes map[string]struct {
				Policy struct {
					Timeout string `json:"timeout"`
				} `json:"policy"`
			} `json:"nodes"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(compiled.Normalized, &ir); err != nil {
		t.Fatalf("normalized IR is not valid JSON: %v", err)
	}
	if got := ir.Spec.Nodes["review"].Policy.Timeout; got != "168h" {
		t.Errorf("normalized review timeout = %q, want the authored 168h", got)
	}
	if got := ir.Spec.Limits.MaxDuration; got != "336h" {
		t.Errorf("normalized maxDuration = %q, want the authored 336h", got)
	}

	// And the value parses to exactly the wall-clock span the author meant —
	// the same parse internal/worker's IR loader performs before computing
	// the dispatch deadline.
	parsed, err := time.ParseDuration(ir.Spec.Nodes["review"].Policy.Timeout)
	if err != nil {
		t.Fatalf("normalized timeout does not parse: %v", err)
	}
	if want := 7 * 24 * time.Hour; parsed != want {
		t.Errorf("parsed timeout = %s, want %s", parsed, want)
	}
}
