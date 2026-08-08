package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestCommitReviewAppendsDecisionsWithoutTouchingTheTargets is the whole
// review model in one test: the human decision becomes a new record that
// points at the agent's proposal, and the proposal itself is still a
// proposal afterwards.
func TestCommitReviewAppendsDecisionsWithoutTouchingTheTargets(t *testing.T) {
	ctx := context.Background()
	l, store := newTestLedger(t)

	accepted := mustAppend(t, l, claimRecord(t, "the suite passed"))
	refused := mustAppend(t, l, claimRecord(t, "the docs are complete"))

	version := mustVersion(t, l, testRunID)
	req, err := l.CreateReviewRequest(ctx, testRunID, []string{accepted.ID, refused.ID}, version, ledger.WithReviewer(testHuman))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}
	if req.Status != ledger.ReviewRequested {
		t.Fatalf("new review status = %q, want %q", req.Status, ledger.ReviewRequested)
	}

	result, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{
		accepted.ID: ledger.VerdictConfirm,
		refused.ID:  ledger.VerdictReject,
	}, version)
	if err != nil {
		t.Fatalf("CommitReview: %v", err)
	}
	if len(result.Records) != 2 {
		t.Fatalf("committed %d review records, want 2", len(result.Records))
	}
	if result.LedgerVersion != version+2 {
		t.Fatalf("ledger version after commit = %d, want %d", result.LedgerVersion, version+2)
	}

	byTarget := map[string]ledger.Record{}
	for _, rec := range result.Records {
		if rec.RecordType != ledger.RecordReview {
			t.Fatalf("committed a %s record, want review records only", rec.RecordType)
		}
		if rec.Origin.Kind != ledger.OriginHuman || rec.Origin.ActorID != testHuman {
			t.Fatalf("review record origin = %+v, want the human reviewer", rec.Origin)
		}
		byTarget[rec.SubjectRef.String()] = rec
	}

	if got := byTarget[accepted.ID].Authority; got != ledger.AuthorityConfirmed {
		t.Fatalf("confirmation authority = %q, want %q", got, ledger.AuthorityConfirmed)
	}
	if got := byTarget[refused.ID].Authority; got != ledger.AuthorityRejected {
		t.Fatalf("rejection authority = %q, want %q", got, ledger.AuthorityRejected)
	}

	// The review record must say what it decided and against which frame.
	var payload struct {
		Verdict       string   `json:"verdict"`
		ReviewedRefs  []string `json:"reviewed_refs"`
		LedgerVersion string   `json:"ledger_version"`
		FrameChecksum string   `json:"frame_checksum"`
	}
	if err := json.Unmarshal(byTarget[accepted.ID].Data, &payload); err != nil {
		t.Fatalf("decode review payload: %v", err)
	}
	if payload.Verdict != string(ledger.VerdictConfirm) {
		t.Fatalf("verdict = %q, want %q", payload.Verdict, ledger.VerdictConfirm)
	}
	if payload.FrameChecksum != req.FrameChecksum {
		t.Fatalf("frame_checksum = %q, want the request's %q", payload.FrameChecksum, req.FrameChecksum)
	}

	// The reviewed claim is untouched: still proposed, same digest.
	after, err := l.Record(ctx, accepted.ID)
	if err != nil {
		t.Fatalf("re-read the reviewed claim: %v", err)
	}
	if after.Authority != ledger.AuthorityProposed || after.ContentDigest != accepted.ContentDigest {
		t.Fatalf("the reviewed claim changed: authority %q, digest %q", after.Authority, after.ContentDigest)
	}

	// And the projection now reads the confirmation off the review record.
	confirmed, err := ledger.ConfirmedClaims(store.all())
	if err != nil {
		t.Fatalf("ConfirmedClaims: %v", err)
	}
	if len(confirmed.Items) != 1 || confirmed.Items[0].ID != accepted.ID {
		t.Fatalf("confirmed claims = %v, want only %s", ids(confirmed.Items), accepted.ID)
	}
}

// TestCommitReviewRejectsAStaleLedgerAndAppliesNothing is PRD §10.8's core
// promise: a review written against a ledger that has moved is refused whole.
func TestCommitReviewRejectsAStaleLedgerAndAppliesNothing(t *testing.T) {
	ctx := context.Background()
	l, store := newTestLedger(t)

	target := mustAppend(t, l, claimRecord(t, "reviewed at version 1"))
	version := mustVersion(t, l, testRunID)

	req, err := l.CreateReviewRequest(ctx, testRunID, []string{target.ID}, version, ledger.WithReviewer(testHuman))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}

	// The world moves on between the request and the commit.
	mustAppend(t, l, claimRecord(t, "appended after the reviewer looked"))
	before := store.count()

	_, err = l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{target.ID: ledger.VerdictConfirm}, version)
	if !errors.Is(err, ledger.ErrStaleReview) {
		t.Fatalf("CommitReview error = %v, want ErrStaleReview", err)
	}

	var stale *ledger.StaleReviewError
	if !errors.As(err, &stale) {
		t.Fatalf("error %v does not carry a *StaleReviewError", err)
	}
	if stale.Reason != ledger.StaleLedgerMoved {
		t.Fatalf("stale reason = %q, want %q", stale.Reason, ledger.StaleLedgerMoved)
	}

	if store.count() != before {
		t.Fatalf("record count = %d, want %d — a stale review must apply nothing", store.count(), before)
	}
	reread, err := l.ReviewRequest(ctx, req.ID)
	if err != nil {
		t.Fatalf("re-read the review request: %v", err)
	}
	if reread.Status != ledger.ReviewRequested {
		t.Fatalf("review status = %q, want it left at %q", reread.Status, ledger.ReviewRequested)
	}
}

// TestCommitReviewRejectsAVersionTheRequestNeverRead guards the other end:
// the caller's expectation must be the one the reviewer actually reviewed at.
func TestCommitReviewRejectsAVersionTheRequestNeverRead(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLedger(t)

	target := mustAppend(t, l, claimRecord(t, "reviewed"))
	version := mustVersion(t, l, testRunID)
	req := mustReview(t, l, []string{target.ID}, version)

	_, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{target.ID: ledger.VerdictConfirm}, version+5)

	var stale *ledger.StaleReviewError
	if !errors.As(err, &stale) || stale.Reason != ledger.StaleRequestVersionMismatch {
		t.Fatalf("CommitReview error = %v, want reason %q", err, ledger.StaleRequestVersionMismatch)
	}
}

// TestCommitReviewRejectsASupersededTarget refuses to decide a record that
// has since been replaced: confirming superseded work is confirming work
// nobody is doing.
func TestCommitReviewRejectsASupersededTarget(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLedger(t)

	target := mustAppend(t, l, claimRecord(t, "original"))
	version := mustVersion(t, l, testRunID)
	req := mustReview(t, l, []string{target.ID}, version)

	if _, err := l.AppendSuperseding(ctx, claimRecord(t, "correction"), target.ID); err != nil {
		t.Fatalf("AppendSuperseding: %v", err)
	}

	_, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{target.ID: ledger.VerdictConfirm}, version)
	if !errors.Is(err, ledger.ErrStaleReview) {
		t.Fatalf("CommitReview error = %v, want ErrStaleReview", err)
	}
}

// TestCommitReviewRequiresAnExactDecisionSet — a review is a transaction over
// an agreed set of records, so deciding one nobody asked about, or leaving one
// out, is not a partial success.
func TestCommitReviewRequiresAnExactDecisionSet(t *testing.T) {
	ctx := context.Background()
	l, store := newTestLedger(t)

	first := mustAppend(t, l, claimRecord(t, "first"))
	second := mustAppend(t, l, claimRecord(t, "second"))
	version := mustVersion(t, l, testRunID)

	t.Run("undecided record", func(t *testing.T) {
		req := mustReview(t, l, []string{first.ID, second.ID}, version)
		before := store.count()

		_, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{first.ID: ledger.VerdictConfirm}, version)

		var target *ledger.ReviewTargetError
		if !errors.As(err, &target) {
			t.Fatalf("CommitReview error = %v, want *ReviewTargetError", err)
		}
		if len(target.Undecided) != 1 || target.Undecided[0] != second.ID {
			t.Fatalf("undecided = %v, want [%s]", target.Undecided, second.ID)
		}
		if store.count() != before {
			t.Fatalf("record count = %d, want %d", store.count(), before)
		}
	})

	t.Run("record not under review", func(t *testing.T) {
		req := mustReview(t, l, []string{first.ID}, version)

		_, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{
			first.ID:  ledger.VerdictConfirm,
			second.ID: ledger.VerdictConfirm,
		}, version)

		var target *ledger.ReviewTargetError
		if !errors.As(err, &target) {
			t.Fatalf("CommitReview error = %v, want *ReviewTargetError", err)
		}
		if len(target.Unknown) != 1 || target.Unknown[0] != second.ID {
			t.Fatalf("unknown = %v, want [%s]", target.Unknown, second.ID)
		}
	})

	t.Run("unrecognised verdict", func(t *testing.T) {
		req := mustReview(t, l, []string{first.ID}, version)

		_, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{first.ID: "maybe"}, version)
		if err == nil {
			t.Fatal("CommitReview accepted a verdict that is neither confirm nor reject")
		}
	})
}

func TestCommitReviewRefusesASecondCommit(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLedger(t)

	target := mustAppend(t, l, claimRecord(t, "reviewed once"))
	version := mustVersion(t, l, testRunID)
	req := mustReview(t, l, []string{target.ID}, version)

	if _, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{target.ID: ledger.VerdictConfirm}, version); err != nil {
		t.Fatalf("first CommitReview: %v", err)
	}

	_, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{target.ID: ledger.VerdictReject}, version)
	if !errors.Is(err, ledger.ErrReviewAlreadyCommitted) {
		t.Fatalf("second CommitReview error = %v, want ErrReviewAlreadyCommitted", err)
	}
}

// TestCommitReviewRequiresANamedReviewer — a confirmation nobody is
// accountable for is not a confirmation.
func TestCommitReviewRequiresANamedReviewer(t *testing.T) {
	ctx := context.Background()
	l, store := newTestLedger(t)

	target := mustAppend(t, l, claimRecord(t, "reviewed by nobody"))
	version := mustVersion(t, l, testRunID)

	req, err := l.CreateReviewRequest(ctx, testRunID, []string{target.ID}, version)
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}
	before := store.count()

	if _, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{target.ID: ledger.VerdictConfirm}, version); err == nil {
		t.Fatal("CommitReview accepted a review with no reviewer")
	}
	if store.count() != before {
		t.Fatalf("record count = %d, want %d", store.count(), before)
	}
}

// TestEmptyReviewSetCommitsCleanly — PRD §10.8: an empty review set is a real
// answer and returns a valid, empty result rather than an error.
func TestEmptyReviewSetCommitsCleanly(t *testing.T) {
	ctx := context.Background()
	l, store := newTestLedger(t)

	mustAppend(t, l, claimRecord(t, "not under review"))
	version := mustVersion(t, l, testRunID)
	req := mustReview(t, l, nil, version)

	result, err := l.CommitReview(ctx, req.ID, map[string]ledger.Verdict{}, version)
	if err != nil {
		t.Fatalf("CommitReview on an empty set: %v", err)
	}
	if result.Records == nil {
		t.Fatal("committed records slice is nil; an empty result must still serialise as []")
	}
	if len(result.Records) != 0 {
		t.Fatalf("committed %d records for an empty review, want 0", len(result.Records))
	}
	if store.count() != 1 {
		t.Fatalf("record count = %d, want it unchanged at 1", store.count())
	}

	raw, err := json.Marshal(result.Records)
	if err != nil {
		t.Fatalf("marshal empty review result: %v", err)
	}
	if string(raw) != "[]" {
		t.Fatalf("empty review result serialised as %s, want []", raw)
	}
}

// TestCreateReviewRequestRefusesAVersionThatIsNotCurrent stops a request
// being born stale.
func TestCreateReviewRequestRefusesAVersionThatIsNotCurrent(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLedger(t)

	target := mustAppend(t, l, claimRecord(t, "one"))
	version := mustVersion(t, l, testRunID)

	_, err := l.CreateReviewRequest(ctx, testRunID, []string{target.ID}, version+1, ledger.WithReviewer(testHuman))
	if !errors.Is(err, ledger.ErrStaleReview) {
		t.Fatalf("CreateReviewRequest error = %v, want ErrStaleReview", err)
	}
}

func TestCreateReviewRequestRefusesATargetFromAnotherRun(t *testing.T) {
	ctx := context.Background()
	l, _ := newTestLedger(t)

	elsewhere := claimRecord(t, "another run")
	elsewhere.RunID = "run_01TESTRUN0000000000000002"
	other := mustAppend(t, l, elsewhere)

	version := mustVersion(t, l, testRunID)
	if _, err := l.CreateReviewRequest(ctx, testRunID, []string{other.ID}, version, ledger.WithReviewer(testHuman)); err == nil {
		t.Fatal("CreateReviewRequest accepted a target from another run")
	}
}

// TestLedgerVersionCountsRecordsPerRun pins the definition the review guards
// depend on.
func TestLedgerVersionCountsRecordsPerRun(t *testing.T) {
	l, _ := newTestLedger(t)

	if v := mustVersion(t, l, testRunID); v != 0 {
		t.Fatalf("empty run version = %d, want 0", v)
	}
	mustAppend(t, l, claimRecord(t, "one"))
	mustAppend(t, l, claimRecord(t, "two"))
	if v := mustVersion(t, l, testRunID); v != 2 {
		t.Fatalf("version = %d, want 2", v)
	}

	elsewhere := claimRecord(t, "another run")
	elsewhere.RunID = "run_01TESTRUN0000000000000002"
	mustAppend(t, l, elsewhere)

	if v := mustVersion(t, l, testRunID); v != 2 {
		t.Fatalf("version = %d after appending to another run, want it unchanged at 2", v)
	}
}

func mustVersion(t *testing.T, l *ledger.Ledger, runID string) int64 {
	t.Helper()
	version, err := l.LedgerVersion(context.Background(), runID)
	if err != nil {
		t.Fatalf("LedgerVersion: %v", err)
	}
	return version
}

func mustReview(t *testing.T, l *ledger.Ledger, recordIDs []string, version int64) ledger.ReviewRequest {
	t.Helper()
	req, err := l.CreateReviewRequest(context.Background(), testRunID, recordIDs, version, ledger.WithReviewer(testHuman))
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}
	return req
}
