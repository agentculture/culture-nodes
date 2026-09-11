package ledger_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/contracts"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// handTurnRecord is one hand_turn record (task t16, decision c25) written by
// origin at authority. Callers own picking a combination the unchanged
// producer/authority matrix admits; the tests below pin which ones it does.
func handTurnRecord(t *testing.T, origin ledger.Origin, authority ledger.Authority) ledger.Record {
	t.Helper()
	return ledger.Record{
		RecordType: ledger.RecordHandTurn,
		RunID:      testRunID,
		Origin:     origin,
		Authority:  authority,
		Data: mustJSON(t, map[string]any{
			"what":           "cherry-picked review-fix/t3 onto the PR branch",
			"stage":          "land",
			"work_item":      "SCRUM-9",
			"definition_ref": "ledger_00000000000000000000000042",
			"rule":           "commit_without_run",
		}),
	}
}

func handTurnDefinitionRecord(t *testing.T, origin ledger.Origin, authority ledger.Authority) ledger.Record {
	t.Helper()
	return ledger.Record{
		RecordType: ledger.RecordHandTurnDefinition,
		RunID:      testRunID,
		Origin:     origin,
		Authority:  authority,
		Data: mustJSON(t, map[string]any{
			"stages": []string{"dispatch", "land", "review", "cleanup"},
			"rules": []map[string]any{
				{"id": "commit_without_run", "stage": "land", "description": "a commit on the PR branch with no run attempt in the window"},
			},
		}),
	}
}

// TestHandTurnKindsAreRegisteredAdditively: both kinds are registered after
// the PRD §10.2 set, exactly like grade, and the contracts package agrees.
func TestHandTurnKindsAreRegisteredAdditively(t *testing.T) {
	for _, kind := range []ledger.RecordType{ledger.RecordHandTurn, ledger.RecordHandTurnDefinition} {
		if !kind.Valid() {
			t.Errorf("%s is not a registered record type", kind)
		}
		found := false
		for _, registered := range contracts.LedgerRecordTypes() {
			if registered == string(kind) {
				found = true
			}
		}
		if !found {
			t.Errorf("contracts.LedgerRecordTypes() does not list %s", kind)
		}
	}
	types := ledger.RecordTypes()
	if types[len(types)-2] != ledger.RecordHandTurn || types[len(types)-1] != ledger.RecordHandTurnDefinition {
		t.Fatalf("hand_turn kinds must be appended after the existing set, got tail %v", types[len(types)-3:])
	}
}

// TestHandTurnAgentProposes is the observer's path: an agent that watched
// the loop writes a hand_turn as origin=agent authority=proposed. It is a
// claim about what a person did, not evidence of it.
func TestHandTurnAgentProposes(t *testing.T) {
	l, store := newTestLedger(t)
	out, err := l.Append(context.Background(), handTurnRecord(t, agentOrigin, ledger.AuthorityProposed))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if out.Authority != ledger.AuthorityProposed {
		t.Fatalf("authority = %q, want proposed", out.Authority)
	}
	if err := testValidator(t).Validate(contracts.LedgerRecordSchema(string(ledger.RecordHandTurn)), out); err != nil {
		t.Fatalf("appended hand_turn fails its own schema: %v", err)
	}
	if store.count() != 1 {
		t.Fatalf("stored %d records, want 1", store.count())
	}
}

// TestHandTurnAgentCannotObserveOrDerive: the observer is an agent, and an
// agent has no standing beyond proposed, hand_turn included.
func TestHandTurnAgentCannotObserveOrDerive(t *testing.T) {
	l, _ := newTestLedger(t)
	for _, authority := range []ledger.Authority{ledger.AuthorityObserved, ledger.AuthorityDerived, ledger.AuthorityConfirmed} {
		_, err := l.Append(context.Background(), handTurnRecord(t, agentOrigin, authority))
		var authErr *ledger.AuthorityError
		if !errors.As(err, &authErr) || authErr.Rule != ledger.RuleAgentProposesOnly {
			t.Fatalf("agent %s hand_turn: err = %v, want rule %s", authority, err, ledger.RuleAgentProposesOnly)
		}
	}
}

// TestHandTurnHumanDirectEntryLandsProposed is `nodes hand-turn` typed by a
// person: the matrix is unchanged, so a human's ordinary append lands
// proposed and reaches confirmed only through a review transaction — there
// is deliberately no grade-style carve-out for hand_turn.
func TestHandTurnHumanDirectEntryLandsProposed(t *testing.T) {
	l, _ := newTestLedger(t)
	out, err := l.Append(context.Background(), handTurnRecord(t, humanOrigin, ledger.AuthorityProposed))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if out.Authority != ledger.AuthorityProposed {
		t.Fatalf("authority = %q, want proposed", out.Authority)
	}

	_, err = l.Append(context.Background(), handTurnRecord(t, humanOrigin, ledger.AuthorityConfirmed))
	var authErr *ledger.AuthorityError
	if !errors.As(err, &authErr) || authErr.Rule != ledger.RuleHumanReviewOnly {
		t.Fatalf("human confirmed hand_turn outside a review: err = %v, want rule %s", err, ledger.RuleHumanReviewOnly)
	}
}

// TestHandTurnHumanCannotWriteObserved is the acceptance criterion spelled
// out in the task: a human principal can NOT write a hand_turn as observed.
// Observed belongs to a measuring runner; a person asserting it would be
// claiming coverage nobody measured.
func TestHandTurnHumanCannotWriteObserved(t *testing.T) {
	l, store := newTestLedger(t)
	for _, rec := range []ledger.Record{
		handTurnRecord(t, humanOrigin, ledger.AuthorityObserved),
		handTurnDefinitionRecord(t, humanOrigin, ledger.AuthorityObserved),
	} {
		_, err := l.Append(context.Background(), rec)
		var authErr *ledger.AuthorityError
		if !errors.As(err, &authErr) {
			t.Fatalf("%s: err = %v (%T), want *ledger.AuthorityError", rec.RecordType, err, err)
		}
		if authErr.Rule != ledger.RuleHumanAuthorityLimited {
			t.Fatalf("%s: refused by rule %q, want %q", rec.RecordType, authErr.Rule, ledger.RuleHumanAuthorityLimited)
		}
		if checkErr := ledger.CheckAuthority(rec, nil); !errors.As(checkErr, &authErr) || authErr.Rule != ledger.RuleHumanAuthorityLimited {
			t.Fatalf("%s: CheckAuthority = %v, want the same refusal as Append", rec.RecordType, checkErr)
		}
	}
	if store.count() != 0 {
		t.Fatalf("stored %d records after refused observed hand-turns, want 0", store.count())
	}
}

// TestHandTurnRunnerRefused: a runner reports evidence only; a hand_turn is
// not evidence, whatever authority the runner asks for.
func TestHandTurnRunnerRefused(t *testing.T) {
	l, _ := newTestLedger(t)
	rec := handTurnRecord(t, runnerOrigin, ledger.AuthorityObserved)
	_, err := l.Append(context.Background(), rec, ledger.WithRunnerManifest(ledger.RunnerManifest{ActorID: testRunner, ObservableFields: []string{""}}))
	var authErr *ledger.AuthorityError
	if !errors.As(err, &authErr) || authErr.Rule != ledger.RuleRunnerEvidenceOnly {
		t.Fatalf("runner hand_turn: err = %v, want rule %s", err, ledger.RuleRunnerEvidenceOnly)
	}
}

// TestHandTurnConfirmedThroughReview is the human-guided loop's second half
// (decision c25): the agent's proposed hand_turn records — and the human's
// own proposed definition — are confirmed in one review batch, which appends
// confirmed review records naming them and leaves the targets proposed.
func TestHandTurnConfirmedThroughReview(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLedger(t)

	definition := mustAppend(t, l, handTurnDefinitionRecord(t, humanOrigin, ledger.AuthorityProposed))
	first := mustAppend(t, l, handTurnRecord(t, agentOrigin, ledger.AuthorityProposed))
	second := mustAppend(t, l, handTurnRecord(t, agentOrigin, ledger.AuthorityProposed))

	version := mustVersion(t, l, testRunID)
	req, err := l.CreateReviewRequest(ctx, testRunID, []string{definition.ID, first.ID, second.ID}, version, ledger.WithReviewer(testHuman))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}
	result, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{
		definition.ID: ledger.VerdictConfirm,
		first.ID:      ledger.VerdictConfirm,
		second.ID:     ledger.VerdictReject,
	}, version)
	if err != nil {
		t.Fatalf("CommitReview: %v", err)
	}
	got := map[string]ledger.Authority{}
	for _, rec := range result.Records {
		if rec.RecordType != ledger.RecordReview || rec.Origin.Kind != ledger.OriginHuman {
			t.Fatalf("review committed %s by %s, want human review records only", rec.RecordType, rec.Origin.Kind)
		}
		got[rec.SubjectRef.String()] = rec.Authority
	}
	if got[definition.ID] != ledger.AuthorityConfirmed || got[first.ID] != ledger.AuthorityConfirmed || got[second.ID] != ledger.AuthorityRejected {
		t.Fatalf("review verdicts = %v, want definition+first confirmed, second rejected", got)
	}
}
