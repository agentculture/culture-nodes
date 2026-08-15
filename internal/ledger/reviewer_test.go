package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestCommitReviewRefusesANonHumanReviewer is the affirmative half's guard
// rail (task t30, issue #99). CommitReview stamps the review records it
// appends with `Origin{Kind: OriginHuman, ActorID: <reviewer>}` — so without
// this check the origin kind is a value the ledger asserts on the caller's
// behalf, not a fact about the named actor, and an agent could decide its own
// claim simply by naming itself as the reviewer.
//
// The refusal is what makes "an agent-origin actor cannot decide its own
// claim" true of the whole product rather than only of Ledger.Append: stub the
// kind check out of CommitReview and this test fails, because the agent's
// confirmation of its own claim goes through.
func TestCommitReviewRefusesANonHumanReviewer(t *testing.T) {
	ctx := context.Background()
	l, store := newTestLedger(t)

	// The agent's own completion claim, which it then tries to confirm.
	claim := mustAppend(t, l, claimRecord(t, "I ran the suite and it passed"))
	version := mustVersion(t, l, testRunID)

	req, err := l.CreateReviewRequest(ctx, testRunID, []string{claim.ID}, version, ledger.WithReviewer(testAgent))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}

	before := store.count()
	_, err = l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{claim.ID: ledger.VerdictConfirm}, version,
		ledger.WithRationale("the agent vouching for itself"))

	var authErr *ledger.AuthorityError
	if !errors.As(err, &authErr) || authErr.Rule != ledger.RuleReviewerNotHuman {
		t.Fatalf("CommitReview error = %v, want an AuthorityError with rule %s", err, ledger.RuleReviewerNotHuman)
	}
	if authErr.ActorID != testAgent {
		t.Errorf("refusal names actor %q, want the reviewer %q", authErr.ActorID, testAgent)
	}
	if authErr.Origin != ledger.OriginAgent {
		t.Errorf("refusal names origin %q, want the reviewer's REGISTERED kind %q — reporting `human` here "+
			"would repeat the very assumption the check exists to refuse", authErr.Origin, ledger.OriginAgent)
	}
	if got := store.count(); got != before {
		t.Fatalf("%d record(s) appended by a refused review, want 0", got-before)
	}

	// And the claim is still exactly what it was: an undecided proposal.
	after, err := l.Record(ctx, claim.ID)
	if err != nil {
		t.Fatalf("re-read the claim: %v", err)
	}
	if after.Authority != ledger.AuthorityProposed {
		t.Fatalf("claim authority = %q, want it left %q", after.Authority, ledger.AuthorityProposed)
	}
}

// TestCommitReviewRefusesAnUnregisteredReviewer: a reviewer id that resolves
// to no actor at all is refused for the same reason an agent reviewer is —
// the accountability the review record claims ("this human decided") has to
// be checkable against the registry, and an id nobody registered is not.
func TestCommitReviewRefusesAnUnregisteredReviewer(t *testing.T) {
	ctx := context.Background()
	l, store := newTestLedger(t)

	claim := mustAppend(t, l, claimRecord(t, "unreviewable"))
	version := mustVersion(t, l, testRunID)

	req, err := l.CreateReviewRequest(ctx, testRunID, []string{claim.ID}, version,
		ledger.WithReviewer("actor_nobody_registered"))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}

	before := store.count()
	_, err = l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{claim.ID: ledger.VerdictConfirm}, version,
		ledger.WithRationale("read the transcript"))
	if !errors.Is(err, ledger.ErrActorNotFound) {
		t.Fatalf("CommitReview error = %v, want ErrActorNotFound", err)
	}
	if got := store.count(); got != before {
		t.Fatalf("%d record(s) appended by a refused review, want 0", got-before)
	}
}

// TestCommitReviewRecordsTheStatedRationale: the decision record has to say
// WHY, not only who and what. A confirmation with no stated reason cannot be
// told apart from an unread one — which is the failure mode
// scripts/decide-claims.py already refused locally, with nowhere to put the
// answer until now.
func TestCommitReviewRecordsTheStatedRationale(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLedger(t)

	claim := mustAppend(t, l, claimRecord(t, "the collector received a trace"))
	version := mustVersion(t, l, testRunID)

	req, err := l.CreateReviewRequest(ctx, testRunID, []string{claim.ID}, version, ledger.WithReviewer(testHuman))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}

	const why = "read the trace in the collector; all three seam spans share one trace id"
	result, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{claim.ID: ledger.VerdictConfirm}, version,
		ledger.WithRationale(why))
	if err != nil {
		t.Fatalf("CommitReview: %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("committed %d review records, want 1", len(result.Records))
	}

	var payload struct {
		Verdict   string `json:"verdict"`
		Rationale string `json:"rationale"`
	}
	if err := json.Unmarshal(result.Records[0].Data, &payload); err != nil {
		t.Fatalf("decode review payload: %v", err)
	}
	if payload.Rationale != why {
		t.Fatalf("rationale = %q, want %q", payload.Rationale, why)
	}
	if payload.Verdict != string(ledger.VerdictConfirm) {
		t.Fatalf("verdict = %q, want confirm", payload.Verdict)
	}
}

// TestCommitReviewOmitsAnAbsentRationale: no rationale means no `rationale`
// key, never an empty string. An empty rationale would read as "a reason was
// given and it was blank"; absence reads as what it is. The HTTP decision
// surface requires one (see internal/api/reviews.go); the ledger records what
// it was given.
func TestCommitReviewOmitsAnAbsentRationale(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLedger(t)

	claim := mustAppend(t, l, claimRecord(t, "no reason offered"))
	version := mustVersion(t, l, testRunID)

	req, err := l.CreateReviewRequest(ctx, testRunID, []string{claim.ID}, version, ledger.WithReviewer(testHuman))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}
	result, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{claim.ID: ledger.VerdictConfirm}, version)
	if err != nil {
		t.Fatalf("CommitReview: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(result.Records[0].Data, &payload); err != nil {
		t.Fatalf("decode review payload: %v", err)
	}
	if _, present := payload["rationale"]; present {
		t.Fatalf("review payload carries a rationale key with no rationale given: %v", payload)
	}
}

// TestCommitReviewAllowsAHumanToDecideItsOwnProposal pins the one shape that
// looks like self-promotion and is not: PRD §9.11's human-task decision, where
// the decider appends its own `decision` record as proposed and confirms it in
// the same transaction (internal/engine/humandecision.go). The rule that keeps
// agents out must not also lock humans out of their own authority — so this
// test exists to fail if the reviewer check is ever widened to "no actor may
// decide a record it produced".
func TestCommitReviewAllowsAHumanToDecideItsOwnProposal(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLedger(t)

	own, err := l.Append(ctx, ledger.Record{
		RecordType: ledger.RecordDecision,
		RunID:      testRunID,
		Origin:     humanOrigin,
		Authority:  ledger.AuthorityProposed,
		Data:       mustJSON(t, map[string]any{"outcome": "approve", "human_task_id": "ht_01"}),
	})
	if err != nil {
		t.Fatalf("append the human's own decision record: %v", err)
	}

	version := mustVersion(t, l, testRunID)
	req, err := l.CreateReviewRequest(ctx, testRunID, []string{own.ID}, version, ledger.WithReviewer(testHuman))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}
	if _, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{own.ID: ledger.VerdictConfirm}, version); err != nil {
		t.Fatalf("a human confirming its own proposed decision: %v", err)
	}
}
