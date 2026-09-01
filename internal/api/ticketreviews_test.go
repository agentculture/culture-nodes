package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/auth"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// Task t14 (spec c11/h5, c16/h8, decision c40): the ticket page decides
// CLAIMS, not only human tasks.
//
// Three properties are covered here, and each one exists because the
// obvious implementation of this feature gets it wrong:
//
//   - The ledger version a decider attests to has to be the one the page
//     they read was rendered from (ticketpending.go:44-53). Serving the
//     records and letting the client fetch the version separately would
//     let a client attest to a frame it never showed anyone — so the
//     records and the version are one read, and a record appended after it
//     makes the existing conflict path refuse the submission.
//   - A ticket's claims live on SEVERAL runs, and one review is opened per
//     run at that run's own version (PRD §10.8). A batch therefore cannot
//     be all-or-nothing: a run whose version moved reports conflict while
//     every other run still commits.
//   - A reply's identity comes from the verified principal, on the engine
//     fact AND on the Jira mirror a person reads on the board.

// ticketVerdictConfirmedWord is the verdict word this surface documents —
// the state a record ends up in, spelled the way api/openapi/openapi.yaml
// spells it.
const ticketVerdictConfirmedWord = "confirmed"

type ticketReviewRunReq struct {
	RunID                 string   `json:"run_id"`
	ExpectedLedgerVersion int64    `json:"expected_ledger_version"`
	Records               []string `json:"records"`
	Verdict               string   `json:"verdict"`
}

type ticketReviewsReq struct {
	Runs            []ticketReviewRunReq `json:"runs"`
	Rationale       string               `json:"rationale"`
	ReviewerActorID string               `json:"reviewer_actor_id,omitempty"`
}

// newRun publishes minimal.workflow.yaml and creates one run against it.
// Publishing is content-addressed and idempotent, so the second call within
// one test re-publishes the same digest and answers 200 rather than 201 —
// both are accepted here, which is why this is not createMinimalRun.
func newRun(t *testing.T, f *fixture) apipkg.RunOut {
	t.Helper()
	var published apipkg.WorkflowVersionOut
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/workflows"),
		workflowSourceReq{Format: "yaml", Source: string(readFixtureWorkflow(t, "minimal.workflow.yaml"))}, &published)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("publish minimal workflow: status = %d; body = %s", resp.StatusCode, body)
	}
	var run apipkg.RunOut
	resp, body = doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/runs"),
		createRunReq{WorkflowDigest: published.Digest, Input: json.RawMessage(`{}`)}, &run)
	requireStatus(t, resp, body, http.StatusCreated)
	return run
}

// ticketRun creates a run addressed to ticketID the way the ticket
// projection finds one — by the runs.subject column listRuns filters on.
func ticketRun(t *testing.T, f *fixture, ticketID string) apipkg.RunOut {
	t.Helper()
	run := newRun(t, f)
	if _, err := f.store.Pool().Exec(t.Context(), `UPDATE runs SET subject=$2 WHERE id=$1`, run.ID, ticketID); err != nil {
		t.Fatalf("address run %s to %s: %v", run.ID, ticketID, err)
	}
	return run
}

func findTicketPendingRun(ticket apipkg.TicketOut, runID string) *apipkg.PendingDecisionRunOut {
	for i := range ticket.PendingRecords {
		if ticket.PendingRecords[i].RunID == runID {
			return &ticket.PendingRecords[i]
		}
	}
	return nil
}

func postTicketReviews(t *testing.T, f *fixture, ticketID string, req ticketReviewsReq, want int) apipkg.TicketReviewsResultOut {
	t.Helper()
	var result apipkg.TicketReviewsResultOut
	resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/tickets/"+ticketID+"/reviews"),
		decisionAuthSecret, req, &result)
	requireStatus(t, resp, body, want)
	return result
}

// TestTicketPendingRecordsAreServedAtTheVersionTheGuardRefusesAgainst is
// c11/h5: the version travels WITH the records, and it is the one the
// existing stale guard measures a submission against.
func TestTicketPendingRecordsAreServedAtTheVersionTheGuardRefusesAgainst(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	const ticketID = "SCRUM-140"
	run := ticketRun(t, f, ticketID)
	agent := f.insertActor("claimer")
	reviewer := f.insertActorKind("reviewer", "human")
	claim := appendAgentClaim(t, f, run.ID, agent, "the suite passed on spark")

	ticket := getTicketProjection(t, f, ticketID)
	group := findTicketPendingRun(ticket, run.ID)
	if group == nil {
		t.Fatalf("pending_records has no group for run %s: %+v", run.ID, ticket.PendingRecords)
	}
	if !containsPendingRecord(*group, claim.ID) {
		t.Fatalf("claim %s absent from run %s's pending records: %+v", claim.ID, run.ID, group.Records)
	}
	if group.LedgerVersion <= 0 {
		t.Fatalf("ledger_version = %d, want the version this same read saw", group.LedgerVersion)
	}
	served := group.LedgerVersion

	// The frame the page rendered is now out of date: someone appended.
	appendAgentClaim(t, f, run.ID, agent, "and a second claim landed after the read")

	result := postTicketReviews(t, f, ticketID, ticketReviewsReq{
		Runs: []ticketReviewRunReq{{
			RunID: run.ID, ExpectedLedgerVersion: served,
			Records: []string{claim.ID}, Verdict: "confirmed",
		}},
		Rationale:       "read the claim and re-ran the suite",
		ReviewerActorID: reviewer,
	}, http.StatusOK)

	if len(result.Runs) != 1 || result.Runs[0].Status != "conflict" {
		t.Fatalf("per-run results = %+v, want one conflict — the served version is stale", result.Runs)
	}
	if result.Runs[0].Message == "" {
		t.Errorf("a conflict with no message tells the decider nothing: %+v", result.Runs[0])
	}
	if result.CommittedRuns != 0 {
		t.Errorf("committed_runs = %d, want 0", result.CommittedRuns)
	}

	// Nothing was written: the claim is still awaiting a decision.
	after := getTicketProjection(t, f, ticketID)
	group = findTicketPendingRun(after, run.ID)
	if group == nil || !containsPendingRecord(*group, claim.ID) {
		t.Fatalf("claim %s stopped being pending after a REFUSED review: %+v", claim.ID, after.PendingRecords)
	}
	if group.LedgerVersion == served {
		t.Fatalf("ledger_version is still %d after an append — the version is not being re-read", served)
	}
}

// TestTicketReviewsCommitPerRunWhileAStaleRunConflicts is decision c40: one
// review per run, in order, and a stale run does not take the others down
// with it.
func TestTicketReviewsCommitPerRunWhileAStaleRunConflicts(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	const ticketID = "SCRUM-141"
	stale := ticketRun(t, f, ticketID)
	fresh := ticketRun(t, f, ticketID)
	agent := f.insertActor("claimer")
	reviewer := f.insertActorKind("reviewer", "human")
	staleClaim := appendAgentClaim(t, f, stale.ID, agent, "claim on the stale run")
	freshClaim := appendAgentClaim(t, f, fresh.ID, agent, "claim on the fresh run")

	ticket := getTicketProjection(t, f, ticketID)
	staleGroup, freshGroup := findTicketPendingRun(ticket, stale.ID), findTicketPendingRun(ticket, fresh.ID)
	if staleGroup == nil || freshGroup == nil {
		t.Fatalf("want a pending group per run, got %+v", ticket.PendingRecords)
	}

	// Only one of the two runs moves on after the page was rendered.
	appendAgentClaim(t, f, stale.ID, agent, "an append the decider never saw")

	result := postTicketReviews(t, f, ticketID, ticketReviewsReq{
		Runs: []ticketReviewRunReq{
			{RunID: stale.ID, ExpectedLedgerVersion: staleGroup.LedgerVersion, Records: []string{staleClaim.ID}, Verdict: "confirmed"},
			{RunID: fresh.ID, ExpectedLedgerVersion: freshGroup.LedgerVersion, Records: []string{freshClaim.ID}, Verdict: "rejected"},
		},
		Rationale:       "decided both from the ticket page",
		ReviewerActorID: reviewer,
	}, http.StatusOK)

	if len(result.Runs) != 2 {
		t.Fatalf("per-run results = %+v, want one per submitted run in order", result.Runs)
	}
	if result.Runs[0].RunID != stale.ID || result.Runs[0].Status != "conflict" {
		t.Errorf("first result = %+v, want conflict on the stale run %s", result.Runs[0], stale.ID)
	}
	if result.Runs[1].RunID != fresh.ID || result.Runs[1].Status != "committed" {
		t.Fatalf("second result = %+v, want the fresh run %s committed anyway", result.Runs[1], fresh.ID)
	}
	if result.Runs[1].ReviewID == "" {
		t.Errorf("a committed run reports no review_id: %+v", result.Runs[1])
	}
	if result.CommittedRuns != 1 {
		t.Errorf("committed_runs = %d, want 1", result.CommittedRuns)
	}

	// The committed verdict is a review record beside the claim, which is
	// still proposed — a review names records, it never rewrites them.
	after := getTicketProjection(t, f, ticketID)
	if group := findTicketPendingRun(after, fresh.ID); group != nil && containsPendingRecord(*group, freshClaim.ID) {
		t.Errorf("claim %s is still pending after being rejected: %+v", freshClaim.ID, group.Records)
	}
	if group := findTicketPendingRun(after, stale.ID); group == nil || !containsPendingRecord(*group, staleClaim.ID) {
		t.Errorf("claim %s on the conflicted run stopped being pending: %+v", staleClaim.ID, after.PendingRecords)
	}
	var kinds []string
	for _, entry := range after.Ledger {
		if entry.RunID != fresh.ID {
			continue
		}
		for _, rec := range entry.Records {
			if string(rec.RecordType) == "review" {
				kinds = append(kinds, string(rec.Authority))
			}
			if rec.ID == freshClaim.ID && string(rec.Authority) != "proposed" {
				t.Errorf("the reviewed claim was rewritten: authority = %s", rec.Authority)
			}
		}
	}
	if len(kinds) != 1 || kinds[0] != "rejected" {
		t.Errorf("review records on the committed run = %v, want exactly one rejected", kinds)
	}
}

// TestTicketReviewsRefuseAnUngroundedDecision: rationale is required, the
// reviewer must be a registered human, and a run that is not this ticket's
// cannot be decided through the ticket's own route. Every one of these is
// refused BEFORE anything is written.
func TestTicketReviewsRefuseAnUngroundedDecision(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	const ticketID = "SCRUM-142"
	run := ticketRun(t, f, ticketID)
	agent := f.insertActor("claimer")
	human := f.insertActorKind("reviewer", "human")
	claim := appendAgentClaim(t, f, run.ID, agent, "decide me")
	elsewhere := newRun(t, f)
	version := currentLedgerVersion(t, f, run.ID)
	good := ticketReviewRunReq{RunID: run.ID, ExpectedLedgerVersion: version, Records: []string{claim.ID}, Verdict: "confirmed"}

	for _, tc := range []struct {
		name string
		req  ticketReviewsReq
	}{
		{"no rationale", ticketReviewsReq{Runs: []ticketReviewRunReq{good}, ReviewerActorID: human}},
		{"no reviewer", ticketReviewsReq{Runs: []ticketReviewRunReq{good}, Rationale: "read it"}},
		{"reviewer is an agent", ticketReviewsReq{Runs: []ticketReviewRunReq{good}, Rationale: "read it", ReviewerActorID: agent}},
		{"reviewer is not registered", ticketReviewsReq{Runs: []ticketReviewRunReq{good}, Rationale: "read it", ReviewerActorID: "01JZZZNOSUCHACTOR00000000"}},
		{"no runs", ticketReviewsReq{Runs: []ticketReviewRunReq{}, Rationale: "read it", ReviewerActorID: human}},
		{"no records", ticketReviewsReq{Runs: []ticketReviewRunReq{{RunID: run.ID, ExpectedLedgerVersion: version, Verdict: "confirmed"}}, Rationale: "read it", ReviewerActorID: human}},
		{"unknown verdict", ticketReviewsReq{Runs: []ticketReviewRunReq{{RunID: run.ID, ExpectedLedgerVersion: version, Records: []string{claim.ID}, Verdict: "maybe"}}, Rationale: "read it", ReviewerActorID: human}},
		{"a run this ticket does not own", ticketReviewsReq{Runs: []ticketReviewRunReq{{RunID: elsewhere.ID, ExpectedLedgerVersion: version, Records: []string{claim.ID}, Verdict: "confirmed"}}, Rationale: "read it", ReviewerActorID: human}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doJSONBearer(t, f.client, http.MethodPost, f.url("/v1alpha1/tickets/"+ticketID+"/reviews"),
				decisionAuthSecret, tc.req, nil)
			requireStatus(t, resp, body, http.StatusBadRequest)
			decodeAPIError(t, body)
		})
	}

	// None of the refusals decided anything.
	if group := findTicketPendingRun(getTicketProjection(t, f, ticketID), run.ID); group == nil || !containsPendingRecord(*group, claim.ID) {
		t.Fatalf("claim %s stopped being pending after refusals alone", claim.ID)
	}
}

// TestTicketReviewsRequireTheDecisionGate: this route writes human-authority
// records into the ledger, exactly as POST /reviews/{id}/commit does, so it
// carries the same gate.
func TestTicketReviewsRequireTheDecisionGate(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	resp, body := doJSON(t, f.client, http.MethodPost, f.url("/v1alpha1/tickets/SCRUM-143/reviews"),
		ticketReviewsReq{Rationale: "no token", Runs: []ticketReviewRunReq{}}, nil)
	requireStatus(t, resp, body, http.StatusUnauthorized)
	decodeAPIError(t, body)
}

// TestTicketReviewsStampThePrincipalsActorAsTheReviewer: signed in, the
// decider is who Access says they are — the body's reviewer_actor_id is a
// display hint that is overridden and warned about, exactly as every other
// write route on this ticket treats its free-text identity field (task t10).
//
// The Access listener is a SECOND server over the same store: only
// AccessHandler reads Cf-Access-Jwt-Assertion, and the authless fixture
// beside it is what stages the run and the claim.
func TestTicketReviewsStampThePrincipalsActorAsTheReviewer(t *testing.T) {
	f := newFixture(t)
	const ticketID = "SCRUM-146"
	run := ticketRun(t, f, ticketID)
	agent := f.insertActor("claimer")
	claim := appendAgentClaim(t, f, run.ID, agent, "decided by whoever is signed in")
	bound := insertPrincipalTestActor(t, f.store, f.nsID, "signed-in-reviewer")
	other := f.insertActorKind("named-in-the-body", "human")
	if _, err := f.store.BindIdentity(context.Background(), f.nsID, "cloudflare-access", "reviewer-sub", bound, []string{"approver"}); err != nil {
		t.Fatal(err)
	}
	access, err := apipkg.NewServer(f.store, f.nsID, apipkg.WithPrincipalVerifier(verifierFunc(func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{Subject: "reviewer-sub", Kind: auth.PrincipalInteractive}, nil
	})))
	if err != nil {
		t.Fatal(err)
	}

	group := findTicketPendingRun(getTicketProjection(t, f, ticketID), run.ID)
	if group == nil {
		t.Fatalf("no pending group for run %s", run.ID)
	}
	body, err := json.Marshal(ticketReviewsReq{
		Runs: []ticketReviewRunReq{{
			RunID: run.ID, ExpectedLedgerVersion: group.LedgerVersion,
			Records: []string{claim.ID}, Verdict: ticketVerdictConfirmedWord,
		}},
		Rationale:       "read the claim on the ticket page",
		ReviewerActorID: other,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1alpha1/tickets/"+ticketID+"/reviews", strings.NewReader(string(body)))
	req.Header.Set("Cf-Access-Jwt-Assertion", "assertion")
	rr := httptest.NewRecorder()
	access.AccessHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"warning"`) || !strings.Contains(rr.Body.String(), other) {
		t.Fatalf("an overridden reviewer must be reported back, not silently swapped: %s", rr.Body.String())
	}

	var origins []string
	rows, err := f.store.Pool().Query(t.Context(),
		`SELECT origin_actor_id FROM ledger_records WHERE run_id=$1 AND record_type='review'`, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var origin string
		if err := rows.Scan(&origin); err != nil {
			t.Fatal(err)
		}
		origins = append(origins, origin)
	}
	if len(origins) != 1 || origins[0] != bound {
		t.Fatalf("review record origins = %v, want exactly the principal's actor %s (not the body's %s)", origins, bound, other)
	}
}

// TestTicketReplyOriginAndMirrorNameThePrincipalsActor is c16/h8: a signed-in
// person's reply carries their actor as the engine fact's origin (t10) and
// their VERIFIED display name — read from the bound actor, never from the
// free-text replier the body sent — in the Jira mirror a person reads.
func TestTicketReplyOriginAndMirrorNameThePrincipalsActor(t *testing.T) {
	s := requireStore(t)
	nsID := pgtest.MustNamespace(t, s, "principal-reply-origin").ID
	bound := insertPrincipalTestActor(t, s, nsID, "verified-replier")
	other := insertPrincipalTestActor(t, s, nsID, "free-text-replier")
	if _, err := s.Pool().Exec(context.Background(),
		`UPDATE actors SET metadata='{"display_name":"Ada Lovelace"}'::jsonb WHERE id=$1`, bound); err != nil {
		t.Fatal(err)
	}
	var namedActorKey string
	if err := s.Pool().QueryRow(context.Background(), `SELECT actor_key FROM actors WHERE id=$1`, other).Scan(&namedActorKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindIdentity(context.Background(), nsID, "cloudflare-access", "reply-sub", bound, []string{"approver"}); err != nil {
		t.Fatal(err)
	}
	srv, err := apipkg.NewServer(s, nsID, apipkg.WithPrincipalVerifier(verifierFunc(func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{Subject: "reply-sub", Email: "ada@example.test", Kind: auth.PrincipalInteractive}, nil
	})))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1alpha1/tickets/SCRUM-144/replies",
		strings.NewReader(`{"replier":"`+other+`","text":"Use option A."}`))
	req.Header.Set("Cf-Access-Jwt-Assertion", "assertion")
	rr := httptest.NewRecorder()
	srv.AccessHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("reply status = %d: %s", rr.Code, rr.Body.String())
	}

	// The engine fact's origin is the principal's actor (task t10).
	var payload string
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT payload::text FROM signal_events WHERE namespace_id=$1 AND payload->>'id'='SCRUM-144'`, nsID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var fact struct {
		Replier string `json:"replier"`
		Origin  struct {
			Kind    string `json:"kind"`
			Replier string `json:"replier"`
		} `json:"origin"`
		Answer struct {
			Body string `json:"body"`
		} `json:"answer"`
	}
	if err := json.Unmarshal([]byte(payload), &fact); err != nil {
		t.Fatal(err)
	}
	if fact.Origin.Kind != "human" || fact.Origin.Replier != bound || fact.Replier != bound {
		t.Fatalf("fact origin = %+v, want the principal's actor %s, not the body's %s", fact, bound, other)
	}
	if fact.Answer.Body != "Use option A." {
		t.Fatalf("the fact's body must stay the text the person wrote: %q", fact.Answer.Body)
	}

	// The Jira mirror reads the text, then the VERIFIED display name.
	var mirror string
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT payload->>'comment' FROM jira_ticket_report_outbox WHERE namespace_id=$1 AND issue_key='SCRUM-144' AND phase='reply'`, nsID).Scan(&mirror); err != nil {
		t.Fatal(err)
	}
	if mirror != "Use option A.\n\nvia Ada Lovelace" {
		t.Fatalf("mirror = %q, want the text then 'via' the bound actor's display name", mirror)
	}
	if strings.Contains(mirror, other) || strings.Contains(mirror, namedActorKey) {
		t.Fatalf("mirror names the free-text replier: %q", mirror)
	}
}

// A bound actor with no display name in its metadata is still named by
// something a person can read back to the registry — its actor key — never
// by the free text the body sent.
func TestTicketReplyMirrorFallsBackToTheBoundActorsKey(t *testing.T) {
	s := requireStore(t)
	nsID := pgtest.MustNamespace(t, s, "principal-reply-fallback").ID
	bound := insertPrincipalTestActor(t, s, nsID, "nameless-replier")
	var actorKey string
	if err := s.Pool().QueryRow(context.Background(), `SELECT actor_key FROM actors WHERE id=$1`, bound).Scan(&actorKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindIdentity(context.Background(), nsID, "cloudflare-access", "fallback-sub", bound, []string{"approver"}); err != nil {
		t.Fatal(err)
	}
	srv, err := apipkg.NewServer(s, nsID, apipkg.WithPrincipalVerifier(verifierFunc(func(context.Context, string) (auth.Principal, error) {
		return auth.Principal{Subject: "fallback-sub", Kind: auth.PrincipalInteractive}, nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1alpha1/tickets/SCRUM-145/replies",
		strings.NewReader(`{"replier":"someone else entirely","text":"Option B."}`))
	req.Header.Set("Cf-Access-Jwt-Assertion", "assertion")
	rr := httptest.NewRecorder()
	srv.AccessHandler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("reply status = %d: %s", rr.Code, rr.Body.String())
	}
	var mirror string
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT payload->>'comment' FROM jira_ticket_report_outbox WHERE namespace_id=$1 AND issue_key='SCRUM-145' AND phase='reply'`, nsID).Scan(&mirror); err != nil {
		t.Fatal(err)
	}
	if mirror != "Option B.\n\nvia "+actorKey {
		t.Fatalf("mirror = %q, want 'via' the bound actor's key %q", mirror, actorKey)
	}
	if strings.Contains(mirror, "someone else entirely") {
		t.Fatalf("mirror names the free-text replier: %q", mirror)
	}
}
