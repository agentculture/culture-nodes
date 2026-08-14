package api

// Plan import (task t22, issue #45): POST /v1alpha1/plan-imports decodes
// `devague plan show --json` (the faithful per-task view, carrying real
// dependency edges and real per-task status) and, optionally,
// `.devague/deliveries/<slug>.json` (deviations carrying their origin —
// the issue's "system knows" llm vs "user reports" user split), and
// persists a new immutable snapshot through internal/store/postgres'
// plan-import tables. This is deliberately NOT the append-only work ledger
// -- see migrations/0024_plan_imports.sql's header. Parsing/validation is
// internal/devague's job (ParsePlanShow/ParseDeviations); this handler
// only translates the validated result into the store's input shape and
// renders its own domain errors as 400 with a hint, never a panic and
// never a partial import (ParsePlanShow/ParseDeviations either return a
// complete, valid result or an error -- there is no partial result to leak
// through).

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/devague"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// importPlanRequest is components.schemas.PlanImportRequest.
type importPlanRequest struct {
	PlanShow   json.RawMessage `json:"plan_show"`
	Deviations json.RawMessage `json:"deviations,omitempty"`
}

// planImportTaskOut is components.schemas.PlanImportTask.
type planImportTaskOut struct {
	TaskRef            string   `json:"task_ref"`
	Summary            string   `json:"summary"`
	Instruction        string   `json:"instruction,omitempty"`
	OriginKind         string   `json:"origin_kind"`
	SourceStatus       string   `json:"source_status"`
	DependsOn          []string `json:"depends_on"`
	Wave               *int     `json:"wave,omitempty"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Covers             []string `json:"covers"`
}

// planImportDeviationOut is components.schemas.PlanImportDeviation.
type planImportDeviationOut struct {
	DeviationRef   string   `json:"deviation_ref"`
	What           string   `json:"what"`
	TaskRef        string   `json:"task_ref"`
	Reason         string   `json:"reason"`
	Affects        []string `json:"affects"`
	OriginKind     string   `json:"origin_kind"`
	SourceStatus   string   `json:"source_status"`
	Classification string   `json:"classification,omitempty"`
}

// planImportOut is components.schemas.PlanImport.
type planImportOut struct {
	ID           string                   `json:"id"`
	Slug         string                   `json:"slug"`
	Title        string                   `json:"title"`
	SourceSlug   string                   `json:"source_slug"`
	SourceStatus string                   `json:"source_status"`
	SourceDigest string                   `json:"source_digest"`
	ImportedAt   time.Time                `json:"imported_at"`
	Tasks        []planImportTaskOut      `json:"tasks"`
	Deviations   []planImportDeviationOut `json:"deviations"`
}

func planImportOutFrom(pi postgres.PlanImport) planImportOut {
	out := planImportOut{
		ID:           pi.ID,
		Slug:         pi.Slug,
		Title:        pi.Title,
		SourceSlug:   pi.SourceSlug,
		SourceStatus: pi.SourceStatus,
		SourceDigest: pi.SourceDigest,
		ImportedAt:   pi.ImportedAt,
		Tasks:        make([]planImportTaskOut, 0, len(pi.Tasks)),
		Deviations:   make([]planImportDeviationOut, 0, len(pi.Deviations)),
	}
	for _, t := range pi.Tasks {
		out.Tasks = append(out.Tasks, planImportTaskOut{
			TaskRef:            t.TaskRef,
			Summary:            t.Summary,
			Instruction:        t.Instruction,
			OriginKind:         t.OriginKind,
			SourceStatus:       t.SourceStatus,
			DependsOn:          nonNilJSONStrings(t.DependsOn),
			Wave:               t.Wave,
			AcceptanceCriteria: nonNilJSONStrings(t.AcceptanceCriteria),
			Covers:             nonNilJSONStrings(t.Covers),
		})
	}
	for _, d := range pi.Deviations {
		out.Deviations = append(out.Deviations, planImportDeviationOut{
			DeviationRef:   d.DeviationRef,
			What:           d.What,
			TaskRef:        d.TaskRef,
			Reason:         d.Reason,
			Affects:        nonNilJSONStrings(d.Affects),
			OriginKind:     d.OriginKind,
			SourceStatus:   d.SourceStatus,
			Classification: d.Classification,
		})
	}
	return out
}

// nonNilJSONStrings keeps a nil slice from rendering as JSON `null` in a
// field the schema declares as an array — writeJSON's readers should never
// have to special-case an absent list versus an empty one.
func nonNilJSONStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// handleImportPlan is POST /v1alpha1/plan-imports.
func (s *Server) handleImportPlan(w http.ResponseWriter, r *http.Request) error {
	var req importPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest(
			"send a JSON body matching PlanImportRequest: {plan_show, deviations?}",
			"decode request body: %v", err)
	}
	if len(req.PlanShow) == 0 {
		return badRequest(
			`pass "plan_show" — the exact object 'devague plan show --json' prints`,
			"plan_show is required")
	}

	plan, err := devague.ParsePlanShow(req.PlanShow)
	if err != nil {
		return badRequest(
			"plan_show must be a valid 'devague plan show --json' document — see the message for what is wrong and fix the source plan before re-importing",
			"%v", err)
	}

	in := postgres.ImportPlanInput{
		Slug:         plan.Slug,
		Title:        plan.Title,
		SourceSlug:   plan.FrameSlug,
		SourceStatus: plan.SourceStatus,
		Tasks:        make([]postgres.PlanImportTaskInput, 0, len(plan.Tasks)),
	}
	for _, task := range plan.Tasks {
		in.Tasks = append(in.Tasks, postgres.PlanImportTaskInput{
			TaskRef:            task.ID,
			Summary:            task.Summary,
			Instruction:        task.Instruction,
			OriginKind:         string(task.Origin),
			SourceStatus:       task.SourceStatus,
			DependsOn:          task.DependsOn,
			Wave:               task.Wave,
			AcceptanceCriteria: task.AcceptanceCriteria,
			Covers:             task.Covers,
		})
	}

	digestInput := append([]byte(nil), req.PlanShow...)
	if len(req.Deviations) > 0 {
		deliveries, err := devague.ParseDeviations(req.Deviations)
		if err != nil {
			return badRequest(
				"deviations must be a valid delivery document (the shape .devague/deliveries/<slug>.json holds on disk) — see the message for what is wrong and fix the source before re-importing",
				"%v", err)
		}
		if deliveries.PlanSlug != plan.Slug {
			return badRequest(
				"deviations.plan_slug must name the same plan as plan_show.slug",
				"deviations plan_slug %q does not match plan_show slug %q", deliveries.PlanSlug, plan.Slug)
		}
		in.Deviations = make([]postgres.PlanImportDeviationInput, 0, len(deliveries.Deviations))
		for _, d := range deliveries.Deviations {
			in.Deviations = append(in.Deviations, postgres.PlanImportDeviationInput{
				DeviationRef:   d.ID,
				What:           d.What,
				TaskRef:        d.TaskRef,
				Reason:         d.Reason,
				Affects:        d.Affects,
				OriginKind:     string(d.Origin),
				SourceStatus:   d.SourceStatus,
				Classification: d.Classification,
			})
		}
		digestInput = append(digestInput, req.Deviations...)
	}
	// One digest over everything actually imported: two imports of
	// byte-identical plan_show+deviations content digest identically, which
	// is what lets a caller (or a future dedup pass) tell "the same
	// content, imported twice" apart from "the plan genuinely changed"
	// without this handler guessing at that itself (see
	// migrations/0024_plan_imports.sql's "no supersedes chain" note).
	in.SourceDigest = contracts.Digest(digestInput)

	created, err := s.engineStore.ImportPlan(r.Context(), in)
	if err != nil {
		return classify(err)
	}

	writeJSON(w, http.StatusCreated, planImportOutFrom(created))
	return nil
}

// handleGetPlanImport is GET /v1alpha1/plan-imports/{id}.
func (s *Server) handleGetPlanImport(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")

	pi, err := s.engineStore.GetPlanImport(r.Context(), id)
	if err != nil {
		return classify(err)
	}

	writeJSON(w, http.StatusOK, planImportOutFrom(pi))
	return nil
}
