package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// This file covers the affirmative half of PRD §10.4 as a product surface
// (task t30, issue #99): finding the claims that await a decision, and
// deciding one so that the decision is itself a ledger record naming who
// decided and why.
//
// The refusal half is tested where it lives — internal/ledger's
// authority_test.go and reviewer_test.go. What these tests add is that the
// refusal survives the trip through HTTP: an agent actor cannot decide a
// claim by presenting itself as the reviewer of one.

// appendAgentClaim appends one agent-origin completion claim to run and
// returns it — the thing a human then has to decide.
func appendAgentClaim(t *testing.T, f *fixture, runID, actorID, statement string) ledger.Record {
	t.Helper()
	rec, err := f.api.Ledger.Append(t.Context(), ledger.Record{
		RecordType: ledger.RecordClaim,
		RunID:      runID,
		Origin:     ledger.Origin{Kind: ledger.OriginAgent, ActorID: actorID},
		Authority:  ledger.AuthorityProposed,
		Data:       json.RawMessage(`{"statement":` + mustQuote(statement) + `,"kind":"completion"}`),
	})
	if err != nil {
		t.Fatalf("append agent claim: %v", err)
	}
	return rec
}

func mustQuote(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// TestPendingDecisionsListsProposedClaimsUntilTheyAreDecided is the
// discoverability half: "what is awaiting my decision" has to be a query, not
// a hand-maintained manifest file. Before t30 the operator kept
// docs/triage/cycle-runs.txt by hand so scripts/ledger-gate.py had something
// to read.
func TestPendingDecisionsListsProposedClaimsUntilTheyAreDecided(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	agent := f.insertActor("claimer")
	reviewer := f.insertActorKind("reviewer", "human")

	claim := appendAgentClaim(t, f, run.ID, agent, "the suite passed on spark")

	var pending apipkg.PendingDecisionListOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/pending-decisions"), nil, &pending)
	requireStatus(t, resp, body, http.StatusOK)

	group := findPendingRun(pending, run.ID)
	if group == nil {
		t.Fatalf("run %s absent from pending-decisions: %+v", run.ID, pending.Items)
	}
	if !containsPendingRecord(*group, claim.ID) {
		t.Fatalf("claim %s absent from run %s's pending records: %+v", claim.ID, run.ID, group.Records)
	}
	if group.LedgerVersion <= 0 {
		t.Fatalf("ledger_version = %d, want the run's real version — the caller needs it to open a review",
			group.LedgerVersion)
	}
	if pending.RecordCount < 1 {
		t.Fatalf("record_count = %d, want at least 1", pending.RecordCount)
	}

	// Deciding it removes it from the queue. A rejection counts as decided
	// just as much as a confirmation does: the question was answered.
	decideAll(t, f, run.ID, reviewer, group.LedgerVersion, []string{claim.ID}, "confirm",
		"re-ran the suite on spark and read the output")

	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/pending-decisions"), nil, &pending)
	requireStatus(t, resp, body, http.StatusOK)
	if group := findPendingRun(pending, run.ID); group != nil && containsPendingRecord(*group, claim.ID) {
		t.Fatalf("claim %s still pending after being decided: %+v", claim.ID, group.Records)
	}

	// ?run_id= narrows to one run, which is what a run view asks for.
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/pending-decisions?run_id="+run.ID), nil, &pending)
	requireStatus(t, resp, body, http.StatusOK)
	for _, item := range pending.Items {
		if item.RunID != run.ID {
			t.Fatalf("run_id filter returned run %s", item.RunID)
		}
	}
}

// TestPendingDecisionsRejectsAnUnknownFilter: a filter the surface does not
// understand is refused, never silently ignored — a query that quietly drops
// the narrowing the caller asked for answers a different question than the
// one asked, and looks authoritative doing it.
func TestPendingDecisionsRejectsAnUnknownFilter(t *testing.T) {
	f := newFixture(t)

	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/pending-decisions?authority=proposed"), nil, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)
}

// TestDecideAProposedClaimRecordsWhoDecidedAndWhy is acceptance criterion 1:
// a proposed claim is confirmed through the product, and the decision is
// itself an immutable ledger record naming the decider and the stated reason.
func TestDecideAProposedClaimRecordsWhoDecidedAndWhy(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	agent := f.insertActor("claimer")
	reviewer := f.insertActorKind("reviewer", "human")

	claim := appendAgentClaim(t, f, run.ID, agent, "the collector received a trace")
	version := currentLedgerVersion(t, f, run.ID)

	const why = "read the trace in the collector; three seam spans, one trace id"
	result := decideAll(t, f, run.ID, reviewer, version, []string{claim.ID}, "confirm", why)
	if len(result.Records) != 1 {
		t.Fatalf("commit produced %d records, want 1", len(result.Records))
	}

	decision := result.Records[0]
	if decision.RecordType != ledger.RecordReview {
		t.Errorf("decision record_type = %q, want review", decision.RecordType)
	}
	if decision.Authority != ledger.AuthorityConfirmed {
		t.Errorf("decision authority = %q, want confirmed", decision.Authority)
	}
	if decision.Origin.Kind != ledger.OriginHuman || decision.Origin.ActorID != reviewer {
		t.Errorf("decision origin = %+v, want the human reviewer %s", decision.Origin, reviewer)
	}
	if decision.SubjectRef.String() != claim.ID {
		t.Errorf("decision subject_ref = %q, want the claim %q", decision.SubjectRef.String(), claim.ID)
	}

	var payload struct {
		Verdict      string   `json:"verdict"`
		Rationale    string   `json:"rationale"`
		ReviewedRefs []string `json:"reviewed_refs"`
	}
	if err := json.Unmarshal(decision.Data, &payload); err != nil {
		t.Fatalf("decode decision payload: %v", err)
	}
	if payload.Verdict != "confirm" || payload.Rationale != why {
		t.Fatalf("decision payload verdict/rationale = %q/%q, want confirm/%q", payload.Verdict, payload.Rationale, why)
	}

	// The claim itself is untouched — records are immutable, so the decision
	// is an appended record that names it, never an edit of it. (This is the
	// behaviour that broke the first stage-exit gate: a confirmed claim still
	// reads authority=proposed forever.)
	var listed apipkg.LedgerRecordsOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+run.ID+"/ledger"), nil, &listed)
	requireStatus(t, resp, body, http.StatusOK)
	for _, rec := range listed.Items {
		if rec.ID == claim.ID && rec.Authority != ledger.AuthorityProposed {
			t.Fatalf("the reviewed claim's authority changed to %q; records are immutable", rec.Authority)
		}
	}
}

// TestDecideRefusesAnAgentReviewer is acceptance criterion 2 at the product
// boundary: an agent-origin actor cannot decide its own claim even by driving
// the HTTP decision surface directly and naming itself as the reviewer.
//
// Stub ledger.checkReviewerIsHuman and this test fails — the commit succeeds
// and the agent's confirmation of its own claim lands in the ledger.
func TestDecideRefusesAnAgentReviewer(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	agent := f.insertActor("claimer")

	claim := appendAgentClaim(t, f, run.ID, agent, "I did the work and it is fine")
	version := currentLedgerVersion(t, f, run.ID)

	var review apipkg.ReviewRequestOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/reviews"),
		decisionAuthSecret,
		createReviewReq{RecordIDs: []string{claim.ID}, LedgerVersion: version, ReviewerActorID: agent}, &review)
	requireStatus(t, resp, body, http.StatusCreated)

	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/reviews/"+review.ID+"/commit"),
		decisionAuthSecret,
		commitReviewReq{
			Decisions:             map[string]string{claim.ID: "confirm"},
			ExpectedLedgerVersion: version,
			Rationale:             "I am sure of my own work",
		}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	apiErr := decodeAPIError(t, body)
	if !strings.Contains(apiErr.Message, ledger.RuleReviewerNotHuman) {
		t.Fatalf("refusal message %q does not name the rule %q", apiErr.Message, ledger.RuleReviewerNotHuman)
	}

	// Nothing was written: the claim is still awaiting a decision.
	var pending apipkg.PendingDecisionListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/pending-decisions?run_id="+run.ID), nil, &pending)
	requireStatus(t, resp, body, http.StatusOK)
	group := findPendingRun(pending, run.ID)
	if group == nil || !containsPendingRecord(*group, claim.ID) {
		t.Fatalf("claim %s no longer pending after a REFUSED self-decision: %+v", claim.ID, pending.Items)
	}
}

// TestCommitReviewRequiresAStatedRationale: the decision surface will not
// record a verdict with no reason. A confirmation with no stated reason
// cannot be told apart from an unread one.
func TestCommitReviewRequiresAStatedRationale(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	agent := f.insertActor("claimer")
	reviewer := f.insertActorKind("reviewer", "human")

	claim := appendAgentClaim(t, f, run.ID, agent, "reasonless")
	version := currentLedgerVersion(t, f, run.ID)

	var review apipkg.ReviewRequestOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/reviews"),
		decisionAuthSecret,
		createReviewReq{RecordIDs: []string{claim.ID}, LedgerVersion: version, ReviewerActorID: reviewer}, &review)
	requireStatus(t, resp, body, http.StatusCreated)

	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/reviews/"+review.ID+"/commit"),
		decisionAuthSecret,
		commitReviewReq{
			Decisions:             map[string]string{claim.ID: "confirm"},
			ExpectedLedgerVersion: version,
		}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)
}

// TestCreateReviewRequiresANamedReviewer: a review with no reviewer is
// refused where it is created, not two calls later at commit time. The
// ledger refuses it either way; this is about telling the caller which field
// is missing while they can still fix it.
func TestCreateReviewRequiresANamedReviewer(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	agent := f.insertActor("claimer")

	claim := appendAgentClaim(t, f, run.ID, agent, "unattributed")
	version := currentLedgerVersion(t, f, run.ID)

	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/reviews"),
		decisionAuthSecret,
		createReviewReq{RecordIDs: []string{claim.ID}, LedgerVersion: version}, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)
}

// TestDecisionSurfaceRequiresTheDecisionToken: the review routes write
// human-authority records into the ledger on whoever presents the token,
// exactly as POST /human-tasks/{id}/decision does, so they carry the same
// gate. Before t30 they were the one unauthenticated way to write a
// confirmed record.
func TestDecisionSurfaceRequiresTheDecisionToken(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, _ := createMinimalRun(t, f)
	agent := f.insertActor("claimer")
	reviewer := f.insertActorKind("reviewer", "human")

	claim := appendAgentClaim(t, f, run.ID, agent, "gated")
	version := currentLedgerVersion(t, f, run.ID)
	createBody := createReviewReq{RecordIDs: []string{claim.ID}, LedgerVersion: version, ReviewerActorID: reviewer}

	// No token at all.
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/reviews"), createBody, nil)
	requireStatus(t, resp, body, http.StatusUnauthorized)
	decodeAPIError(t, body)

	// A wrong token.
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/reviews"),
		"not-the-configured-secret", createBody, nil)
	requireStatus(t, resp, body, http.StatusUnauthorized)
	decodeAPIError(t, body)

	// The commit route is gated too, independently of the create route.
	var review apipkg.ReviewRequestOut
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+run.ID+"/reviews"),
		decisionAuthSecret, createBody, &review)
	requireStatus(t, resp, body, http.StatusCreated)

	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/reviews/"+review.ID+"/commit"),
		commitReviewReq{
			Decisions:             map[string]string{claim.ID: "confirm"},
			ExpectedLedgerVersion: version,
			Rationale:             "no token presented",
		}, nil)
	requireStatus(t, resp, body, http.StatusUnauthorized)
	decodeAPIError(t, body)
}

// --- helpers ---------------------------------------------------------------

func currentLedgerVersion(t *testing.T, f *fixture, runID string) int64 {
	t.Helper()
	var listed apipkg.LedgerRecordsOut
	resp, body := doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/runs/"+runID+"/ledger"), nil, &listed)
	requireStatus(t, resp, body, http.StatusOK)
	return listed.LedgerVersion
}

// decideAll opens a review over recordIDs and commits one verdict across all
// of them — the two-call shape scripts/decide-claims.py and the web Decisions
// view both drive.
func decideAll(t *testing.T, f *fixture, runID, reviewer string, version int64, recordIDs []string, verdict, why string) apipkg.ReviewCommitResultOut {
	t.Helper()

	var review apipkg.ReviewRequestOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/runs/"+runID+"/reviews"),
		decisionAuthSecret,
		createReviewReq{RecordIDs: recordIDs, LedgerVersion: version, ReviewerActorID: reviewer}, &review)
	requireStatus(t, resp, body, http.StatusCreated)

	decisions := make(map[string]string, len(recordIDs))
	for _, id := range recordIDs {
		decisions[id] = verdict
	}

	var result apipkg.ReviewCommitResultOut
	resp, body = doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/reviews/"+review.ID+"/commit"),
		decisionAuthSecret,
		commitReviewReq{Decisions: decisions, ExpectedLedgerVersion: version, Rationale: why}, &result)
	requireStatus(t, resp, body, http.StatusOK)
	return result
}

func findPendingRun(list apipkg.PendingDecisionListOut, runID string) *apipkg.PendingDecisionRunOut {
	for i := range list.Items {
		if list.Items[i].RunID == runID {
			return &list.Items[i]
		}
	}
	return nil
}

func containsPendingRecord(group apipkg.PendingDecisionRunOut, recordID string) bool {
	for _, rec := range group.Records {
		if rec.ID == recordID {
			return true
		}
	}
	return false
}
