package compiler

import (
	"encoding/json"
	"strings"
	"testing"
)

// The declared economic budget (task t11, spec claim c6 / honesty h5).
//
// PRD §9.7 already lists "optional agent token or cost budget" beside the
// loop bounds. These tests pin the three things that make it usable: it
// reaches the IR (so a run pins it with everything else in the digest), it is
// never invented where the author declared none, and a value that cannot mean
// one thing is refused rather than guessed at.

func TestBudgetReachesTheNormalizedIR(t *testing.T) {
	compiled, diags := compileFixture(t, "budget-ok.workflow.yaml", FormatYAML)
	for _, d := range diags {
		if d.Level == LevelError {
			t.Fatalf("unexpected error diagnostic: %s %s: %s", d.Path, d.Code, d.Message)
		}
	}
	if compiled == nil {
		t.Fatal("Compile returned no CompiledWorkflow for a workflow with a declared budget")
	}

	if compiled.IR.Spec.Budget == nil {
		t.Fatal("IR carries no budget; a run pins the IR, so an unpinned budget is a budget the run never agreed to")
	}
	if got := compiled.IR.Spec.Budget.MaxSessions; got == nil || *got != 3 {
		t.Errorf("IR budget.maxSessions = %v, want 3", got)
	}
	if got := compiled.IR.Spec.Budget.MaxUncachedInput; got == nil || *got != 500000 {
		t.Errorf("IR budget.maxUncachedInput = %v, want 500000", got)
	}

	// It must survive canonicalisation too: the digest addresses these bytes.
	var normalized struct {
		Spec struct {
			Budget *struct {
				MaxSessions      *int   `json:"maxSessions"`
				MaxUncachedInput *int64 `json:"maxUncachedInput"`
			} `json:"budget"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(compiled.Normalized, &normalized); err != nil {
		t.Fatalf("decode normalized IR: %v", err)
	}
	if normalized.Spec.Budget == nil || normalized.Spec.Budget.MaxSessions == nil || *normalized.Spec.Budget.MaxSessions != 3 {
		t.Errorf("normalized JSON budget = %+v, want maxSessions 3", normalized.Spec.Budget)
	}
}

// A budget is never defaulted. `limits` expands defaults because an unbounded
// loop is a bug; an unstated budget is a statement that this workflow is not
// economically bounded, and inventing a ceiling would refuse work nobody
// restricted.
func TestAbsentBudgetStaysAbsent(t *testing.T) {
	compiled, diags := compileFixture(t, "minimal.workflow.yaml", FormatYAML)
	for _, d := range diags {
		if d.Level == LevelError {
			t.Fatalf("unexpected error diagnostic: %s %s: %s", d.Path, d.Code, d.Message)
		}
	}
	if compiled.IR.Spec.Budget != nil {
		t.Errorf("IR budget = %+v for a workflow that declares none, want nil: the compiler must not invent an economic ceiling",
			compiled.IR.Spec.Budget)
	}
	if got := string(compiled.Normalized); strings.Contains(got, `"budget"`) {
		t.Errorf("normalized JSON mentions budget for a workflow that declares none: %s", got)
	}
}

// Zero and negative are refused at the policy level as well as by the schema
// — the second, independent no this compiler gives every value whose two
// plausible readings are opposites.
func TestNonPositiveBudgetIsRefused(t *testing.T) {
	compiled, diags := compileFixture(t, "err-budget-nonsense.workflow.yaml", FormatYAML)
	if compiled != nil {
		t.Fatal("a workflow declaring budget.maxSessions: 0 compiled; it must not")
	}

	want := map[string]bool{
		"/spec/budget/maxSessions":      false,
		"/spec/budget/maxUncachedInput": false,
	}
	for _, d := range diags {
		if d.Code == CodeBudgetNotPositive && d.Level == LevelError {
			want[d.Path] = true
		}
	}
	for path, found := range want {
		if !found {
			t.Errorf("no %s diagnostic at %s; diagnostics: %s", CodeBudgetNotPositive, path, formatDiags(diags))
		}
	}
}

// An edge from `budget_exhausted` compiles on the kinds the budget guards and
// nowhere else: the check lives at the actor-dispatch site, so a code node's
// refusal edge could never fire.
func TestBudgetExhaustedEdgeOnlyRoutesFromGuardedKinds(t *testing.T) {
	compiled, diags := compileFixture(t, "err-budget-edge-kind.workflow.yaml", FormatYAML)
	if compiled != nil {
		t.Fatal("an edge from a code node's budget_exhausted compiled; it can never fire")
	}
	found := false
	for _, d := range diags {
		if d.Code == CodeGraphOutcomeUndeclared && d.Level == LevelError {
			found = true
		}
	}
	if !found {
		t.Errorf("no %s diagnostic for a budget_exhausted edge on a code node; diagnostics: %s",
			CodeGraphOutcomeUndeclared, formatDiags(diags))
	}
}

func formatDiags(diags []Diagnostic) string {
	out := ""
	for _, d := range diags {
		out += "\n  " + string(d.Level) + " " + d.Path + " " + d.Code + ": " + d.Message
	}
	if out == "" {
		return "(none)"
	}
	return out
}
