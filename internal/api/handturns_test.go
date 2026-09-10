package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// createHandTurnReq mirrors components.schemas.CreateHandTurnRequest (task
// t16, decision c25) -- the documented wire shape, encoded here rather than
// imported from internal/api's unexported request type (see api_test.go's
// package doc comment).
type createHandTurnReq struct {
	What          string   `json:"what"`
	Stage         string   `json:"stage"`
	WorkItem      string   `json:"work_item"`
	ActorID       string   `json:"actor_id"`
	RunID         string   `json:"run_id,omitempty"`
	DefinitionRef string   `json:"definition_ref,omitempty"`
	Rule          string   `json:"rule,omitempty"`
	EvidenceRefs  []string `json:"evidence_refs,omitempty"`
}

// createHandTurnRun publishes minimal.workflow.yaml and creates one run
// carrying work_item (and optionally a category), returning the run and its
// single node run id -- createCategorizedRun's shape with the t1 column
// (runs_workitem_test.go's createRunWithWorkItemReq).
func createHandTurnRun(t *testing.T, f *fixture, workItem, category string) (apipkg.RunOut, string) {
	t.Helper()
	source := readFixtureWorkflow(t, "minimal.workflow.yaml")
	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(source)}, &published)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("publish minimal.workflow.yaml: status = %d; body = %s", resp.StatusCode, body)
	}
	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunWithWorkItemReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`), Category: category, WorkItem: workItem}, &run)
	requireStatus(t, resp, body, http.StatusCreated)
	view := getRunView(t, f, run.ID)
	if len(view.NodeRuns) != 1 {
		t.Fatalf("run %s: got %d node runs, want 1", run.ID, len(view.NodeRuns))
	}
	return run, view.NodeRuns[0].ID
}

func postHandTurn(t *testing.T, f *fixture, req createHandTurnReq, wantStatus int) (ledger.Record, []byte) {
	t.Helper()
	var rec ledger.Record
	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/hand-turns"), decisionAuthSecret, req, &rec)
	requireStatus(t, resp, body, wantStatus)
	return rec, body
}

// confirmThroughReview confirms (or rejects) the named records on run through
// the EXISTING review surface -- create + commit -- as the human reviewer.
func confirmThroughReview(t *testing.T, f *fixture, runID, reviewer string, decisions map[string]string) {
	t.Helper()
	var records apipkg.LedgerRecordsOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+runID+"/ledger"), nil, &records)
	requireStatus(t, resp, body, http.StatusOK)
	ids := make([]string, 0, len(decisions))
	for id := range decisions {
		ids = append(ids, id)
	}
	var review apipkg.ReviewRequestOut
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+runID+"/reviews"), decisionAuthSecret,
		createReviewReq{RecordIDs: ids, LedgerVersion: records.LedgerVersion, ReviewerActorID: reviewer}, &review)
	requireStatus(t, resp, body, http.StatusCreated)
	var result apipkg.ReviewCommitResultOut
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/reviews/"+review.ID+"/commit"), decisionAuthSecret,
		commitReviewReq{Decisions: decisions, ExpectedLedgerVersion: records.LedgerVersion, Rationale: "read the PR history; these are the turns I took"}, &result)
	requireStatus(t, resp, body, http.StatusOK)
}

// TestCreateHandTurnAgentProposesThenHumanConfirmsBatch is decision c25 end
// to end on the API: the observing agent proposes hand_turn records against
// a proposed definition, and the human confirms the batch -- definition and
// turns -- in ONE review. The targets stay proposed; the confirmation is the
// review record naming each of them.
func TestCreateHandTurnAgentProposesThenHumanConfirmsBatch(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createHandTurnRun(t, f, "SCRUM-9", "")
	observer := f.insertActor("hand-turn-observer")
	human := f.insertActorKind("ori", "human")

	// The human's definition is itself a record (honesty h17), written
	// proposed through its own route and confirmed in the same batch below.
	var definition ledger.Record
	resp0, body0 := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/hand-turn-definitions"), decisionAuthSecret,
		map[string]any{
			"stages":    []string{"land", "cleanup"},
			"rules":     []map[string]any{{"id": "commit_without_run", "stage": "land", "description": "a commit on the PR branch with no run attempt in the window", "kind": "commit_without_run"}},
			"work_item": "SCRUM-9", "actor_id": human, "notes": "first definition",
		}, &definition)
	requireStatus(t, resp0, body0, http.StatusCreated)
	if definition.RecordType != ledger.RecordHandTurnDefinition || definition.Origin.Kind != ledger.OriginHuman || definition.Authority != ledger.AuthorityProposed || definition.RunID != run.ID {
		t.Fatalf("definition = %+v, want a proposed human-origin hand_turn_definition on run %s", definition, run.ID)
	}

	first, _ := postHandTurn(t, f, createHandTurnReq{
		What: "cherry-picked a029689 onto the PR branch", Stage: "land", WorkItem: "SCRUM-9",
		ActorID: observer, DefinitionRef: definition.ID, Rule: "commit_without_run", EvidenceRefs: []string{"a029689"},
	}, http.StatusCreated)
	if first.RecordType != ledger.RecordHandTurn {
		t.Fatalf("record_type = %q, want hand_turn", first.RecordType)
	}
	if first.Origin.Kind != ledger.OriginAgent || first.Origin.ActorID != observer {
		t.Fatalf("origin = %+v, want agent %s", first.Origin, observer)
	}
	if first.Authority != ledger.AuthorityProposed {
		t.Fatalf("authority = %q, want proposed", first.Authority)
	}
	if first.RunID != run.ID {
		t.Fatalf("run_id = %q, want the SCRUM-9 run %s (resolved from work_item)", first.RunID, run.ID)
	}
	if first.SubjectRef.String() != definition.ID {
		t.Fatalf("subject_ref = %q, want the definition %s", first.SubjectRef, definition.ID)
	}
	data, _ := first.DataMap()
	if data["work_item"] != "SCRUM-9" || data["stage"] != "land" || data["definition_ref"] != definition.ID || data["rule"] != "commit_without_run" {
		t.Fatalf("data = %v, want work_item/stage/definition_ref/rule carried verbatim", data)
	}
	second, _ := postHandTurn(t, f, createHandTurnReq{
		What: "deleted review-fix/t3", Stage: "cleanup", WorkItem: "SCRUM-9", ActorID: observer, DefinitionRef: definition.ID,
	}, http.StatusCreated)

	confirmThroughReview(t, f, run.ID, human, map[string]string{definition.ID: "confirm", first.ID: "confirm", second.ID: "reject"})

	var records apipkg.LedgerRecordsOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID+"/ledger"), nil, &records)
	requireStatus(t, resp, body, http.StatusOK)
	verdicts := map[string]ledger.Authority{}
	for _, r := range records.Items {
		if r.ID == first.ID || r.ID == second.ID || r.ID == definition.ID {
			if r.Authority != ledger.AuthorityProposed {
				t.Fatalf("target %s is %q after review, want still proposed (a review never rewrites its target)", r.ID, r.Authority)
			}
		}
		if r.RecordType == ledger.RecordReview {
			verdicts[r.SubjectRef.String()] = r.Authority
		}
	}
	if verdicts[definition.ID] != ledger.AuthorityConfirmed || verdicts[first.ID] != ledger.AuthorityConfirmed || verdicts[second.ID] != ledger.AuthorityRejected {
		t.Fatalf("review verdicts = %v, want definition+first confirmed, second rejected", verdicts)
	}
}

// TestCreateHandTurnHumanDirectEntryLandsProposed is `nodes hand-turn` typed
// by a person: unlike a grade, a hand-turn is a claim about the world that
// the review surface ratifies, so the human's direct entry lands proposed.
func TestCreateHandTurnHumanDirectEntryLandsProposed(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	createHandTurnRun(t, f, "SCRUM-11", "")
	human := f.insertActorKind("ori", "human")

	rec, _ := postHandTurn(t, f, createHandTurnReq{What: "replied on the thread by hand", Stage: "review", WorkItem: "SCRUM-11", ActorID: human}, http.StatusCreated)
	if rec.Origin.Kind != ledger.OriginHuman || rec.Authority != ledger.AuthorityProposed {
		t.Fatalf("origin/authority = %s/%s, want human/proposed", rec.Origin.Kind, rec.Authority)
	}
	data, _ := rec.DataMap()
	if _, present := data["definition_ref"]; !present || data["definition_ref"] != nil {
		t.Fatalf("definition_ref = %v, want an explicit null when no definition was named", data["definition_ref"])
	}
}

// TestCreateHandTurnResolvesNewestRunAndRefusesUnknownWorkItem: the run is
// resolved from the work item -- the NEWEST run carrying it -- and a work
// item no run carries is a 404 naming the fix, never a record with no run.
func TestCreateHandTurnResolvesNewestRunAndRefusesUnknownWorkItem(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	human := f.insertActorKind("ori", "human")
	createHandTurnRun(t, f, "SCRUM-12", "")
	time.Sleep(5 * time.Millisecond)
	newest, _ := createHandTurnRun(t, f, "SCRUM-12", "")

	rec, _ := postHandTurn(t, f, createHandTurnReq{What: "reset the checkout over ssh", Stage: "dispatch", WorkItem: "SCRUM-12", ActorID: human}, http.StatusCreated)
	if rec.RunID != newest.ID {
		t.Fatalf("run_id = %q, want the newest SCRUM-12 run %s", rec.RunID, newest.ID)
	}

	_, body := postHandTurn(t, f, createHandTurnReq{What: "x", Stage: "land", WorkItem: "SCRUM-404", ActorID: human}, http.StatusNotFound)
	apiErr := decodeAPIError(t, body)
	if !strings.Contains(apiErr.Message, "SCRUM-404") || !strings.Contains(apiErr.Remediation, "run_id") {
		t.Fatalf("404 body = %+v, want the work item named and run_id offered as the fix", apiErr)
	}

	other, _ := createHandTurnRun(t, f, "SCRUM-13", "")
	_, body = postHandTurn(t, f, createHandTurnReq{What: "x", Stage: "land", WorkItem: "SCRUM-12", ActorID: human, RunID: other.ID}, http.StatusBadRequest)
	if apiErr := decodeAPIError(t, body); !strings.Contains(apiErr.Message, "SCRUM-13") {
		t.Fatalf("run/work-item mismatch body = %+v, want the run's own work item named", apiErr)
	}
}

// TestCreateHandTurnRequiresCoreFieldsAndKnownActor pins the 400/404 edges.
func TestCreateHandTurnRequiresCoreFieldsAndKnownActor(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	createHandTurnRun(t, f, "SCRUM-14", "")
	human := f.insertActorKind("ori", "human")
	runner := f.insertActorKind("headspace", "runner")

	postHandTurn(t, f, createHandTurnReq{Stage: "land", WorkItem: "SCRUM-14", ActorID: human}, http.StatusBadRequest)
	postHandTurn(t, f, createHandTurnReq{What: "x", WorkItem: "SCRUM-14", ActorID: human}, http.StatusBadRequest)
	postHandTurn(t, f, createHandTurnReq{What: "x", Stage: "land", ActorID: human}, http.StatusBadRequest)
	postHandTurn(t, f, createHandTurnReq{What: "x", Stage: "land", WorkItem: "SCRUM-14"}, http.StatusBadRequest)
	postHandTurn(t, f, createHandTurnReq{What: "x", Stage: "land", WorkItem: "SCRUM-14", ActorID: "actor_nobody"}, http.StatusNotFound)
	_, body := postHandTurn(t, f, createHandTurnReq{What: "x", Stage: "land", WorkItem: "SCRUM-14", ActorID: runner}, http.StatusBadRequest)
	if apiErr := decodeAPIError(t, body); !strings.Contains(apiErr.Message, "runner") {
		t.Fatalf("runner-kind actor body = %+v, want the kind named", apiErr)
	}
}

// TestActorStatsHandTurnsByStageCountsConfirmedOnly is the stats half of
// decision c25: hand_turns_by_stage counts CONFIRMED hand_turn records only
// -- proposed and rejected ones contribute nothing -- per stage and per work
// item, scoped to runs this actor attempted, in the total and in the run's
// category bucket.
func TestActorStatsHandTurnsByStageCountsConfirmedOnly(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	worker := f.insertActor("codex-thor")
	bystander := f.insertActor("codex-orin")
	observer := f.insertActor("hand-turn-observer")
	human := f.insertActorKind("ori", "human")

	run, nodeRun := createHandTurnRun(t, f, "SCRUM-9", "review")
	now := time.Now().UTC()
	seedActorAttempt(t, f, nodeRun, worker, 1, engine.StatusSucceeded, now.Add(-time.Minute), now, nil)

	turn := func(what, stage string) ledger.Record {
		rec, _ := postHandTurn(t, f, createHandTurnReq{What: what, Stage: stage, WorkItem: "SCRUM-9", ActorID: observer}, http.StatusCreated)
		return rec
	}
	land1 := turn("cherry-pick a029689", "land")
	land2 := turn("cherry-pick 1b2c3d4", "land")
	cleanup := turn("deleted review-fix/t3", "cleanup")
	rejected := turn("a false positive", "review")
	turn("never reviewed", "dispatch")

	confirmThroughReview(t, f, run.ID, human, map[string]string{land1.ID: "confirm", land2.ID: "confirm", cleanup.ID: "confirm", rejected.ID: "reject"})

	stats := getActorStats(t, f, worker)
	want := []apipkg.ActorHandTurnStageCountOut{
		{Stage: "cleanup", WorkItem: "SCRUM-9", Count: 1},
		{Stage: "land", WorkItem: "SCRUM-9", Count: 2},
	}
	assertHandTurns := func(label string, got []apipkg.ActorHandTurnStageCountOut) {
		if len(got) != len(want) {
			t.Fatalf("%s hand_turns_by_stage = %+v, want %+v", label, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s hand_turns_by_stage[%d] = %+v, want %+v", label, i, got[i], want[i])
			}
		}
	}
	assertHandTurns("total", stats.Total.HandTurnsByStage)
	review, ok := findCategoryBucket(stats, "review")
	if !ok {
		t.Fatalf("no 'review' category bucket in %+v", stats.Categories)
	}
	assertHandTurns("review bucket", review.HandTurnsByStage)

	// An actor with no attempt on the run inherits none of its hand-turns,
	// and the empty case renders as a present-but-empty list, never null.
	other := getActorStats(t, f, bystander)
	if other.Total.HandTurnsByStage == nil || len(other.Total.HandTurnsByStage) != 0 {
		t.Fatalf("bystander hand_turns_by_stage = %#v, want an empty (non-nil) list", other.Total.HandTurnsByStage)
	}
}

// TestCreateHandTurnDefinitionIteratesBySuperseding: the human iterates the
// definition from what the observer got wrong by appending a new record that
// names the old one in `supersedes` -- the old record is untouched, and a
// second replacement of the same record is refused.
func TestCreateHandTurnDefinitionIteratesBySuperseding(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	createHandTurnRun(t, f, "SCRUM-15", "")
	human := f.insertActorKind("ori", "human")
	body := func(supersedes string) map[string]any {
		out := map[string]any{
			"stages":    []string{"land"},
			"rules":     []map[string]any{{"id": "r1", "stage": "land", "description": "d"}},
			"work_item": "SCRUM-15", "actor_id": human,
		}
		if supersedes != "" {
			out["supersedes"] = supersedes
		}
		return out
	}
	var first, second ledger.Record
	resp, raw := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/hand-turn-definitions"), decisionAuthSecret, body(""), &first)
	requireStatus(t, resp, raw, http.StatusCreated)
	resp, raw = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/hand-turn-definitions"), decisionAuthSecret, body(first.ID), &second)
	requireStatus(t, resp, raw, http.StatusCreated)
	if second.Supersedes.String() != first.ID {
		t.Fatalf("supersedes = %q, want %s", second.Supersedes, first.ID)
	}
	resp, raw = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/hand-turn-definitions"), decisionAuthSecret, body(first.ID), nil)
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("second replacement of %s: status %d body %s, want a 4xx refusal", first.ID, resp.StatusCode, raw)
	}
	resp, raw = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/hand-turn-definitions"), decisionAuthSecret,
		map[string]any{"stages": []string{}, "rules": []any{}, "work_item": "SCRUM-15", "actor_id": human}, nil)
	requireStatus(t, resp, raw, http.StatusBadRequest)
}

// TestCreateHandTurnDefinitionRefusesSupersedingANonDefinition: `supersedes`
// is what removes a record from every projection, so the route may only
// point it at another definition. A definition naming the run's hand_turn
// would otherwise be accepted -- the ledger only checks the target exists,
// is unreplaced, and shares the run -- and would silently drop that turn
// from the confirmed hand_turns_by_stage count a delivery summary cites.
func TestCreateHandTurnDefinitionRefusesSupersedingANonDefinition(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createHandTurnRun(t, f, "SCRUM-16", "")
	human := f.insertActorKind("ori", "human")
	turn, _ := postHandTurn(t, f, createHandTurnReq{
		What: "rebased the branch by hand", Stage: "land", WorkItem: "SCRUM-16", ActorID: human,
	}, http.StatusCreated)

	definition := func(supersedes string) map[string]any {
		return map[string]any{
			"stages":    []string{"land"},
			"rules":     []map[string]any{{"id": "r1", "stage": "land", "description": "d"}},
			"work_item": "SCRUM-16", "actor_id": human, "supersedes": supersedes,
		}
	}
	resp, raw := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/hand-turn-definitions"), decisionAuthSecret, definition(turn.ID), nil)
	requireStatus(t, resp, raw, http.StatusBadRequest)
	if !strings.Contains(string(raw), string(ledger.RecordHandTurn)) {
		t.Fatalf("refusal = %s, want it to name the kind of record %s actually is", raw, turn.ID)
	}
	// An id that names no record at all is a 404, not a 400.
	resp, raw = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/hand-turn-definitions"), decisionAuthSecret,
		definition("ledger_DOESNOTEXIST0000000000001"), nil)
	requireStatus(t, resp, raw, http.StatusNotFound)

	// The turn is still live: nothing names it, so it is still on the run's
	// ledger and still countable.
	var records apipkg.LedgerRecordsOut
	resp, raw = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID+"/ledger"), nil, &records)
	requireStatus(t, resp, raw, http.StatusOK)
	for _, r := range records.Items {
		if r.Supersedes.String() == turn.ID {
			t.Fatalf("record %s supersedes the hand-turn %s; the refusal did not hold", r.ID, turn.ID)
		}
	}
}
