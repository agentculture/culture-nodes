package devague_test

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/devague"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// mapAllFixtures runs all three Map* functions over the real fixture files
// and returns the combined record set — one run's worth of ledger records
// as this package would hand them to a caller wiring devague into a
// culture-nodes run.
func mapAllFixtures(t *testing.T) []ledger.Record {
	t.Helper()

	claims, err := devague.MapFrameClaims(readTestdata(t, "show.json"))
	if err != nil {
		t.Fatalf("MapFrameClaims: %v", err)
	}
	tasks, err := devague.MapPlanWaves(readTestdata(t, "waves.json"))
	if err != nil {
		t.Fatalf("MapPlanWaves: %v", err)
	}
	signals, err := devague.MapDeliverables(readTestdata(t, "deliverables.json"))
	if err != nil {
		t.Fatalf("MapDeliverables: %v", err)
	}

	all := make([]ledger.Record, 0, len(claims)+len(tasks)+len(signals))
	all = append(all, claims...)
	all = append(all, tasks...)
	all = append(all, signals...)
	return all
}

// TestProjectionsRoundTripDeterministically is the t25 acceptance: mapping
// the same fixture bytes twice and feeding each mapping's combined records
// through internal/ledger's pure projection functions yields identical
// projection digests. Nothing here appends to a store — Live/ReadyTasks/
// ConfirmedClaims/DecisionHistory are pure functions over a []ledger.Record,
// exactly the shape Map* returns, so a devague frame+plan can be projected
// the same way any other run's ledger records are, with no ledger runtime
// involved.
func TestProjectionsRoundTripDeterministically(t *testing.T) {
	first := mapAllFixtures(t)
	second := mapAllFixtures(t)

	if len(first) != len(second) {
		t.Fatalf("got %d records the first mapping, %d the second", len(first), len(second))
	}

	projections := []struct {
		name string
		fn   func([]ledger.Record) (ledger.Projection, error)
	}{
		{"ReadyTasks", ledger.ReadyTasks},
		{"ConfirmedClaims", ledger.ConfirmedClaims},
		{"DecisionHistory", ledger.DecisionHistory},
		// Exercised for completeness beyond the three the task names:
		// CurrentScope and OpenAssumptionsAndQuestions also read record
		// types this package emits (announcement, assumption).
		{"CurrentScope", ledger.CurrentScope},
		{"OpenAssumptionsAndQuestions", ledger.OpenAssumptionsAndQuestions},
	}

	for _, p := range projections {
		t.Run(p.name, func(t *testing.T) {
			firstProjection, err := p.fn(first)
			if err != nil {
				t.Fatalf("%s(first): %v", p.name, err)
			}
			secondProjection, err := p.fn(second)
			if err != nil {
				t.Fatalf("%s(second): %v", p.name, err)
			}
			if firstProjection.Digest == "" {
				t.Fatalf("%s: digest is empty", p.name)
			}
			if firstProjection.Digest != secondProjection.Digest {
				t.Fatalf("%s: digest %q != %q across two mappings of the same fixture",
					p.name, firstProjection.Digest, secondProjection.Digest)
			}
			if err := firstProjection.VerifyDigest(); err != nil {
				t.Fatalf("%s: VerifyDigest: %v", p.name, err)
			}
		})
	}
}

// TestProjectionsSurfaceRealContent guards against the round-trip test above
// passing vacuously (two empty projections are trivially "identical"). Each
// projection named in the t25 acceptance must actually select something from
// this fixture.
func TestProjectionsSurfaceRealContent(t *testing.T) {
	records := mapAllFixtures(t)

	ready, err := ledger.ReadyTasks(records)
	if err != nil {
		t.Fatalf("ReadyTasks: %v", err)
	}
	if len(ready.Items) != 2 {
		t.Errorf("ReadyTasks: got %d items, want 2 (t1, t2 — both mapped to status ready)", len(ready.Items))
	}

	confirmed, err := ledger.ConfirmedClaims(records)
	if err != nil {
		t.Fatalf("ConfirmedClaims: %v", err)
	}
	// c2, c3, c4, c5, c8: the confirmed claim-kind records. c1 is an
	// announcement and c6 a decision — confirmed too, but not record_type
	// claim, so ConfirmedClaims does not select them.
	if len(confirmed.Items) != 5 {
		t.Errorf("ConfirmedClaims: got %d items, want 5", len(confirmed.Items))
	}

	decisions, err := ledger.DecisionHistory(records)
	if err != nil {
		t.Fatalf("DecisionHistory: %v", err)
	}
	if len(decisions.Items) != 1 {
		t.Errorf("DecisionHistory: got %d items, want 1 (c6)", len(decisions.Items))
	}
}

// TestMappedRecordsHaveNoDanglingProvenance checks every provenance_ref and
// subject_ref this package emits across all three Map* functions names a
// record id that actually exists in the combined set — a basic conformance
// property for a "conformance adapter": it does not invent references to
// records that are not there.
func TestMappedRecordsHaveNoDanglingProvenance(t *testing.T) {
	records := mapAllFixtures(t)
	exists := make(map[string]bool, len(records))
	for _, r := range records {
		exists[r.ID] = true
	}
	for _, r := range records {
		for _, ref := range r.ProvenanceRefs {
			if !exists[ref] {
				t.Errorf("%s: provenance_refs names %q, which is not in the mapped set", r.ID, ref)
			}
		}
		if subject := r.SubjectRef.String(); subject != "" && !exists[subject] {
			t.Errorf("%s: subject_ref names %q, which is not in the mapped set", r.ID, subject)
		}
	}
}
