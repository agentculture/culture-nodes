package devague_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/devague"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// mapPlanShowAndDeviationsFixtures is roundtrip_test.go's mapAllFixtures for
// the t22 fixture pair (plan-show.json + deviations.json) instead of the
// original claims/waves/deliverables trio — the same "one run's worth of
// ledger records" shape, so every test below can reuse the authority_test.go
// property proofs unchanged.
func mapPlanShowAndDeviationsFixtures(t *testing.T) []ledger.Record {
	t.Helper()

	tasks, err := devague.MapPlanShow(readTestdata(t, "plan-show.json"))
	if err != nil {
		t.Fatalf("MapPlanShow: %v", err)
	}
	deviations, err := devague.MapDeviations(readTestdata(t, "deviations.json"))
	if err != nil {
		t.Fatalf("MapDeviations: %v", err)
	}
	all := make([]ledger.Record, 0, len(tasks)+len(deviations))
	all = append(all, tasks...)
	all = append(all, deviations...)
	return all
}

// TestPlanShowAndDeviations_AgentOriginRecordsNeverCarryConfirmedAuthority
// is authority_test.go's TestAgentOriginRecordsNeverCarryConfirmedAuthority,
// proven again over MapPlanShow/MapDeviations' own fixture: an llm-origin
// task or deviation's OWN record must stay proposed, however devague itself
// classified it (t1/t3/t4 confirmed and t5 rejected are all llm-origin
// tasks; d2/d3 are llm-origin deviations).
func TestPlanShowAndDeviations_AgentOriginRecordsNeverCarryConfirmedAuthority(t *testing.T) {
	records := mapPlanShowAndDeviationsFixtures(t)

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
		t.Fatal("fixture produced no agent-origin record; it no longer exercises this property")
	}
}

// TestPlanShowAndDeviations_NonReviewRecordsPassCheckAuthority mirrors
// authority_test.go's TestNonReviewRecordsPassCheckAuthority: every task and
// deviation record other than the confirmed/rejected review companions is
// accepted by a bare ledger.CheckAuthority call.
func TestPlanShowAndDeviations_NonReviewRecordsPassCheckAuthority(t *testing.T) {
	records := mapPlanShowAndDeviationsFixtures(t)

	checked := 0
	for _, rec := range records {
		if rec.RecordType == ledger.RecordReview {
			continue
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

// TestPlanShowAndDeviations_ConfirmedReviewsRequireARealReviewTransaction is
// the authority-preserving half of MapPlanShow's and MapDeviations' own
// authority doc comments: the review records they emit are provably NOT
// usable ledger-confirmed authority on their own — a bare CheckAuthority
// refuses them exactly the way it refuses MapFrameClaims' review records
// (authority_test.go's TestConfirmedReviewRecordsRequireARealReviewTransaction).
// This is the evidence behind the claim that emitting a review-shaped
// record does not "manufacture" authority an import did not earn.
func TestPlanShowAndDeviations_ConfirmedReviewsRequireARealReviewTransaction(t *testing.T) {
	records := mapPlanShowAndDeviationsFixtures(t)

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

// TestMapPlanShow_ReviewIsAFaithfulProjectionOfARealReviewTransaction is
// plan_show.go's twin of claims_test.go/authority_test.go's
// TestAuthorityHonestyMatchesLedgerRules: it runs t1 (llm-origin, devague-
// confirmed) through the REAL ledger — Append the base record exactly as
// MapPlanShow produced it, then CreateReviewRequest + CommitReview it as a
// human reviewer really would — and asserts the record CommitReview
// actually appends matches the shape MapPlanShow's own mapped review record
// already has.
func TestMapPlanShow_ReviewIsAFaithfulProjectionOfARealReviewTransaction(t *testing.T) {
	tasks, err := devague.MapPlanShow(readTestdata(t, "plan-show.json"))
	if err != nil {
		t.Fatalf("MapPlanShow: %v", err)
	}
	var base, mappedReview ledger.Record
	for _, rec := range tasks {
		switch rec.ID {
		case "dv_t22fixture_t1":
			base = rec
		case "dv_t22fixture_t1_review":
			mappedReview = rec
		}
	}
	if base.ID == "" || mappedReview.ID == "" {
		t.Fatal("fixture no longer contains dv_t22fixture_t1 / dv_t22fixture_t1_review")
	}
	if base.Origin.Kind != ledger.OriginAgent {
		t.Fatalf("dv_t22fixture_t1 origin.kind = %s, want agent (test requires an llm-origin confirmed task)", base.Origin.Kind)
	}

	store := newMemStore()
	l, err := ledger.New(store)
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	ctx := context.Background()

	appended, err := l.Append(ctx, base)
	if err != nil {
		t.Fatalf("Append(base t1 task, as MapPlanShow produced it): %v", err)
	}
	if appended.Authority != ledger.AuthorityProposed {
		t.Fatalf("Append accepted t1 at authority %s, want proposed", appended.Authority)
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
		t.Errorf("real review subject_ref = %s, want the appended task id %s", real.SubjectRef, appended.ID)
	}
}
