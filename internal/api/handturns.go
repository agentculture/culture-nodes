package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// createHandTurnRequest is components.schemas.CreateHandTurnRequest (task
// t16, decision c25).
type createHandTurnRequest struct {
	What          string   `json:"what"`
	Stage         string   `json:"stage"`
	WorkItem      string   `json:"work_item"`
	ActorID       string   `json:"actor_id"`
	RunID         string   `json:"run_id,omitempty"`
	DefinitionRef string   `json:"definition_ref,omitempty"`
	Rule          string   `json:"rule,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
	ObservedAt    string   `json:"observed_at,omitempty"`
}

// handleCreateHandTurn is POST /v1alpha1/hand-turns (issue #319, decision
// c25): one step a person performed by hand, appended as a
// schemas/ledger/hand_turn.schema.json record against the run that carries
// the named work item.
//
// Like handleCreateGrade this handler decides two things and lets
// internal/ledger decide the rest:
//
//  1. Origin: actor_id is looked up in the registry and its registered
//     `kind` -- human or agent -- becomes origin.Kind. The observing agent
//     (examples/hand-turn-observer) writes as itself; a person typing
//     `nodes hand-turn` writes as themselves. Any other kind has no producer
//     rule for a claim about what a person did and is refused 400.
//  2. Run: a hand-turn is filed against a work item, not a run, because
//     that is how the operator experiences it -- but every ledger record is
//     run-scoped, and the review that confirms it is too. So the NEWEST run
//     whose runs.work_item (migrations/0057, decision c41) equals work_item
//     is the record's run, unless run_id names one explicitly, in which case
//     that run must carry the same work item. A work item no run carries is
//     a 404 that names the fix, never a record with no run.
//
// Authority is NOT decided here: it is always proposed. Unlike a grade -- a
// human's own opinion, confirmed on writing -- a hand-turn is a claim about
// the world that the review surface ratifies (POST /v1alpha1/runs/{id}/
// reviews + POST /v1alpha1/reviews/{id}/commit). The producer/authority
// matrix is unchanged by this record kind; a human asking for observed or
// confirmed here would be refused by ledger.CheckAuthority, and this handler
// never asks. Per-actor stats count the confirmed subset only
// (postgres.ActorCategoryStats.HandTurnsByStage), so the number a delivery
// summary cites is the number a person ratified.
func (s *Server) handleCreateHandTurn(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}
	ctx := r.Context()

	var req createHandTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest(
			"send a JSON body matching CreateHandTurnRequest: {what, stage, work_item, actor_id, run_id?, definition_ref?, rule?, evidence_refs?}",
			"decode request body: %v", err)
	}
	var warning string
	req.ActorID, warning = principalActor(r, "actor_id", req.ActorID)
	switch {
	case req.What == "":
		return badRequest("what describes the step the person performed, in one line", "what is required")
	case req.Stage == "":
		return badRequest("stage names the loop stage (one of the confirmed hand_turn_definition's stages, e.g. land)", "stage is required")
	case req.WorkItem == "":
		return badRequest("work_item names the work item the turn served (e.g. SCRUM-9)", "work_item is required")
	case req.ActorID == "":
		return badRequest("actor_id names the actor recording the hand-turn (the observer, or the person)", "actor_id is required")
	}

	producer, err := s.engineStore.GetActor(ctx, req.ActorID)
	if err != nil {
		return classify(err)
	}
	var origin ledger.OriginKind
	switch producer.Kind {
	case "human":
		origin = ledger.OriginHuman
	case "agent":
		origin = ledger.OriginAgent
	default:
		return badRequest(
			"record a hand-turn with an actor_id registered as kind human or agent",
			"actor %s is registered as kind %q, which has no producer rule for a hand-turn claim", req.ActorID, producer.Kind)
	}

	runID, err := s.resolveHandTurnRun(r, req.WorkItem, req.RunID)
	if err != nil {
		return err
	}

	data := map[string]any{
		"what":      req.What,
		"stage":     req.Stage,
		"work_item": req.WorkItem,
		// Always present: null says "no definition was named", which is a
		// real answer the schema admits and a stats reader can tell apart
		// from a field that was never written.
		"definition_ref": nil,
	}
	if req.DefinitionRef != "" {
		data["definition_ref"] = req.DefinitionRef
	}
	if req.Rule != "" {
		data["rule"] = req.Rule
	}
	if len(req.EvidenceRefs) > 0 {
		data["evidence_refs"] = req.EvidenceRefs
	}
	if req.ObservedAt != "" {
		data["observed_at"] = req.ObservedAt
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return internalError(err)
	}

	rec := ledger.Record{
		RecordType: ledger.RecordHandTurn,
		RunID:      runID,
		Origin:     ledger.Origin{Kind: origin, ActorID: req.ActorID},
		Authority:  ledger.AuthorityProposed,
		SubjectRef: ledger.NullableID(req.DefinitionRef),
		Data:       payload,
	}
	if req.DefinitionRef != "" {
		rec.ProvenanceRefs = []string{req.DefinitionRef}
	}
	appended, err := s.Ledger.Append(ctx, rec)
	if err != nil {
		return classify(err)
	}
	writeJSONWithWarning(w, http.StatusCreated, appended, warning)
	return nil
}

// resolveHandTurnRun picks the run a hand-turn is filed against: runID when
// given (verified to exist and to carry workItem), else the newest run in
// this namespace whose work_item column equals workItem.
func (s *Server) resolveHandTurnRun(r *http.Request, workItem, runID string) (string, error) {
	ctx := r.Context()
	if runID != "" {
		run, err := s.engineStore.Run(ctx, runID)
		if err != nil {
			return "", classify(err)
		}
		if run.WorkItem != workItem {
			return "", badRequest(
				"omit run_id to file against the newest run carrying work_item, or name a run whose work_item matches",
				"run %s carries work_item %q, not %q", runID, run.WorkItem, workItem)
		}
		return runID, nil
	}
	runs, _, err := s.listRuns(ctx, listRunsParams{WorkItem: workItem, Limit: 1, Sort: sortCreatedAt})
	if err != nil {
		return "", internalError(err)
	}
	if len(runs) == 0 {
		return "", notFound(
			"create the work item's run first (nodes run create --work-item KEY), or pass run_id to file against a specific run",
			"no run in this namespace carries work_item %q", workItem)
	}
	return runs[0].ID, nil
}

// createHandTurnDefinitionRequest is components.schemas.CreateHandTurnDefinitionRequest.
type createHandTurnDefinitionRequest struct {
	Stages       []string          `json:"stages"`
	Rules        []json.RawMessage `json:"rules"`
	Notes        string            `json:"notes,omitempty"`
	HumanLogins  []string          `json:"human_logins,omitempty"`
	WorkItem     string            `json:"work_item"`
	ActorID      string            `json:"actor_id"`
	RunID        string            `json:"run_id,omitempty"`
	SupersedesID string            `json:"supersedes,omitempty"`
}

// handleCreateHandTurnDefinition is POST /v1alpha1/hand-turn-definitions
// (decision c25, honesty h17: "the human's hand-turn definition is a
// confirmed record"). It appends a hand_turn_definition record -- proposed,
// under the recording actor's registered kind -- against the work item's
// newest run (or run_id), exactly like handleCreateHandTurn; the person then
// confirms it through the review surface, so the ledger shows the definition
// was ratified rather than merely typed. An iteration names the record it
// replaces in `supersedes` and is appended through AppendSuperseding, which
// is what removes the old definition from every projection without touching
// it -- and that record must itself be a hand_turn_definition
// (requireSupersededDefinition), because "removes it from every projection"
// is just as true of a record this route has no business replacing.
func (s *Server) handleCreateHandTurnDefinition(w http.ResponseWriter, r *http.Request) error {
	if err := s.requireDecisionAuth(r); err != nil {
		return err
	}
	ctx := r.Context()

	var req createHandTurnDefinitionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return badRequest(
			"send a JSON body matching CreateHandTurnDefinitionRequest: {stages, rules, work_item, actor_id, notes?, human_logins?, run_id?, supersedes?}",
			"decode request body: %v", err)
	}
	var warning string
	req.ActorID, warning = principalActor(r, "actor_id", req.ActorID)
	switch {
	case len(req.Stages) == 0:
		return badRequest("stages lists the loop stages a hand-turn may be filed under (e.g. dispatch, land, review, cleanup)", "stages is required")
	case len(req.Rules) == 0:
		return badRequest("rules lists what the observer recognises, each {id, stage, description, ...}", "rules is required")
	case req.WorkItem == "":
		return badRequest("work_item names the work item the definition is filed against", "work_item is required")
	case req.ActorID == "":
		return badRequest("actor_id names the actor recording the definition", "actor_id is required")
	}

	producer, err := s.engineStore.GetActor(ctx, req.ActorID)
	if err != nil {
		return classify(err)
	}
	var origin ledger.OriginKind
	switch producer.Kind {
	case "human":
		origin = ledger.OriginHuman
	case "agent":
		origin = ledger.OriginAgent
	default:
		return badRequest(
			"record a definition with an actor_id registered as kind human or agent",
			"actor %s is registered as kind %q, which has no producer rule for a definition", req.ActorID, producer.Kind)
	}

	runID, err := s.resolveHandTurnRun(r, req.WorkItem, req.RunID)
	if err != nil {
		return err
	}

	data := map[string]any{"stages": req.Stages, "rules": req.Rules}
	if req.Notes != "" {
		data["notes"] = req.Notes
	}
	if len(req.HumanLogins) > 0 {
		data["human_logins"] = req.HumanLogins
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return internalError(err)
	}
	rec := ledger.Record{
		RecordType: ledger.RecordHandTurnDefinition,
		RunID:      runID,
		Origin:     ledger.Origin{Kind: origin, ActorID: req.ActorID},
		Authority:  ledger.AuthorityProposed,
		Data:       payload,
	}
	var appended ledger.Record
	if req.SupersedesID != "" {
		if err := s.requireSupersededDefinition(ctx, req.SupersedesID); err != nil {
			return err
		}
		appended, err = s.Ledger.AppendSuperseding(ctx, rec, req.SupersedesID)
	} else {
		appended, err = s.Ledger.Append(ctx, rec)
	}
	if err != nil {
		return classify(err)
	}
	writeJSONWithWarning(w, http.StatusCreated, appended, warning)
	return nil
}

// requireSupersededDefinition refuses a `supersedes` that names anything
// other than another hand_turn_definition.
//
// The ledger checks what it can see: the target exists, it has no live
// replacement, and it belongs to the same run. Within one run it takes the
// caller's word for WHAT is being replaced -- so without this check, a
// definition naming a `hand_turn` (or an `evidence` record, or a `review`)
// on that run would be appended happily, and naming a record is exactly
// what removes it from every projection. A typo'd id would silently drop a
// confirmed hand-turn from the `hand_turns_by_stage` count a delivery
// summary cites, leaving a ledger where nothing was edited and a number
// that no longer matches it. An iteration replaces a definition; a mistaken
// record of any other kind is corrected by its own writer, through its own
// route.
func (s *Server) requireSupersededDefinition(ctx context.Context, supersedesID string) error {
	target, err := s.Ledger.Record(ctx, supersedesID)
	if err != nil {
		return classify(err)
	}
	if target.RecordType != ledger.RecordHandTurnDefinition {
		return badRequest(
			"name the hand_turn_definition this one iterates, or omit supersedes to file a first definition",
			"record %s is a %s record, and a definition may only replace another %s",
			supersedesID, target.RecordType, ledger.RecordHandTurnDefinition)
	}
	return nil
}
