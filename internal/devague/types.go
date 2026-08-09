package devague

// The types below are the subset of each devague CLI --json shape this
// package reads. Fields devague emits that no Map* function uses (frame
// open_vagueness, plan risks, ...) are simply not declared here;
// encoding/json ignores what a struct does not name.

// frameShow is `devague show --json` (PRD §9.11: frame claims).
type frameShow struct {
	Slug   string       `json:"slug"`
	Title  string       `json:"title"`
	Claims []frameClaim `json:"claims"`
}

// frameClaim is one entry of frameShow.Claims.
type frameClaim struct {
	ID                string             `json:"id"`
	Kind              string             `json:"kind"`
	Text              string             `json:"text"`
	Origin            string             `json:"origin"`
	Status            string             `json:"status"`
	Instruction       string             `json:"instruction"`
	HonestyConditions []honestyCondition `json:"honesty_conditions"`
}

// honestyCondition is one entry of frameClaim.HonestyConditions.
type honestyCondition struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Status string `json:"status"`
}

// planWaves is `devague plan waves --json`.
//
// Empirically (verified against a real devague binary; see
// testdata/README.md), this view carries only wave layering plus each
// active task's working contract — summary, instruction, acceptance
// criteria, covers. It does not carry devague's own proposed/confirmed task
// status or explicit per-task dependency edges; MapPlanWaves's doc comment
// explains how this package derives ledger fields anyway from what is
// actually here.
type planWaves struct {
	Plan  string              `json:"plan"`
	Waves [][]string          `json:"waves"`
	Tasks map[string]planTask `json:"tasks"`
}

// planTask is one value of planWaves.Tasks.
type planTask struct {
	Summary            string   `json:"summary"`
	Instruction        string   `json:"instruction"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Covers             []string `json:"covers"`
}

// planDeliverables is `devague plan deliverables --json`.
type planDeliverables struct {
	Plan          string   `json:"plan"`
	Converged     bool     `json:"converged"`
	SuccessSignal []string `json:"success_signal"`
}
