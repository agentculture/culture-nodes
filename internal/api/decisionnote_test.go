package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apipkg "github.com/agentculture/culture-nodes/internal/api"
	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// Task t46 (owner decision d19): every decision offers an optional note.
// A human task's note is recorded on the ledger decision record, as the
// confirming review's rationale, and on the decided task; a review commit's
// rationale stays required (decisions_test.go). These tests pin the note's
// three landing places, its bound, and that a decision without one is
// exactly what it was before.

// decisionRecordsOf splits a decision result's ledger records into the
// decision record and the review record(s) confirming it.
func decisionRecordsOf(t *testing.T, result apipkg.HumanTaskDecisionResultOut) (ledger.Record, []ledger.Record) {
	t.Helper()
	var decision *ledger.Record
	var reviews []ledger.Record
	for i := range result.LedgerRecords {
		rec := result.LedgerRecords[i]
		switch rec.RecordType {
		case ledger.RecordDecision:
			decision = &rec
		case ledger.RecordReview:
			reviews = append(reviews, rec)
		}
	}
	if decision == nil || len(reviews) == 0 {
		t.Fatalf("result carries no decision+review pair: %+v", result.LedgerRecords)
	}
	return *decision, reviews
}

func decodeObject(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func TestHumanTaskDecisionRecordsTheNote(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	_, task := advanceToReview(t, f)

	resp, body := authedDecide(t, f, task.ID, decisionAuthSecret, decideHumanTaskReq{
		Outcome:               "approved",
		DeciderActorID:        f.insertActorKind("approver", "human"),
		ExpectedLedgerVersion: 0,
		Note:                  "  read the diff; the risky part is behind a flag  ",
	})
	requireStatus(t, resp, body, http.StatusOK)
	var result apipkg.HumanTaskDecisionResultOut
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	const want = "read the diff; the risky part is behind a flag"
	if result.Note != want {
		t.Fatalf("result.note = %q, want the trimmed note %q", result.Note, want)
	}

	decision, reviews := decisionRecordsOf(t, result)
	if got := decodeObject(t, decision.Data)["note"]; got != want {
		t.Fatalf("decision record note = %v, want %q", got, want)
	}
	for _, review := range reviews {
		if got := decodeObject(t, review.Data)["rationale"]; got != want {
			t.Fatalf("review record rationale = %v, want the note %q", got, want)
		}
	}

	// The decided task returns it — the Decided tab reads this field.
	var fetched apipkg.HumanTaskOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks/"+task.ID), nil, &fetched)
	requireStatus(t, resp, body, http.StatusOK)
	if fetched.Note != want {
		t.Fatalf("decided task note = %q, want %q", fetched.Note, want)
	}
	if got := decodeObject(t, fetched.Response)["note"]; got != want {
		t.Fatalf("decided task response.note = %v, want %q", got, want)
	}
}

func TestHumanTaskDecisionWithoutNoteWritesNoNote(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	_, task := advanceToReview(t, f)

	resp, body := authedDecide(t, f, task.ID, decisionAuthSecret, decideHumanTaskReq{
		Outcome:               "approved",
		DeciderActorID:        f.insertActorKind("approver", "human"),
		ExpectedLedgerVersion: 0,
		Note:                  "   ", // blank is no note, not an empty one
	})
	requireStatus(t, resp, body, http.StatusOK)
	var result apipkg.HumanTaskDecisionResultOut
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.RunState != string(engine.RunCompleted) || result.Note != "" {
		t.Fatalf("result = state %q note %q, want completed and no note", result.RunState, result.Note)
	}
	decision, reviews := decisionRecordsOf(t, result)
	if _, ok := decodeObject(t, decision.Data)["note"]; ok {
		t.Fatalf("decision record carries a note key with no note: %s", decision.Data)
	}
	for _, review := range reviews {
		if _, ok := decodeObject(t, review.Data)["rationale"]; ok {
			t.Fatalf("review record carries a rationale nobody wrote: %s", review.Data)
		}
	}
	var fetched apipkg.HumanTaskOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks/"+task.ID), nil, &fetched)
	requireStatus(t, resp, body, http.StatusOK)
	if fetched.Note != "" || strings.Contains(string(fetched.Response), `"note"`) {
		t.Fatalf("decided task carries a note: %q / %s", fetched.Note, fetched.Response)
	}
}

func TestHumanTaskDecisionNoteIsBounded(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	_, task := advanceToReview(t, f)
	decider := f.insertActorKind("approver", "human")

	resp, body := authedDecide(t, f, task.ID, decisionAuthSecret, decideHumanTaskReq{
		Outcome:               "approved",
		DeciderActorID:        decider,
		ExpectedLedgerVersion: 0,
		Note:                  strings.Repeat("é", engine.MaxDecisionNoteRunes+1),
	})
	requireStatus(t, resp, body, http.StatusBadRequest)
	decodeAPIError(t, body)

	var fetched apipkg.HumanTaskOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/human-tasks/"+task.ID), nil, &fetched)
	requireStatus(t, resp, body, http.StatusOK)
	if fetched.Status != "pending" {
		t.Fatalf("status = %q after a refused over-long note, want pending", fetched.Status)
	}

	// Exactly at the bound (counted in characters, not bytes) is accepted.
	resp, body = authedDecide(t, f, task.ID, decisionAuthSecret, decideHumanTaskReq{
		Outcome:               "approved",
		DeciderActorID:        decider,
		ExpectedLedgerVersion: 0,
		Note:                  strings.Repeat("é", engine.MaxDecisionNoteRunes),
	})
	requireStatus(t, resp, body, http.StatusOK)
}

// TestReviewedRecordsListsDecidedClaimsNotTaskDecisions: the Decided tab's
// review half. A committed review of a claim is listed with its verdict,
// reviewer, rationale and the claim itself; a human task's own decision
// (already listed as a decided task) is not listed a second time.
func TestReviewedRecordsListsDecidedClaimsNotTaskDecisions(t *testing.T) {
	f := newFixtureWithDecisionAuth(t, decisionAuthSecret)
	run, task := advanceToReview(t, f)
	agent := f.insertActor("claimer")
	reviewer := f.insertActorKind("reviewer", "human")

	claim := appendAgentClaim(t, f, run.ID, agent, "the suite passed")
	const why = "re-ran the suite and read the output"
	decideAll(t, f, run.ID, reviewer, currentLedgerVersion(t, f, run.ID), []string{claim.ID}, "reject", why)

	resp, body := authedDecide(t, f, task.ID, decisionAuthSecret, decideHumanTaskReq{
		Outcome:               "approved",
		DeciderActorID:        reviewer,
		ExpectedLedgerVersion: currentLedgerVersion(t, f, run.ID),
		Note:                  "fine",
	})
	requireStatus(t, resp, body, http.StatusOK)

	var listed apipkg.ReviewedRecordListOut
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/reviewed-records?run_id="+run.ID), nil, &listed)
	requireStatus(t, resp, body, http.StatusOK)
	if len(listed.Items) != 1 {
		t.Fatalf("reviewed-records = %d items, want exactly the claim's review: %+v", len(listed.Items), listed.Items)
	}
	got := listed.Items[0]
	if got.Record.ID != claim.ID || got.Verdict != "reject" || got.Rationale != why || got.ReviewerActorID != reviewer {
		t.Fatalf("reviewed record = %+v, want claim %s rejected by %s because %q", got, claim.ID, reviewer, why)
	}
	if got.Record.RecordType != string(ledger.RecordClaim) || !strings.Contains(string(got.Record.Data), "the suite passed") {
		t.Fatalf("reviewed record's claim = %+v, want the claim rendered in full", got.Record)
	}

	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/reviewed-records?authority=confirmed"), nil, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
	resp, body = doJSON(t, f.client, http.MethodGet, f.url("/v1alpha1/reviewed-records?cursor=nope"), nil, nil)
	requireStatus(t, resp, body, http.StatusBadRequest)
}
