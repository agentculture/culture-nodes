package api_test

import (
	"net/http"
	"strings"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// createGradeReq mirrors components.schemas.CreateGradeRequest — see
// api_test.go's package doc comment on why this package encodes the
// documented wire shape rather than reaching for internal/api's unexported
// request type.
type createGradeReq struct {
	Rating           int    `json:"rating"`
	Rationale        string `json:"rationale"`
	EvaluatedActorID string `json:"evaluated_actor_id"`
	GradingActorID   string `json:"grading_actor_id"`
}

// TestCreateGradeAgentProposedThenHumanConfirmsThroughReview is task t16's
// acceptance criterion 1: an agent actor grades a run (lands proposed), and
// a human confirms that grade end to end through the EXISTING review
// surface — create review + commit — with nothing special-cased for grade
// records along the way.
func TestCreateGradeAgentProposedThenHumanConfirmsThroughReview(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)

	graderAgent := f.insertActor("grader-agent")
	evaluatedActor := f.insertActor("evaluated")
	reviewerActor := f.insertActorKind("reviewer", "human")

	var grade ledger.Record
	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/grades"), decisionAuthSecret,
		createGradeReq{
			Rating: 4, Rationale: "solid, one small miss",
			EvaluatedActorID: evaluatedActor, GradingActorID: graderAgent,
		}, &grade)
	requireStatus(t, resp, body, http.StatusCreated)
	if grade.RecordType != ledger.RecordGrade {
		t.Fatalf("record_type = %q, want %q", grade.RecordType, ledger.RecordGrade)
	}
	if grade.Origin.Kind != ledger.OriginAgent || grade.Origin.ActorID != graderAgent {
		t.Fatalf("origin = %+v, want kind=%q actor_id=%q", grade.Origin, ledger.OriginAgent, graderAgent)
	}
	if grade.Authority != ledger.AuthorityProposed {
		t.Fatalf("authority = %q, want %q (an agent may only propose)", grade.Authority, ledger.AuthorityProposed)
	}

	// A human now confirms the agent's proposed grade through the ordinary
	// review surface -- create + commit -- exactly the path any other
	// agent-origin proposal is confirmed through (PRD §10.8).
	var review apipkg.ReviewRequestOut
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/reviews"),
		decisionAuthSecret,
		createReviewReq{RecordIDs: []string{grade.ID}, LedgerVersion: 1, ReviewerActorID: reviewerActor}, &review)
	requireStatus(t, resp, body, http.StatusCreated)

	var commitResult apipkg.ReviewCommitResultOut
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/reviews/"+review.ID+"/commit"),
		decisionAuthSecret,
		commitReviewReq{
			Decisions:             map[string]string{grade.ID: "confirm"},
			ExpectedLedgerVersion: 1,
			Rationale:             "read the run transcript; the rating matches what the attempts show",
		}, &commitResult)
	requireStatus(t, resp, body, http.StatusOK)
	if len(commitResult.Records) != 1 {
		t.Fatalf("review commit result records = %+v, want exactly one", commitResult.Records)
	}
	confirmRecord := commitResult.Records[0]
	if confirmRecord.Authority != ledger.AuthorityConfirmed {
		t.Fatalf("review record authority = %q, want %q", confirmRecord.Authority, ledger.AuthorityConfirmed)
	}
	if confirmRecord.SubjectRef.String() != grade.ID {
		t.Fatalf("review record subject_ref = %q, want the graded record %s", confirmRecord.SubjectRef, grade.ID)
	}

	// The original grade record itself is untouched -- a review never
	// rewrites its target, it only appends a review record referencing it.
	var records apipkg.LedgerRecordsOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID+"/ledger"), nil, &records)
	requireStatus(t, resp, body, http.StatusOK)
	var stillProposed bool
	for _, r := range records.Items {
		if r.ID == grade.ID {
			stillProposed = r.Authority == ledger.AuthorityProposed
		}
	}
	if !stillProposed {
		t.Fatalf("grade record %s is not still proposed in the ledger listing: %+v", grade.ID, records.Items)
	}
}

// TestCreateGradeHumanDirectLandsConfirmed is task t16's acceptance
// criterion 2's second half: a human actor's direct grade lands confirmed
// authority outside any review transaction -- internal/ledger's
// checkHumanAuthority carve-out for grade records (it is the human's own
// opinion, not a claim someone else must ratify).
func TestCreateGradeHumanDirectLandsConfirmed(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)

	humanActor := f.insertActorKind("ori", "human")
	evaluatedActor := f.insertActor("evaluated")

	var grade ledger.Record
	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/grades"), decisionAuthSecret,
		createGradeReq{
			Rating: 5, Rationale: "clean, verified myself",
			EvaluatedActorID: evaluatedActor, GradingActorID: humanActor,
		}, &grade)
	requireStatus(t, resp, body, http.StatusCreated)
	if grade.Origin.Kind != ledger.OriginHuman {
		t.Fatalf("origin.kind = %q, want %q", grade.Origin.Kind, ledger.OriginHuman)
	}
	if grade.Authority != ledger.AuthorityConfirmed {
		t.Fatalf("authority = %q, want %q", grade.Authority, ledger.AuthorityConfirmed)
	}
}

// TestCreateGradeSelfGradeRefused proves a grade whose grading actor equals
// the evaluated actor is refused with a structured 4xx naming
// ledger.RuleNoSelfGrade -- internal/ledger.CheckAuthority's self-promotion
// rule extended to opinion records, surfaced through classify() rather than
// re-implemented in the handler.
func TestCreateGradeSelfGradeRefused(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)

	actor := f.insertActor("solo")

	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/grades"), decisionAuthSecret,
		createGradeReq{Rating: 3, Rationale: "grading myself", EvaluatedActorID: actor, GradingActorID: actor}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	apiErr := decodeAPIError(t, body)
	if !strings.Contains(apiErr.Message, ledger.RuleNoSelfGrade) {
		t.Fatalf("error message = %q, want it to name rule %q", apiErr.Message, ledger.RuleNoSelfGrade)
	}
}

// TestCreateGradeRatingOutOfBoundsIsASchemaValidationError exercises the
// schema-validation error path: a rating outside grade.schema.json's 1-5
// bound, and an empty rationale, are both malformed documents rather than
// authority refusals, and both render as 400 in the documented Error shape
// (contracts.ValidationError, classified alongside ledger.AuthorityError —
// see internal/api/errors.go).
func TestCreateGradeRatingOutOfBoundsIsASchemaValidationError(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)

	graderAgent := f.insertActor("grader")
	evaluatedActor := f.insertActor("evaluated")

	t.Run("rating_too_high", func(t *testing.T) {
		resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/grades"), decisionAuthSecret,
			createGradeReq{Rating: 6, Rationale: "out of range", EvaluatedActorID: evaluatedActor, GradingActorID: graderAgent}, nil)
		requireStatus(t, resp, body, http.StatusBadRequest)
		decodeAPIError(t, body)
	})

	t.Run("rating_too_low", func(t *testing.T) {
		resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/grades"), decisionAuthSecret,
			createGradeReq{Rating: 0, Rationale: "out of range", EvaluatedActorID: evaluatedActor, GradingActorID: graderAgent}, nil)
		requireStatus(t, resp, body, http.StatusBadRequest)
		decodeAPIError(t, body)
	})

	t.Run("empty_rationale", func(t *testing.T) {
		resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/grades"), decisionAuthSecret,
			createGradeReq{Rating: 3, Rationale: "", EvaluatedActorID: evaluatedActor, GradingActorID: graderAgent}, nil)
		requireStatus(t, resp, body, http.StatusBadRequest)
		decodeAPIError(t, body)
	})
}

// TestCreateGradeUnknownRunOrActorIs404 covers every id this handler looks
// up before appending anything: the run itself, the grading actor, and the
// evaluated actor.
func TestCreateGradeUnknownRunOrActorIs404(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)

	graderAgent := f.insertActor("grader")
	evaluatedActor := f.insertActor("evaluated")

	t.Run("unknown_run", func(t *testing.T) {
		resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/does-not-exist/grades"),
			createGradeReq{Rating: 3, Rationale: "x", EvaluatedActorID: evaluatedActor, GradingActorID: graderAgent}, nil)
		requireStatus(t, resp, body, http.StatusNotFound)
		decodeAPIError(t, body)
	})

	t.Run("unknown_grading_actor", func(t *testing.T) {
		resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/grades"), decisionAuthSecret,
			createGradeReq{Rating: 3, Rationale: "x", EvaluatedActorID: evaluatedActor, GradingActorID: "does-not-exist"}, nil)
		requireStatus(t, resp, body, http.StatusNotFound)
		decodeAPIError(t, body)
	})

	t.Run("unknown_evaluated_actor", func(t *testing.T) {
		resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/grades"), decisionAuthSecret,
			createGradeReq{Rating: 3, Rationale: "x", EvaluatedActorID: "does-not-exist", GradingActorID: graderAgent}, nil)
		requireStatus(t, resp, body, http.StatusNotFound)
		decodeAPIError(t, body)
	})
}

// TestCreateGradeUnsupportedGradingActorKindRefused proves a grading actor
// registered as neither "human" nor "agent" (a runner, here) is refused
// with 400 rather than silently picking an origin for it -- CheckAuthority
// states no producer rule for a bare opinion from a runner/engine/validator
// (see internal/ledger/authority.go's origin switch), so this handler
// refuses before ever attempting the append.
func TestCreateGradeUnsupportedGradingActorKindRefused(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)

	runnerActor := f.insertActorKind("runner", "runner")
	evaluatedActor := f.insertActor("evaluated")

	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/grades"), decisionAuthSecret,
		createGradeReq{Rating: 3, Rationale: "x", EvaluatedActorID: evaluatedActor, GradingActorID: runnerActor}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)
}
