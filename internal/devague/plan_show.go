package devague

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// PlanTask is one validated task from `devague plan show --json`, the
// typed shape ParsePlanShow returns and both MapPlanShow (ledger records)
// and a durable-store import build from. Keeping the parse+validate step
// exported and separate from ledger-record construction means "a malformed
// plan is refused with a hint" happens exactly once, in ParsePlanShow, no
// matter which caller needs the result -- the store/API import path
// (internal/store/postgres, internal/api) does not need to go through
// ledger.Record at all, since a plan_import_tasks row is not a ledger
// record (see migrations/0024_plan_imports.sql's header for why).
type PlanTask struct {
	ID          string
	Summary     string
	Instruction string
	// Origin is the ledger producer kind devague's own user|llm origin maps
	// to (see claimOriginKind) -- human or agent, never anything else.
	Origin ledger.OriginKind
	// RawOrigin is devague's own origin word verbatim ("user" or "llm"),
	// kept alongside Origin the same way claimData preserves a claim's raw
	// origin string beside the record's translated envelope Origin.Kind --
	// the ledger vocabulary is not the only vocabulary worth keeping
	// visible in the payload.
	RawOrigin string
	// SourceStatus is devague's own plan-authoring decision status verbatim
	// -- proposed, confirmed, or rejected (devague/plan.py TASK_STATUSES).
	// This is NOT the same axis as task.schema.json's execution-status
	// enum (proposed/ready/claimed/.../cancelled): SourceStatus answers "did
	// a human accept this task into the plan"; taskLedgerStatus (below)
	// translates it into the different question the ledger's own task
	// status field answers, "where does this task stand as a unit of work".
	SourceStatus string
	// DependsOn is the task's REAL per-task dependency edges -- other task
	// ids in this plan, exactly as devague's `deps` field recorded them.
	// This is the entire reason MapPlanShow exists rather than reusing
	// MapPlanWaves: it is never widened to "everything in the previous
	// wave".
	DependsOn []string
	// Wave is computed locally by planTaskWaves, via topological layering
	// over DependsOn -- devague's `plan show --json` does not emit this
	// field at all (unlike planWaves.Waves, which is devague's own
	// layering of a DIFFERENT, lossy dependency reading). It exists purely
	// as derived display/grouping metadata for the durable store's "wave"
	// state; it carries no authority of its own and is never fed back into
	// DependsOn. Absent (nil) for a rejected task: a rejected task never
	// ships, so it does not occupy a wave.
	Wave               *int
	AcceptanceCriteria []string
	Covers             []string
}

// PlanShow is the validated, typed result of ParsePlanShow.
type PlanShow struct {
	Slug      string
	Title     string
	FrameSlug string
	// SourceStatus is devague's own plan.status verbatim: drafting,
	// converged, or exported (devague/plan.py Plan.status).
	SourceStatus string
	Tasks        []PlanTask
}

// ParsePlanShow decodes and validates `devague plan show --json` -- the
// faithful per-task view (see planShow's doc comment for why this package
// reads this view rather than `plan waves`). It never returns a partially
// valid result: any structural problem (a missing slug, a task with no id,
// a dependency edge naming a task this plan does not have, a dependency
// cycle) is refused as an error naming what is wrong, so a caller building
// on top of this (MapPlanShow, the plan-import API route, the plan-import
// CLI verb) cannot silently import a broken graph.
//
// # Wave computation
//
// Wave is derived HERE, locally, from the real DependsOn edges this view
// carries -- by the same topological-layering algorithm devague's own
// dependency_waves (devague/plan.py) uses: a task's wave is
// 1 + max(wave of each of its ACTIVE dependencies), and a dependency edge
// naming a task that is not active (rejected, or -- structurally impossible
// here since that is refused above -- absent) is trivially satisfied,
// exactly matching devague's own `d not in by_id` rule for its `by_id`
// built from active tasks only. That parity matters: this package is not
// inventing a new reading of what "wave" means, it is running the SAME
// algorithm devague runs, just over the real per-task edges this view
// carries instead of over `plan waves --json`'s already-computed (and, per
// plan.go's doc comment, coarser) layering.
//
// A dependency CYCLE among active tasks is refused as malformed rather than
// silently accepted into one leftover "wave", unlike devague's own
// dependency_waves (which dumps unplaceable tasks into a trailing wave and
// leaves cycle prevention to capture-time checks that devague's own
// `_require_dep_target` doc admits only catch a direct two-task cycle, not
// a longer one -- devague/cli/_commands/plan.py). An importer has no
// reason to be more permissive than the source system's own stated gap.
func ParsePlanShow(showJSON []byte) (PlanShow, error) {
	var raw planShow
	if err := json.Unmarshal(showJSON, &raw); err != nil {
		return PlanShow{}, fmt.Errorf("devague: decode plan show json: %w", err)
	}
	if raw.Slug == "" {
		return PlanShow{}, fmt.Errorf("devague: plan show json has no slug")
	}

	byID := make(map[string]planShowTask, len(raw.Tasks))
	order := make([]string, 0, len(raw.Tasks))
	for _, t := range raw.Tasks {
		if t.ID == "" {
			return PlanShow{}, fmt.Errorf("devague: plan %s: a task has no id", raw.Slug)
		}
		if _, dup := byID[t.ID]; dup {
			return PlanShow{}, fmt.Errorf("devague: plan %s: task id %s appears more than once", raw.Slug, t.ID)
		}
		byID[t.ID] = t
		order = append(order, t.ID)
	}

	for _, id := range order {
		t := byID[id]
		for _, dep := range t.Deps {
			if _, known := byID[dep]; !known {
				return PlanShow{}, fmt.Errorf(
					"devague: plan %s task %s depends on unknown task %s; run 'devague plan show --json' and check its deps name real task ids in this plan",
					raw.Slug, id, dep)
			}
		}
	}

	waves, err := planTaskWaves(byID, order)
	if err != nil {
		return PlanShow{}, fmt.Errorf("devague: plan %s: %w", raw.Slug, err)
	}

	tasks := make([]PlanTask, 0, len(order))
	for _, id := range order {
		t := byID[id]
		origin, err := claimOriginKind(t.Origin)
		if err != nil {
			return PlanShow{}, fmt.Errorf("devague: plan %s task %s: %w", raw.Slug, id, err)
		}
		if _, _, _, err := reviewForClaimStatus(t.Status); err != nil {
			return PlanShow{}, fmt.Errorf("devague: plan %s task %s: %w", raw.Slug, id, err)
		}
		tasks = append(tasks, PlanTask{
			ID:                 id,
			Summary:            t.Summary,
			Instruction:        t.Instruction,
			Origin:             origin,
			RawOrigin:          t.Origin,
			SourceStatus:       t.Status,
			DependsOn:          append([]string(nil), t.Deps...),
			Wave:               waves[id],
			AcceptanceCriteria: append([]string(nil), t.AcceptanceCriteria...),
			Covers:             append([]string(nil), t.Covers...),
		})
	}

	return PlanShow{
		Slug:         raw.Slug,
		Title:        raw.Title,
		FrameSlug:    raw.FrameSlug,
		SourceStatus: raw.Status,
		Tasks:        tasks,
	}, nil
}

// planTaskWaves computes each active (non-rejected) task's wave index by
// Kahn's-algorithm layering over the real dependency graph, mirroring
// devague's own dependency_waves exactly (see ParsePlanShow's doc comment).
// A rejected task's entry is nil. An unbreakable cycle among active tasks
// returns an error rather than the partial layering devague's own function
// falls back to.
func planTaskWaves(byID map[string]planShowTask, order []string) (map[string]*int, error) {
	active := make(map[string]bool, len(order))
	for _, id := range order {
		if byID[id].Status != "rejected" {
			active[id] = true
		}
	}

	wave := make(map[string]int, len(active))
	placed := make(map[string]bool, len(active))
	remaining := make([]string, 0, len(active))
	for _, id := range order {
		if active[id] {
			remaining = append(remaining, id)
		}
	}

	for level := 0; len(remaining) > 0; level++ {
		var ready []string
		for _, id := range remaining {
			ok := true
			for _, dep := range byID[id].Deps {
				// A dep on an inactive (rejected) task is trivially
				// satisfied -- devague's own `d not in by_id` rule, ported
				// verbatim (see the doc comment above).
				if active[dep] && !placed[dep] {
					ok = false
					break
				}
			}
			if ok {
				ready = append(ready, id)
			}
		}
		if len(ready) == 0 {
			sort.Strings(remaining)
			return nil, fmt.Errorf("dependency cycle among tasks %v: no task's dependencies are all satisfied; break the cycle with 'devague plan depend --remove'", remaining)
		}
		for _, id := range ready {
			wave[id] = level
			placed[id] = true
		}
		next := remaining[:0:0]
		for _, id := range remaining {
			if !placed[id] {
				next = append(next, id)
			}
		}
		remaining = next
	}

	out := make(map[string]*int, len(order))
	for _, id := range order {
		if w, ok := wave[id]; ok {
			w := w
			out[id] = &w
		} else {
			out[id] = nil
		}
	}
	return out, nil
}

// taskLedgerStatus translates devague's plan-authoring SourceStatus onto
// task.schema.json's execution-status vocabulary
// (proposed/ready/claimed/running/blocked/completed/failed/cancelled) --
// a DIFFERENT axis devague has no field for at all (see PlanTask's doc
// comment). Unlike MapPlanWaves, which cannot see per-task status and so
// hardcodes "ready" for every task it maps (plan.go's doc comment), this
// function can and does distinguish all three source states:
//
//   - proposed  -> proposed: nobody has accepted this task into the plan
//     yet, so it is not ready to be worked either.
//   - confirmed -> ready: a human accepted it; MapPlanWaves's "appearing in
//     the view an operator fans work out from means accepted work" reasoning
//     applies here too, now grounded in a real per-task signal instead of
//     an assumption.
//   - rejected  -> cancelled: task.schema.json's execution-status enum has
//     no "rejected" value (that is claim/task-approval vocabulary, not
//     execution vocabulary) -- "cancelled" is the enum value that means
//     the same thing an execution axis can say: this task will not run.
func taskLedgerStatus(sourceStatus string) string {
	switch sourceStatus {
	case "confirmed":
		return "ready"
	case "rejected":
		return "cancelled"
	default: // "proposed", and anything ParsePlanShow would have already refused
		return "proposed"
	}
}

// MapPlanShow maps `devague plan show --json` onto ledger records: the
// faithful counterpart to MapPlanWaves (plan.go), carrying each task's real
// per-task status and real dependency edges instead of the wave
// approximation. See ParsePlanShow for the parse/validation this builds on.
//
// # Authority
//
// Every base task record is origin-mapped from devague's task.origin
// (user|llm -> human|agent) and stamped authority proposed, unconditionally
// -- exactly claims.go's MapFrameClaims pattern, reused rather than
// reinvented, because plan.py's TASK_STATUSES is the identical
// proposed/confirmed/rejected vocabulary claim.status uses. When devague
// recorded a decision (task.status confirmed or rejected), a SECOND record
// is emitted: a review record, origin human, authority confirmed/rejected,
// referencing the base record — never rewriting it. This dual-record split
// is deliberately not "flattened" to a single record carrying devague's
// decision as its own authority, for the same reason claims.go's split
// exists: PRD §10.4 ("no actor promotes its own proposal") makes confirmed/
// rejected authority reachable only through a real review transaction
// (internal/ledger's CommitReview), and CheckAuthority provably refuses a
// bare-appended review record with no such transaction behind it
// (authority_test.go's TestConfirmedReviewRecordsRequireARealReviewTransaction,
// extended by this package's own test of the same property over tasks). So
// emitting the review record does not manufacture usable ledger authority
// out of an import — it is inert until a real CommitReview happens — it
// only PRESERVES, in the ledger's own vocabulary, a decision devague's own
// model already recorded, exactly the way MapFrameClaims already does for
// claims. See deviations.go's doc comment for why MapDeviations follows
// this same reasoning, and where it deliberately draws a narrower line.
func MapPlanShow(showJSON []byte) ([]ledger.Record, error) {
	plan, err := ParsePlanShow(showJSON)
	if err != nil {
		return nil, err
	}
	runID := runIDForSlug(plan.Slug)

	out := make([]ledger.Record, 0, len(plan.Tasks)*2)
	for _, t := range plan.Tasks {
		dependsOn := make([]string, 0, len(t.DependsOn))
		for _, dep := range t.DependsOn {
			dependsOn = append(dependsOn, recordIDForTask(plan.Slug, dep))
		}
		sort.Strings(dependsOn)

		devagueData := map[string]any{
			"plan":                plan.Slug,
			"task_id":             t.ID,
			"instruction":         t.Instruction,
			"acceptance_criteria": t.AcceptanceCriteria,
			"covers":              t.Covers,
			"origin":              t.RawOrigin,
			// status here is devague's own plan-authoring decision status,
			// deliberately kept distinct from the ledger-vocabulary
			// data.status field below -- see taskLedgerStatus.
			"status": t.SourceStatus,
		}
		if t.Wave != nil {
			devagueData["wave_index"] = *t.Wave
		}

		data := map[string]any{
			"goal":       t.Summary,
			"status":     taskLedgerStatus(t.SourceStatus),
			"depends_on": dependsOn,
			"devague":    devagueData,
		}

		recID := recordIDForTask(plan.Slug, t.ID)
		base, err := newRecord(
			recID,
			ledger.RecordTask,
			runID,
			ledger.Origin{Kind: t.Origin, ActorID: actorIDFor(t.Origin)},
			ledger.AuthorityProposed,
			"",
			data,
			coveredClaimRefs(plan.Slug, t.Covers),
		)
		if err != nil {
			return nil, err
		}
		out = append(out, base)

		verdict, authority, hasDecision, err := reviewForClaimStatus(t.SourceStatus)
		if err != nil {
			return nil, fmt.Errorf("devague: plan %s task %s: %w", plan.Slug, t.ID, err)
		}
		if !hasDecision {
			continue
		}

		review, err := newRecord(
			reviewIDForTask(plan.Slug, t.ID),
			ledger.RecordReview,
			runID,
			ledger.Origin{Kind: ledger.OriginHuman, ActorID: actorIDFor(ledger.OriginHuman)},
			authority,
			ledger.NullableID(recID),
			reviewData("task", verdict, recID, t.SourceStatus),
			[]string{recID},
		)
		if err != nil {
			return nil, err
		}
		out = append(out, review)
	}

	sortRecords(out)
	return out, nil
}
