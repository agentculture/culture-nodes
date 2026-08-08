package devague

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// MapPlanWaves maps `devague plan waves --json` onto ledger task records.
//
// # What the source actually carries
//
// Verified empirically against a real devague binary (testdata/README.md
// has the exact commands): this view's `tasks` map carries each active
// (non-rejected) task's summary, instruction, acceptance_criteria, and
// covers — and nothing else. It does not carry devague's own per-task
// proposed/confirmed status, and it does not carry explicit dependency
// edges; `waves` is a layering (wave 0, wave 1, ...), not an edge list.
// `devague plan show --json` does carry per-task status and deps, but that
// is a different view than the one this function is named for and pinned
// to, so this function does not read it.
//
// # status: ready
//
// task.schema.json's status enum is proposed/ready/claimed/.../cancelled —
// the same proposed/ready vocabulary the task description asks this
// function to bridge ("status proposed→ready per confirmed"). Because the
// source carries no per-task confirmed flag to key that bridge on, every
// task this function maps gets ledger status "ready": `plan waves` is the
// view an external operator reads to fan work out (see the
// assign-to-workforce skill, which fans out exactly this view's tasks to
// parallel agents), so a task appearing in it is being treated as accepted
// work, not as an as-yet-undecided proposal. This is a documented reading
// of what the source means, not a field-by-field translation of a status
// devague did not send — see testdata/README.md and the t25 delivery notes
// for the full reasoning and the alternative (leaving every task
// "proposed", which would make ReadyTasks vacuous for this source and
// wasn't chosen for that reason).
//
// # depends_on: the previous wave
//
// `dependency_waves` (the devague function that produces `waves`) places a
// task in wave i = 1 + max(wave of each of its real dependencies) — so by
// construction, every real dependency of a wave-i task has already
// completed by the end of wave i-1. Recording depends_on as "every task in
// the previous wave" is therefore not a lossy guess at edges devague did not
// send: it is the exact synchronisation guarantee a wave boundary means.
// It can name a wave-mate that task does not individually depend on (waves
// are coarser than edges), but it never misses a real dependency and never
// claims a task ready before a real dependency of it is.
func MapPlanWaves(wavesJSON []byte) ([]ledger.Record, error) {
	var plan planWaves
	if err := json.Unmarshal(wavesJSON, &plan); err != nil {
		return nil, fmt.Errorf("devague: decode plan waves json: %w", err)
	}
	if plan.Plan == "" {
		return nil, fmt.Errorf("devague: plan waves json has no plan slug")
	}
	runID := runIDForSlug(plan.Plan)

	waveOf := make(map[string]int, len(plan.Tasks))
	for i, wave := range plan.Waves {
		for _, id := range wave {
			waveOf[id] = i
		}
	}

	taskIDs := make([]string, 0, len(plan.Tasks))
	for id := range plan.Tasks {
		taskIDs = append(taskIDs, id)
	}
	sort.Strings(taskIDs)

	out := make([]ledger.Record, 0, len(taskIDs))
	for _, id := range taskIDs {
		task := plan.Tasks[id]

		wave, placed := waveOf[id]
		if !placed {
			return nil, fmt.Errorf("devague: plan %s task %s has a working contract but no wave placement", plan.Plan, id)
		}

		dependsOn := []string{}
		if wave > 0 {
			for _, priorID := range plan.Waves[wave-1] {
				dependsOn = append(dependsOn, recordIDForTask(plan.Plan, priorID))
			}
			sort.Strings(dependsOn)
		}

		coveredRefs := coveredClaimRefs(plan.Plan, task.Covers)

		data := map[string]any{
			"goal":       task.Summary,
			"status":     "ready",
			"depends_on": dependsOn,
			"devague": map[string]any{
				"plan":                plan.Plan,
				"task_id":             id,
				"instruction":         task.Instruction,
				"acceptance_criteria": task.AcceptanceCriteria,
				"covers":              task.Covers,
				"wave_index":          wave,
			},
		}

		rec, err := newRecord(
			recordIDForTask(plan.Plan, id),
			ledger.RecordTask,
			runID,
			// wavesJSON carries no per-task origin either; every task
			// devague's plan engine produced is treated as an agent
			// proposal — the same posture MapFrameClaims takes for an
			// llm-origin claim, and the only one the authority matrix
			// allows a producer that cannot be authenticated as human to
			// hold (PRD §10.4).
			ledger.Origin{Kind: ledger.OriginAgent, ActorID: "devague-plan"},
			ledger.AuthorityProposed,
			"",
			data,
			coveredRefs,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}

	sortRecords(out)
	return out, nil
}

// coveredClaimRefs translates a task's `covers` list into the ledger record
// ids of the claims among them (devague ids shaped "c<n>"), for
// provenance_refs. Honesty condition coverage targets ("h<n>") are recorded
// verbatim in the task's data.devague.covers instead: MapFrameClaims does
// not emit a standalone record per honesty condition (they are folded into
// their claim's payload), so an "h<n>" id here names nothing this run's
// ledger records could resolve, and provenance_refs must only ever point at
// records that exist.
func coveredClaimRefs(planSlug string, covers []string) []string {
	refs := make([]string, 0, len(covers))
	for _, id := range covers {
		if isClaimID(id) {
			refs = append(refs, recordIDForClaim(planSlug, id))
		}
	}
	sort.Strings(refs)
	return refs
}

// isClaimID reports whether id has devague's claim-id shape "c" followed by
// one or more digits (as opposed to an honesty-condition id, "h...").
func isClaimID(id string) bool {
	if len(id) < 2 || id[0] != 'c' {
		return false
	}
	for _, r := range id[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
