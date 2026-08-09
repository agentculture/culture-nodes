package devague_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/agentculture/culture-nodes/internal/devague"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return data
}

// TestMapFrameClaims_Fixture walks the real fixture (testdata/show.json —
// genuine `devague show --json` output, see testdata/README.md for the exact
// move sequence) and checks every claim landed at the record type, origin,
// and authority this package's mapping rules promise.
func TestMapFrameClaims_Fixture(t *testing.T) {
	records, err := devague.MapFrameClaims(readTestdata(t, "show.json"))
	if err != nil {
		t.Fatalf("MapFrameClaims: %v", err)
	}

	byID := make(map[string]ledger.Record, len(records))
	for _, r := range records {
		byID[r.ID] = r
	}

	// 8 claims in the fixture; 7 are confirmed (one review record each) and
	// one (c7, an assumption) is left proposed with no review.
	if len(records) != 15 {
		t.Fatalf("got %d records, want 15 (8 claims + 7 reviews)", len(records))
	}

	cases := []struct {
		id         string
		recordType ledger.RecordType
		originKind ledger.OriginKind
	}{
		{"dv_fixture_c1", ledger.RecordAnnouncement, ledger.OriginHuman}, // announcement
		{"dv_fixture_c2", ledger.RecordClaim, ledger.OriginHuman},        // audience
		{"dv_fixture_c3", ledger.RecordClaim, ledger.OriginHuman},        // after_state
		{"dv_fixture_c4", ledger.RecordClaim, ledger.OriginAgent},        // boundary, llm-origin
		{"dv_fixture_c5", ledger.RecordClaim, ledger.OriginHuman},        // success_signal
		{"dv_fixture_c6", ledger.RecordDecision, ledger.OriginHuman},     // decision
		{"dv_fixture_c7", ledger.RecordAssumption, ledger.OriginAgent},   // assumption, llm-origin, left proposed
		{"dv_fixture_c8", ledger.RecordClaim, ledger.OriginHuman},        // why_it_matters
	}
	for _, tc := range cases {
		rec, ok := byID[tc.id]
		if !ok {
			t.Fatalf("missing record %s", tc.id)
		}
		if rec.RecordType != tc.recordType {
			t.Errorf("%s: record_type = %s, want %s", tc.id, rec.RecordType, tc.recordType)
		}
		if rec.Origin.Kind != tc.originKind {
			t.Errorf("%s: origin.kind = %s, want %s", tc.id, rec.Origin.Kind, tc.originKind)
		}
		// Every base claim record stays proposed — see the authority-honesty
		// test for why this must hold regardless of devague's own status.
		if rec.Authority != ledger.AuthorityProposed {
			t.Errorf("%s: authority = %s, want %s", tc.id, rec.Authority, ledger.AuthorityProposed)
		}
	}

	// c7 (still "proposed" in devague) gets no review record.
	if _, ok := byID["dv_fixture_c7_review"]; ok {
		t.Error("dv_fixture_c7_review exists, want no review record for a still-proposed claim")
	}

	// Every other claim was confirmed in the fixture and gets a matching
	// human-origin confirmed review record referencing it.
	for _, id := range []string{"c1", "c2", "c3", "c4", "c5", "c6", "c8"} {
		reviewID := "dv_fixture_" + id + "_review"
		review, ok := byID[reviewID]
		if !ok {
			t.Fatalf("missing review record %s", reviewID)
		}
		if review.RecordType != ledger.RecordReview {
			t.Errorf("%s: record_type = %s, want review", reviewID, review.RecordType)
		}
		if review.Origin.Kind != ledger.OriginHuman {
			t.Errorf("%s: origin.kind = %s, want human (confirmation is always a human act)", reviewID, review.Origin.Kind)
		}
		if review.Authority != ledger.AuthorityConfirmed {
			t.Errorf("%s: authority = %s, want confirmed", reviewID, review.Authority)
		}
		if review.SubjectRef.String() != "dv_fixture_"+id {
			t.Errorf("%s: subject_ref = %q, want dv_fixture_%s", reviewID, review.SubjectRef, id)
		}
	}
}

// TestMapFrameClaims_UnknownKindIsAnHonestError proves an unrecognised
// devague claim kind is refused rather than silently misclassified into
// "claim". The JSON below is a small hand-written probe of MapFrameClaims'
// error path — not a devague fixture; testdata/*.json remain the only real
// devague output this package tests against.
func TestMapFrameClaims_UnknownKindIsAnHonestError(t *testing.T) {
	show := []byte(`{
		"slug": "probe",
		"claims": [{"id": "c1", "kind": "prophecy", "text": "x", "origin": "user", "status": "proposed"}]
	}`)
	if _, err := devague.MapFrameClaims(show); err == nil {
		t.Fatal("MapFrameClaims accepted an unrecognised claim kind, want an error")
	}
}

func TestMapFrameClaims_UnknownOriginIsAnHonestError(t *testing.T) {
	show := []byte(`{
		"slug": "probe",
		"claims": [{"id": "c1", "kind": "decision", "text": "x", "origin": "oracle", "status": "proposed"}]
	}`)
	if _, err := devague.MapFrameClaims(show); err == nil {
		t.Fatal("MapFrameClaims accepted an unrecognised claim origin, want an error")
	}
}

func TestMapFrameClaims_UnknownStatusIsAnHonestError(t *testing.T) {
	show := []byte(`{
		"slug": "probe",
		"claims": [{"id": "c1", "kind": "decision", "text": "x", "origin": "user", "status": "pending"}]
	}`)
	if _, err := devague.MapFrameClaims(show); err == nil {
		t.Fatal("MapFrameClaims accepted an unrecognised claim status, want an error")
	}
}

func TestMapFrameClaims_MissingSlugIsAnHonestError(t *testing.T) {
	if _, err := devague.MapFrameClaims([]byte(`{"claims": []}`)); err == nil {
		t.Fatal("MapFrameClaims accepted a frame with no slug, want an error")
	}
}

// TestMapFrameClaims_RejectedClaimGetsARejectedReview proves the rejected
// side of the same rule the fixture only exercises for confirm: a rejected
// devague claim's base record still stays proposed, and the decision is a
// second, separate rejected review record. Also a hand-written probe, not a
// devague fixture, kept deliberately tiny (requirement kind, one claim).
func TestMapFrameClaims_RejectedClaimGetsARejectedReview(t *testing.T) {
	show := []byte(`{
		"slug": "probe",
		"claims": [{"id": "c1", "kind": "requirement", "text": "x", "origin": "llm", "status": "rejected"}]
	}`)
	records, err := devague.MapFrameClaims(show)
	if err != nil {
		t.Fatalf("MapFrameClaims: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 (base + rejected review)", len(records))
	}
	base, review := records[0], records[1]
	if base.ID != "dv_probe_c1" || base.RecordType != ledger.RecordClaim || base.Authority != ledger.AuthorityProposed {
		t.Errorf("base record = %+v, want dv_probe_c1/claim/proposed", base)
	}
	if review.ID != "dv_probe_c1_review" || review.Authority != ledger.AuthorityRejected || review.Origin.Kind != ledger.OriginHuman {
		t.Errorf("review record = %+v, want dv_probe_c1_review/rejected/human", review)
	}
}

// TestMapFrameClaims_Deterministic maps the fixture twice and checks every
// record's canonical JSON and content digest are byte-identical across the
// two runs — the core t25 promise: stable ids in, stable bytes out, with no
// randomness (ULIDs, wall-clock timestamps, or map-iteration order)
// anywhere in between.
func TestMapFrameClaims_Deterministic(t *testing.T) {
	show := readTestdata(t, "show.json")

	first, err := devague.MapFrameClaims(show)
	if err != nil {
		t.Fatalf("MapFrameClaims (first): %v", err)
	}
	second, err := devague.MapFrameClaims(show)
	if err != nil {
		t.Fatalf("MapFrameClaims (second): %v", err)
	}
	assertRecordsIdentical(t, first, second)
}

// assertRecordsIdentical asserts two record slices are byte-identical
// records, in the same order, with matching content digests — used by every
// Map* determinism test in this package.
func assertRecordsIdentical(t *testing.T, first, second []ledger.Record) {
	t.Helper()
	if len(first) != len(second) {
		t.Fatalf("got %d records the first run, %d the second", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("record %d: id %q != %q (ordering is not stable)", i, first[i].ID, second[i].ID)
		}
		firstJSON, err := json.Marshal(first[i])
		if err != nil {
			t.Fatalf("marshal first[%d]: %v", i, err)
		}
		secondJSON, err := json.Marshal(second[i])
		if err != nil {
			t.Fatalf("marshal second[%d]: %v", i, err)
		}
		if string(firstJSON) != string(secondJSON) {
			t.Fatalf("record %s: JSON differs between runs:\n first=%s\nsecond=%s", first[i].ID, firstJSON, secondJSON)
		}
		if first[i].ContentDigest == "" {
			t.Fatalf("record %s: content_digest is empty", first[i].ID)
		}
		if first[i].ContentDigest != second[i].ContentDigest {
			t.Fatalf("record %s: content_digest %q != %q", first[i].ID, first[i].ContentDigest, second[i].ContentDigest)
		}
		wantDigest, err := first[i].ComputeDigest()
		if err != nil {
			t.Fatalf("ComputeDigest(%s): %v", first[i].ID, err)
		}
		if first[i].ContentDigest != wantDigest {
			t.Fatalf("record %s: stamped digest %q does not match a fresh ComputeDigest %q", first[i].ID, first[i].ContentDigest, wantDigest)
		}
	}
}
