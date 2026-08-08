package devague_test

import (
	"testing"

	"github.com/agentculture/culture-nodes/internal/devague"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestMapDeliverables_Fixture walks the real fixture (testdata/deliverables.json
// — genuine `devague plan deliverables --json` output) and checks the one
// success signal it carries maps to a derived/engine success_signal record —
// never a proposed/human or proposed/agent one, because nothing in this
// source represents an individual actor's proposal.
func TestMapDeliverables_Fixture(t *testing.T) {
	records, err := devague.MapDeliverables(readTestdata(t, "deliverables.json"))
	if err != nil {
		t.Fatalf("MapDeliverables: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}

	rec := records[0]
	if rec.ID != "dv_fixture_signal_1" {
		t.Errorf("id = %q, want dv_fixture_signal_1", rec.ID)
	}
	if rec.RecordType != ledger.RecordSuccessSignal {
		t.Errorf("record_type = %s, want success_signal", rec.RecordType)
	}
	if rec.Origin.Kind != ledger.OriginEngine {
		t.Errorf("origin.kind = %s, want engine", rec.Origin.Kind)
	}
	if rec.Authority != ledger.AuthorityDerived {
		t.Errorf("authority = %s, want derived", rec.Authority)
	}

	data, err := rec.DataMap()
	if err != nil {
		t.Fatalf("DataMap: %v", err)
	}
	if statement, _ := data["statement"].(string); statement != "Fixture converges in under 3 seconds" {
		t.Errorf("statement = %q, want the fixture's success_signal text", statement)
	}
	if mechanical, ok := data["mechanical"].(bool); !ok || mechanical {
		t.Errorf("mechanical = %v, want false", data["mechanical"])
	}

	// CheckAuthority must accept a derived/engine record unconditionally —
	// no review transaction required, unlike MapFrameClaims' confirmed
	// review records. See TestAuthorityHonestyMatchesLedgerRules for the
	// contrast.
	if err := ledger.CheckAuthority(rec, nil); err != nil {
		t.Errorf("CheckAuthority(%s) = %v, want acceptance", rec.ID, err)
	}
}

func TestMapDeliverables_EmptySuccessSignalsIsNotAnError(t *testing.T) {
	records, err := devague.MapDeliverables([]byte(`{"plan": "probe", "converged": false, "success_signal": []}`))
	if err != nil {
		t.Fatalf("MapDeliverables: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("got %d records, want 0", len(records))
	}
}

func TestMapDeliverables_MissingPlanSlugIsAnHonestError(t *testing.T) {
	if _, err := devague.MapDeliverables([]byte(`{"success_signal": []}`)); err == nil {
		t.Fatal("MapDeliverables accepted a payload with no plan slug, want an error")
	}
}

func TestMapDeliverables_Deterministic(t *testing.T) {
	deliverables := readTestdata(t, "deliverables.json")

	first, err := devague.MapDeliverables(deliverables)
	if err != nil {
		t.Fatalf("MapDeliverables (first): %v", err)
	}
	second, err := devague.MapDeliverables(deliverables)
	if err != nil {
		t.Fatalf("MapDeliverables (second): %v", err)
	}
	assertRecordsIdentical(t, first, second)
}
