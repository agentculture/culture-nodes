package runners_test

import (
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/runners"
)

// resultWithExit returns a minimal completed result whose exit code is code,
// with the exit-status observation's Measured flag set as requested. Real
// runners disagree on this: headspace-cli watches the container's own wait
// status (measured), while a Lambda-shaped adapter can only relay what the
// function payload claims about its own exit (not measured) — see
// dispatch_test.go's lambdaShapedResult.
func resultWithExit(code int, measured bool) runners.Result {
	res := minimalResult()
	res.Exit = &runners.Exit{Code: &code}
	res.Observations.ExitStatus = runners.Observation{Measured: measured, Complete: measured}
	return res
}

// resultWithChanges returns a minimal completed result whose workspace-diff
// completeness is complete, with the changed-paths observation's Measured
// flag set as requested.
func resultWithChanges(complete, measured bool) runners.Result {
	res := minimalResult()
	res.Changes = runners.Changes{Complete: complete}
	res.Observations.ChangedPaths = runners.Observation{Measured: measured, Complete: measured}
	return res
}

func req(fields map[string]any) map[string]any { return fields }

func TestEvaluateAcceptanceProcessExit(t *testing.T) {
	cases := []struct {
		name        string
		requirement map[string]any
		res         runners.Result
		wantPassed  bool
		wantEval    bool
	}{
		{
			name:        "measured exit matches",
			requirement: req(map[string]any{"kind": "process_exit", "equals": 0.0}),
			res:         resultWithExit(0, true),
			wantPassed:  true,
			wantEval:    true,
		},
		{
			name:        "measured exit does not match",
			requirement: req(map[string]any{"kind": "process_exit", "equals": 0.0}),
			res:         resultWithExit(1, true),
			wantPassed:  false,
			wantEval:    true,
		},
		{
			name:        "unmeasured exit is not honestly checkable, even if it happens to match",
			requirement: req(map[string]any{"kind": "process_exit", "equals": 0.0}),
			res:         resultWithExit(0, false),
			wantPassed:  false,
			wantEval:    false,
		},
		{
			name:        "missing equals field",
			requirement: req(map[string]any{"kind": "process_exit"}),
			res:         resultWithExit(0, true),
			wantPassed:  false,
			wantEval:    false,
		},
		{
			name:        "equals field of the wrong type",
			requirement: req(map[string]any{"kind": "process_exit", "equals": "zero"}),
			res:         resultWithExit(0, true),
			wantPassed:  false,
			wantEval:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := runners.EvaluateAcceptance([]map[string]any{tc.requirement}, tc.res)
			if len(verdict.Checks) != 1 {
				t.Fatalf("checks = %+v, want exactly one", verdict.Checks)
			}
			got := verdict.Checks[0]
			if got.Evaluated != tc.wantEval {
				t.Errorf("evaluated = %v, want %v (reason: %s)", got.Evaluated, tc.wantEval, got.Reason)
			}
			if got.Passed != tc.wantPassed {
				t.Errorf("passed = %v, want %v (reason: %s)", got.Passed, tc.wantPassed, got.Reason)
			}
			if got.Reason == "" {
				t.Error("check carries no reason; a mechanical verdict must say why")
			}
			if verdict.Passed != tc.wantPassed {
				t.Errorf("overall verdict.Passed = %v, want %v", verdict.Passed, tc.wantPassed)
			}
		})
	}
}

func TestEvaluateAcceptanceWorkspaceDiff(t *testing.T) {
	cases := []struct {
		name        string
		requirement map[string]any
		res         runners.Result
		wantPassed  bool
		wantEval    bool
	}{
		{
			name:        "measured and complete as required",
			requirement: req(map[string]any{"kind": "workspace_diff", "complete": true}),
			res:         resultWithChanges(true, true),
			wantPassed:  true,
			wantEval:    true,
		},
		{
			name:        "measured but not complete",
			requirement: req(map[string]any{"kind": "workspace_diff", "complete": true}),
			res:         resultWithChanges(false, true),
			wantPassed:  false,
			wantEval:    true,
		},
		{
			name:        "unmeasured — this is the real headspace-cli 0.11.0 shape (docs/acceptance.md criterion 12)",
			requirement: req(map[string]any{"kind": "workspace_diff", "complete": true}),
			res:         resultWithChanges(false, false),
			wantPassed:  false,
			wantEval:    false,
		},
		{
			name:        "missing complete field",
			requirement: req(map[string]any{"kind": "workspace_diff"}),
			res:         resultWithChanges(true, true),
			wantPassed:  false,
			wantEval:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := runners.EvaluateAcceptance([]map[string]any{tc.requirement}, tc.res)
			if len(verdict.Checks) != 1 {
				t.Fatalf("checks = %+v, want exactly one", verdict.Checks)
			}
			got := verdict.Checks[0]
			if got.Evaluated != tc.wantEval {
				t.Errorf("evaluated = %v, want %v (reason: %s)", got.Evaluated, tc.wantEval, got.Reason)
			}
			if got.Passed != tc.wantPassed {
				t.Errorf("passed = %v, want %v (reason: %s)", got.Passed, tc.wantPassed, got.Reason)
			}
		})
	}
}

// TestEvaluateAcceptanceUnknownKindFailsHonestly is the "check payloads stay
// open until the check registry lands" schema comment
// (schemas/workflow/workflow.schema.json) made concrete at runtime: a kind
// this build cannot mechanically evaluate is reported as unevaluated and
// therefore not passed — never silently skipped, and never assumed to pass.
func TestEvaluateAcceptanceUnknownKindFailsHonestly(t *testing.T) {
	for _, kind := range []string{"schema_valid", "artifact_digest", "dependency_delta",
		"parity_fixtures", "changed_paths_within_policy", "claims_confirmed", "no_blocking_questions", "", "not_even_a_real_kind"} {
		t.Run(kind, func(t *testing.T) {
			verdict := runners.EvaluateAcceptance([]map[string]any{{"kind": kind}}, minimalResult())
			if verdict.Passed {
				t.Fatalf("an unrecognised kind %q must never pass", kind)
			}
			if len(verdict.Checks) != 1 || verdict.Checks[0].Evaluated {
				t.Fatalf("checks = %+v, want exactly one unevaluated check", verdict.Checks)
			}
			if !strings.Contains(verdict.Checks[0].Reason, "no mechanical evaluator") {
				t.Errorf("reason = %q, want it to say this build has no evaluator for the kind", verdict.Checks[0].Reason)
			}
		})
	}
}

// TestEvaluateAcceptanceRequiresEveryCheckToPass mirrors the PRD §11.1
// reference node's own acceptance block: several requirements, evaluated
// independently, ANDed into one overall verdict.
func TestEvaluateAcceptanceRequiresEveryCheckToPass(t *testing.T) {
	res := resultWithExit(0, true)
	res.Changes = runners.Changes{Complete: true}
	res.Observations.ChangedPaths = runners.Observation{Measured: true, Complete: true}

	allPass := runners.EvaluateAcceptance([]map[string]any{
		{"kind": "process_exit", "equals": 0.0},
		{"kind": "workspace_diff", "complete": true},
	}, res)
	if !allPass.Passed {
		t.Fatalf("verdict = %+v, want Passed when every requirement passes", allPass)
	}
	if len(allPass.Checks) != 2 {
		t.Fatalf("checks = %+v, want 2", allPass.Checks)
	}

	onePasses := resultWithExit(1, true)
	mixed := runners.EvaluateAcceptance([]map[string]any{
		{"kind": "process_exit", "equals": 0.0},
		{"kind": "schema_valid"},
	}, onePasses)
	if mixed.Passed {
		t.Fatal("verdict.Passed = true, want false — one requirement failed and one could not even be evaluated")
	}
}

// TestEvaluateAcceptanceEmptyRequiresPasses covers the degenerate input: no
// requirements at all is vacuously satisfied. The workflow schema requires
// minItems: 1 on requires, so this is a defensive case rather than one the
// compiler would ever admit — EvaluateAcceptance is still a pure function
// over whatever slice it is handed and must not panic on it.
func TestEvaluateAcceptanceEmptyRequiresPasses(t *testing.T) {
	verdict := runners.EvaluateAcceptance(nil, minimalResult())
	if !verdict.Passed {
		t.Errorf("verdict.Passed = false for an empty requires list, want true")
	}
	if len(verdict.Checks) != 0 {
		t.Errorf("checks = %+v, want none", verdict.Checks)
	}
}

// TestMechanicallyEvaluableChecks documents exactly which of
// internal/compiler/vocabulary.go's nine acceptanceKinds this package can
// mechanically evaluate today — a strict subset, kept in sync by hand
// because the two packages do not import each other (compiler has no
// runtime dependency on the boundary that produces a Result).
func TestMechanicallyEvaluableChecks(t *testing.T) {
	want := map[runners.AcceptanceCheckKind]bool{
		runners.CheckProcessExit:   true,
		runners.CheckWorkspaceDiff: true,
	}
	if len(runners.MechanicallyEvaluableChecks) != len(want) {
		t.Fatalf("MechanicallyEvaluableChecks = %v, want exactly %v", runners.MechanicallyEvaluableChecks, want)
	}
	for k := range want {
		if !runners.MechanicallyEvaluableChecks[k] {
			t.Errorf("MechanicallyEvaluableChecks is missing %q", k)
		}
	}
}
