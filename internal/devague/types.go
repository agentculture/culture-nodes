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

// planShow is `devague plan show --json` -- devague/plan.py's
// `to_dict(plan)`, i.e. `dataclasses.asdict(Plan)` verbatim. Unlike
// planWaves, this is the FAITHFUL view: every task carries devague's own
// per-task status (proposed/confirmed/rejected) and its real `deps` (task
// ids it actually depends on), not a wave layering. ParsePlanShow/
// MapPlanShow are pinned to this view specifically so the round-trip does
// not degrade to planWaves' coarser one; see plan_show.go's doc comment.
//
// Fields devague emits that no Parse/Map function here reads (schema_version,
// created, updated, targets, risks) are declared anyway, for two reasons:
// targets is Phase-0 read by nothing yet but is the coverage-target list a
// later reconciliation pass would need, and leaving it undeclared would
// silently discard it from anyone decoding into this struct by copy; risks
// genuinely has no consumer today and could be dropped, but is kept beside
// targets for the same "declare what devague sends, translate what this
// package uses" convention record.go's own comment states for this package.
type planShow struct {
	Slug      string           `json:"slug"`
	Title     string           `json:"title"`
	FrameSlug string           `json:"frame_slug"`
	Status    string           `json:"status"` // drafting | converged | exported (devague/plan.py Plan.status)
	Tasks     []planShowTask   `json:"tasks"`
	Targets   []planShowTarget `json:"targets"`
}

// planShowTask is one value of planShow.Tasks -- devague/plan.py's Task
// dataclass, asdict'd. TASK_STATUSES there is exactly
// ("proposed", "confirmed", "rejected"), the same vocabulary
// frameClaim.Status uses for a claim, which is what lets plan_show.go reuse
// claims.go's claimOriginKind/reviewForClaimStatus helpers unchanged.
type planShowTask struct {
	ID                 string   `json:"id"`
	Summary            string   `json:"summary"`
	Origin             string   `json:"origin"` // user | llm
	Status             string   `json:"status"` // proposed | confirmed | rejected
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Deps               []string `json:"deps"` // real per-task dependency edges: other task ids
	Covers             []string `json:"covers"`
	Instruction        string   `json:"instruction"`
}

// planShowTarget is one value of planShow.Targets -- a coverage target
// devague derived from the source frame (devague/plan.py's CoverageTarget).
// Declared but not read by any function in this package yet (see planShow's
// doc comment).
type planShowTarget struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// deliveryFile is the real on-disk shape of `.devague/deliveries/<plan
// slug>.json` -- devague/delivery.py's `to_dict(Delivery)`, i.e.
// `dataclasses.asdict(Delivery)` verbatim, exactly as `devague deviate`
// persists it (devague/delivery_store.py). This is deliberately NOT
// `devague deviate --list --json`'s shape: that CLI view's `_record_dict`
// renames `task_ref` to `task` and drops `schema_version`/`created`/
// `updated` (devague/cli/_commands/deviate.py), which would make this
// package's decode target diverge from the file this repo's own
// .devague/deliveries/*.json fixtures actually are. Reading the file shape
// directly (as ParseDeviations/MapDeviations do) means one struct serves
// both a live devague checkout's delivery file and a committed fixture like
// .devague/deliveries/economy-discord-graphs.json unchanged.
type deliveryFile struct {
	PlanSlug   string              `json:"plan_slug"`
	Deviations []deliveryDeviation `json:"deviations"`
}

// deliveryDeviation is one value of deliveryFile.Deviations -- devague/
// delivery.py's DeviationRecord dataclass, asdict'd. DEVIATION_STATUSES
// there is ("proposed", "approved", "rejected") -- note "approved", not
// "confirmed": a deviation and a claim share the same three-state shape
// (an llm-origin proposal a human must ratify; a user-origin one that
// auto-ratifies on capture) but devague spells the ratified state
// differently for the two record kinds, which is why deviations.go needs
// its own reviewForDeviationStatus rather than reusing claims.go's.
type deliveryDeviation struct {
	ID             string   `json:"id"`
	What           string   `json:"what"`
	TaskRef        string   `json:"task_ref"`
	Reason         string   `json:"reason"`
	Affects        []string `json:"affects"`
	Origin         string   `json:"origin"` // user | llm
	Status         string   `json:"status"` // proposed | approved | rejected
	Classification string   `json:"classification"`
}
