package devague_test

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/devague"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestMapPlanWaves_Fixture walks the real fixture (testdata/waves.json —
// genuine `devague plan waves --json` output) and checks the two tasks
// mapped to the fields MapPlanWaves's doc comment promises: status "ready",
// depends_on derived from wave layering, and provenance_refs limited to the
// covered claim ids (not the honesty-condition ids, which have no
// standalone record).
func TestMapPlanWaves_Fixture(t *testing.T) {
	records, err := devague.MapPlanWaves(readTestdata(t, "waves.json"))
	if err != nil {
		t.Fatalf("MapPlanWaves: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 (t1, t2)", len(records))
	}

	byID := make(map[string]ledger.Record, len(records))
	for _, r := range records {
		byID[r.ID] = r
	}

	t1, ok := byID["dv_fixture_t1"]
	if !ok {
		t.Fatal("missing dv_fixture_t1")
	}
	t2, ok := byID["dv_fixture_t2"]
	if !ok {
		t.Fatal("missing dv_fixture_t2")
	}

	for _, rec := range []ledger.Record{t1, t2} {
		if rec.RecordType != ledger.RecordTask {
			t.Errorf("%s: record_type = %s, want task", rec.ID, rec.RecordType)
		}
		if rec.Origin.Kind != ledger.OriginAgent {
			t.Errorf("%s: origin.kind = %s, want agent", rec.ID, rec.Origin.Kind)
		}
		if rec.Authority != ledger.AuthorityProposed {
			t.Errorf("%s: authority = %s, want proposed (agents may only propose)", rec.ID, rec.Authority)
		}
		data, err := rec.DataMap()
		if err != nil {
			t.Fatalf("%s: DataMap: %v", rec.ID, err)
		}
		if status, _ := data["status"].(string); status != "ready" {
			t.Errorf("%s: data.status = %v, want ready", rec.ID, data["status"])
		}
	}

	t1Data, _ := t1.DataMap()
	if deps, _ := t1Data["depends_on"].([]any); len(deps) != 0 {
		t.Errorf("t1 depends_on = %v, want empty (wave 0)", deps)
	}
	t2Data, _ := t2.DataMap()
	deps, _ := t2Data["depends_on"].([]any)
	if len(deps) != 1 || deps[0] != "dv_fixture_t1" {
		t.Errorf("t2 depends_on = %v, want [dv_fixture_t1]", deps)
	}

	// t1 covers c1/h1/c2/h2/c3/h3: provenance_refs should carry only the
	// claim-shaped covers (c1, c2, c3), translated to ledger ids.
	wantProvenance := []string{"dv_fixture_c1", "dv_fixture_c2", "dv_fixture_c3"}
	if !equalStrings(t1.ProvenanceRefs, wantProvenance) {
		t.Errorf("t1 provenance_refs = %v, want %v", t1.ProvenanceRefs, wantProvenance)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMapPlanWaves_TaskMissingFromWavesIsAnHonestError probes a
// hand-written (not devague-fixture) payload where a task appears in
// `tasks` but not in any `waves` entry — an internally inconsistent
// document `devague plan waves --json` should never actually produce, but
// this package refuses to guess a wave for it rather than silently treating
// it as unblocked.
func TestMapPlanWaves_TaskMissingFromWavesIsAnHonestError(t *testing.T) {
	waves := []byte(`{
		"plan": "probe",
		"waves": [],
		"tasks": {"t1": {"summary": "x", "acceptance_criteria": [], "covers": []}}
	}`)
	if _, err := devague.MapPlanWaves(waves); err == nil {
		t.Fatal("MapPlanWaves accepted a task with no wave placement, want an error")
	}
}

func TestMapPlanWaves_MissingPlanSlugIsAnHonestError(t *testing.T) {
	if _, err := devague.MapPlanWaves([]byte(`{"waves": [], "tasks": {}}`)); err == nil {
		t.Fatal("MapPlanWaves accepted a payload with no plan slug, want an error")
	}
}

// TestMapPlanWaves_Deterministic mirrors
// TestMapFrameClaims_Deterministic for the plan side.
func TestMapPlanWaves_Deterministic(t *testing.T) {
	waves := readTestdata(t, "waves.json")

	first, err := devague.MapPlanWaves(waves)
	if err != nil {
		t.Fatalf("MapPlanWaves (first): %v", err)
	}
	second, err := devague.MapPlanWaves(waves)
	if err != nil {
		t.Fatalf("MapPlanWaves (second): %v", err)
	}
	assertRecordsIdentical(t, first, second)
}
