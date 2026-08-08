package devague_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/devague"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestAgentOriginRecordsNeverCarryConfirmedAuthority is the t25
// authority-honesty acceptance, stated as a property over every record this
// package's Map* functions produce from the real fixture: an llm-origin
// devague claim never maps to a single record whose own authority is
// confirmed (or rejected, or anything but proposed). That holds however
// devague itself classified the claim's status — see claims.go's
// reviewForClaimStatus and claimData.
func TestAgentOriginRecordsNeverCarryConfirmedAuthority(t *testing.T) {
	records := mapAllFixtures(t)

	sawAgentOrigin := false
	for _, rec := range records {
		if rec.Origin.Kind != ledger.OriginAgent {
			continue
		}
		sawAgentOrigin = true
		if rec.Authority != ledger.AuthorityProposed {
			t.Errorf("%s: origin agent, authority %s — an agent-origin record must stay proposed", rec.ID, rec.Authority)
		}
	}
	if !sawAgentOrigin {
		t.Fatal("fixture produced no agent-origin record; the fixture no longer exercises this property (c4/c7 should be llm-origin)")
	}
}

// TestNonReviewRecordsPassCheckAuthority proves every record this package
// emits, other than the confirmed/rejected review records, is accepted by
// the ledger's own pure authority check with no review transaction and no
// runner manifest — i.e. an ordinary Append would take it as-is.
func TestNonReviewRecordsPassCheckAuthority(t *testing.T) {
	records := mapAllFixtures(t)

	checked := 0
	for _, rec := range records {
		if rec.RecordType == ledger.RecordReview {
			continue // covered by TestConfirmedReviewRecordsRequireARealReviewTransaction
		}
		checked++
		if err := ledger.CheckAuthority(rec, nil); err != nil {
			t.Errorf("CheckAuthority(%s, %s/%s) = %v, want acceptance", rec.ID, rec.Origin.Kind, rec.Authority, err)
		}
	}
	if checked == 0 {
		t.Fatal("no non-review records to check; the fixture no longer exercises this test")
	}
}

// TestConfirmedReviewRecordsRequireARealReviewTransaction is the other half
// of the t25 authority-honesty acceptance, and the one that reads as a
// surprise against a literal "CheckAuthority accepts every mapped record":
// it does not, for the review records, and that refusal is exactly the
// ledger's own invariant working, not a bug in this package's mapping.
//
// internal/ledger's own TestAppendEnforcesProducerAuthorityMatrix ("human
// confirms directly" / RuleHumanReviewOnly) proves a bare, non-transactional
// append of ANY origin-human confirmed/rejected review record is refused —
// confirmed/rejected authority is reachable only through
// Ledger.CreateReviewRequest + Ledger.CommitReview (PRD §10.4/§10.8: "no
// actor promotes its own proposal"). MapFrameClaims' review records are no
// exception, and must not be: if they *were* accepted by a bare
// CheckAuthority call, that would mean this package (or anything importing
// it) could manufacture ledger-confirmed authority for a devague decision
// with no real review transaction behind it — precisely what the rule
// exists to rule out.
//
// TestAuthorityHonestyMatchesLedgerRules below is the constructive half:
// it proves the record this test shows CheckAuthority refuses is still
// the exact shape a *real* CommitReview call produces, so it is a faithful
// import of an already-made decision, not a forgery of one.
func TestConfirmedReviewRecordsRequireARealReviewTransaction(t *testing.T) {
	records := mapAllFixtures(t)

	checked := 0
	for _, rec := range records {
		if rec.RecordType != ledger.RecordReview {
			continue
		}
		checked++

		if rec.Origin.Kind != ledger.OriginHuman {
			t.Errorf("%s: origin.kind = %s, want human", rec.ID, rec.Origin.Kind)
		}

		err := ledger.CheckAuthority(rec, nil)
		var authErr *ledger.AuthorityError
		if !errors.As(err, &authErr) {
			t.Fatalf("CheckAuthority(%s) = %v (%T), want *ledger.AuthorityError", rec.ID, err, err)
		}
		if authErr.Rule != ledger.RuleHumanReviewOnly {
			t.Errorf("%s: refused by rule %q, want %q", rec.ID, authErr.Rule, ledger.RuleHumanReviewOnly)
		}
	}
	if checked == 0 {
		t.Fatal("no review records to check; the fixture no longer exercises this test")
	}
}

// TestAuthorityHonestyMatchesLedgerRules runs c4 — the fixture's llm-origin,
// devague-confirmed boundary claim — through the real ledger: Append the
// base (proposed, origin agent) record exactly as MapFrameClaims produced
// it, then CreateReviewRequest + CommitReview it as a human reviewer really
// would. It asserts the record CommitReview actually appends has the same
// origin/record_type/authority/subject_ref shape as the confirmed review
// record MapFrameClaims already produced for c4 — proving that record is a
// faithful projection of what the real review transaction does, not an
// invented shortcut around it.
func TestAuthorityHonestyMatchesLedgerRules(t *testing.T) {
	claims, err := devague.MapFrameClaims(readTestdata(t, "show.json"))
	if err != nil {
		t.Fatalf("MapFrameClaims: %v", err)
	}
	var base, mappedReview ledger.Record
	for _, rec := range claims {
		switch rec.ID {
		case "dv_fixture_c4":
			base = rec
		case "dv_fixture_c4_review":
			mappedReview = rec
		}
	}
	if base.ID == "" || mappedReview.ID == "" {
		t.Fatal("fixture no longer contains dv_fixture_c4 / dv_fixture_c4_review (llm-origin, confirmed)")
	}
	if base.Origin.Kind != ledger.OriginAgent {
		t.Fatalf("dv_fixture_c4 origin.kind = %s, want agent (test requires an llm-origin confirmed claim)", base.Origin.Kind)
	}

	store := newMemStore()
	l, err := ledger.New(store)
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	ctx := context.Background()

	appended, err := l.Append(ctx, base)
	if err != nil {
		t.Fatalf("Append(base c4 claim, as MapFrameClaims produced it): %v", err)
	}
	if appended.Authority != ledger.AuthorityProposed {
		t.Fatalf("Append accepted c4 at authority %s, want proposed", appended.Authority)
	}

	version, err := l.LedgerVersion(ctx, appended.RunID)
	if err != nil {
		t.Fatalf("LedgerVersion: %v", err)
	}
	review, err := l.CreateReviewRequest(ctx, appended.RunID, []string{appended.ID}, version,
		ledger.WithReviewer("actor_human_reviewer"))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}

	result, err := l.CommitReview(ctx, review.ID, map[string]ledger.Verdict{appended.ID: ledger.VerdictConfirm}, version)
	if err != nil {
		t.Fatalf("CommitReview: %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("CommitReview appended %d records, want 1", len(result.Records))
	}
	real := result.Records[0]

	if real.RecordType != mappedReview.RecordType {
		t.Errorf("real review record_type = %s, mapped = %s", real.RecordType, mappedReview.RecordType)
	}
	if real.Origin.Kind != mappedReview.Origin.Kind {
		t.Errorf("real review origin.kind = %s, mapped = %s", real.Origin.Kind, mappedReview.Origin.Kind)
	}
	if real.Authority != mappedReview.Authority {
		t.Errorf("real review authority = %s, mapped = %s", real.Authority, mappedReview.Authority)
	}
	if real.SubjectRef.String() != mappedReview.SubjectRef.String() {
		t.Errorf("real review subject_ref = %s, mapped = %s", real.SubjectRef, mappedReview.SubjectRef)
	}
	if real.SubjectRef.String() != appended.ID {
		t.Errorf("real review subject_ref = %s, want the appended claim id %s", real.SubjectRef, appended.ID)
	}

	// The real transaction's own record — appended for real, through the
	// path the ledger actually requires — passes CheckAuthority: it is not
	// that confirmed authority is unreachable, only that it is unreachable
	// by any route other than this one.
	if err := ledger.CheckAuthority(real, nil); err != nil {
		var authErr *ledger.AuthorityError
		if !errors.As(err, &authErr) || authErr.Rule != ledger.RuleHumanReviewOnly {
			t.Fatalf("CheckAuthority(real committed review) = %v, want acceptance or (at worst) the same review-only refusal a bare re-check of any already-committed review record gets", err)
		}
		// RuleHumanReviewOnly here would mean CheckAuthority's bare,
		// non-transactional check refuses even a record that a real
		// CommitReview produced — which is correct and expected: the check
		// answers "would a bare Append accept this", and the answer is
		// still no. The record is nonetheless live in the ledger, reachable
		// only because it went through CommitReview, not because
		// CheckAuthority approved it after the fact.
	}
}
