package devague_test

import (
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/devague"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestParseDeviations_Fixture walks testdata/deviations.json (the genuine
// on-disk `.devague/deliveries/t22fixture.json` — see testdata/README.md).
func TestParseDeviations_Fixture(t *testing.T) {
	deliveries, err := devague.ParseDeviations(readTestdata(t, "deviations.json"))
	if err != nil {
		t.Fatalf("ParseDeviations: %v", err)
	}
	if deliveries.PlanSlug != "t22fixture" {
		t.Fatalf("PlanSlug = %q, want t22fixture", deliveries.PlanSlug)
	}
	if len(deliveries.Deviations) != 3 {
		t.Fatalf("got %d deviations, want 3", len(deliveries.Deviations))
	}

	byID := make(map[string]devague.Deviation, len(deliveries.Deviations))
	for _, d := range deliveries.Deviations {
		byID[d.ID] = d
	}

	cases := []struct {
		id           string
		origin       ledger.OriginKind
		rawOrigin    string
		sourceStatus string
		taskRef      string
	}{
		{"d1", ledger.OriginHuman, "user", "approved", "t3"},
		{"d2", ledger.OriginAgent, "llm", "proposed", "t4"},
		{"d3", ledger.OriginAgent, "llm", "rejected", "t2"},
	}
	for _, tc := range cases {
		d, ok := byID[tc.id]
		if !ok {
			t.Fatalf("missing deviation %s", tc.id)
		}
		if d.Origin != tc.origin || d.RawOrigin != tc.rawOrigin {
			t.Errorf("%s: Origin/RawOrigin = %s/%s, want %s/%s", tc.id, d.Origin, d.RawOrigin, tc.origin, tc.rawOrigin)
		}
		if d.SourceStatus != tc.sourceStatus {
			t.Errorf("%s: SourceStatus = %q, want %q", tc.id, d.SourceStatus, tc.sourceStatus)
		}
		if d.TaskRef != tc.taskRef {
			t.Errorf("%s: TaskRef = %q, want %q", tc.id, d.TaskRef, tc.taskRef)
		}
	}
}

// TestMapDeviations_Fixture checks every deviation landed at the record
// type, origin, and base authority this package's mapping rules promise.
func TestMapDeviations_Fixture(t *testing.T) {
	records, err := devague.MapDeviations(readTestdata(t, "deviations.json"))
	if err != nil {
		t.Fatalf("MapDeviations: %v", err)
	}
	byID := make(map[string]ledger.Record, len(records))
	for _, r := range records {
		byID[r.ID] = r
	}

	// 3 deviations; d1 (approved) and d3 (rejected) each get a review
	// record; d2 (still proposed) gets none.
	if len(records) != 5 {
		t.Fatalf("got %d records, want 5 (3 deviations + 2 reviews)", len(records))
	}

	for _, id := range []string{"d1", "d2", "d3"} {
		rec, ok := byID["dv_t22fixture_"+id]
		if !ok {
			t.Fatalf("missing deviation record dv_t22fixture_%s", id)
		}
		if rec.RecordType != ledger.RecordDecision {
			t.Errorf("%s: record_type = %s, want decision", id, rec.RecordType)
		}
		if rec.Authority != ledger.AuthorityProposed {
			t.Errorf("%s: authority = %s, want proposed (an importer's own record is never itself confirmed)", id, rec.Authority)
		}
	}

	if _, ok := byID["dv_t22fixture_d2_review"]; ok {
		t.Error("dv_t22fixture_d2_review exists, want no review for a still-proposed deviation")
	}
	for id, wantAuthority := range map[string]ledger.Authority{
		"d1": ledger.AuthorityConfirmed,
		"d3": ledger.AuthorityRejected,
	} {
		review, ok := byID["dv_t22fixture_"+id+"_review"]
		if !ok {
			t.Fatalf("missing review record for %s", id)
		}
		if review.Origin.Kind != ledger.OriginHuman {
			t.Errorf("%s review: origin.kind = %s, want human", id, review.Origin.Kind)
		}
		if review.Authority != wantAuthority {
			t.Errorf("%s review: authority = %s, want %s", id, review.Authority, wantAuthority)
		}
	}
}

// TestMapDeviations_OriginSurvivesImport is the t22 acceptance test for
// deviations: "deviations import carrying their origin" — the issue #45
// "system knows" (llm) vs "user reports" (user) split must be visible on
// the imported record, both in the ledger envelope's origin.kind and in the
// raw devague word preserved in the payload.
func TestMapDeviations_OriginSurvivesImport(t *testing.T) {
	records, err := devague.MapDeviations(readTestdata(t, "deviations.json"))
	if err != nil {
		t.Fatalf("MapDeviations: %v", err)
	}
	byID := make(map[string]ledger.Record, len(records))
	for _, r := range records {
		byID[r.ID] = r
	}

	rawOrigin := func(id string) string {
		var data struct {
			Devague struct {
				Origin string `json:"origin"`
			} `json:"devague"`
		}
		if err := json.Unmarshal(byID["dv_t22fixture_"+id].Data, &data); err != nil {
			t.Fatalf("decode %s data: %v", id, err)
		}
		return data.Devague.Origin
	}

	cases := []struct {
		id            string
		envelopeKind  ledger.OriginKind
		rawOriginWord string
	}{
		{"d1", ledger.OriginHuman, "user"}, // user reports
		{"d2", ledger.OriginAgent, "llm"},  // system knows
		{"d3", ledger.OriginAgent, "llm"},  // system knows
	}
	distinctKinds := map[ledger.OriginKind]bool{}
	for _, tc := range cases {
		rec := byID["dv_t22fixture_"+tc.id]
		distinctKinds[rec.Origin.Kind] = true
		if rec.Origin.Kind != tc.envelopeKind {
			t.Errorf("%s: origin.kind = %s, want %s", tc.id, rec.Origin.Kind, tc.envelopeKind)
		}
		if got := rawOrigin(tc.id); got != tc.rawOriginWord {
			t.Errorf("%s: data.devague.origin = %q, want %q", tc.id, got, tc.rawOriginWord)
		}
	}
	if len(distinctKinds) != 2 {
		t.Fatalf("only %d distinct origin kinds across the fixture, want 2 (human and agent both exercised)", len(distinctKinds))
	}
}

// TestMapDeviations_TaskRefResolvesToARealProvenanceRef proves a
// deviation's task_ref is not just carried as text — it becomes a real,
// deterministic subject_ref/provenance_ref naming the ledger record
// MapPlanShow would produce for that same task (over the same plan slug).
func TestMapDeviations_TaskRefResolvesToARealProvenanceRef(t *testing.T) {
	records, err := devague.MapDeviations(readTestdata(t, "deviations.json"))
	if err != nil {
		t.Fatalf("MapDeviations: %v", err)
	}
	byID := make(map[string]ledger.Record, len(records))
	for _, r := range records {
		byID[r.ID] = r
	}

	d1 := byID["dv_t22fixture_d1"]
	if d1.SubjectRef.String() != "dv_t22fixture_t3" {
		t.Errorf("d1 subject_ref = %q, want dv_t22fixture_t3 (its task_ref)", d1.SubjectRef)
	}
	found := false
	for _, ref := range d1.ProvenanceRefs {
		if ref == "dv_t22fixture_t3" {
			found = true
		}
	}
	if !found {
		t.Errorf("d1 provenance_refs = %v, want dv_t22fixture_t3 present", d1.ProvenanceRefs)
	}

	// d2 affects both t4 (its own task_ref) and t1 (a task-shaped affects
	// entry) -- both should resolve, deduplicated.
	d2 := byID["dv_t22fixture_d2"]
	want := map[string]bool{"dv_t22fixture_t4": false, "dv_t22fixture_t1": false}
	for _, ref := range d2.ProvenanceRefs {
		if _, ok := want[ref]; ok {
			want[ref] = true
		}
	}
	for ref, seen := range want {
		if !seen {
			t.Errorf("d2 provenance_refs = %v, want %s present", d2.ProvenanceRefs, ref)
		}
	}
}

// TestMapDeviations_ProvenanceResolvesAgainstPlanShow proves that when
// MapDeviations and MapPlanShow are both run over the matching plan/delivery
// pair, no reference either emits dangles — the cross-function conformance
// property TestMappedRecordsHaveNoDanglingProvenance (roundtrip_test.go)
// checks for the original "fixture" plan, exercised here for the t22 pair.
func TestMapDeviations_ProvenanceResolvesAgainstPlanShow(t *testing.T) {
	tasks, err := devague.MapPlanShow(readTestdata(t, "plan-show.json"))
	if err != nil {
		t.Fatalf("MapPlanShow: %v", err)
	}
	deviations, err := devague.MapDeviations(readTestdata(t, "deviations.json"))
	if err != nil {
		t.Fatalf("MapDeviations: %v", err)
	}

	exists := make(map[string]bool, len(tasks)+len(deviations))
	for _, r := range tasks {
		exists[r.ID] = true
	}
	for _, r := range deviations {
		exists[r.ID] = true
	}
	for _, r := range deviations {
		for _, ref := range r.ProvenanceRefs {
			if !exists[ref] {
				t.Errorf("%s: provenance_refs names %q, which is not in the combined mapped set", r.ID, ref)
			}
		}
		if subject := r.SubjectRef.String(); subject != "" && !exists[subject] {
			t.Errorf("%s: subject_ref names %q, which is not in the combined mapped set", r.ID, subject)
		}
	}
}

// TestMapDeviations_NeverDerived is the guardrail half of MapDeviations'
// authority doc comment: an importer performing a deterministic TRANSLATION
// is not the same thing as a deterministic validator DERIVING a fact from
// confirmed/observed ledger inputs (PRD §10.4), and this function must never
// use ledger.AuthorityDerived to describe what it does.
func TestMapDeviations_NeverDerived(t *testing.T) {
	records, err := devague.MapDeviations(readTestdata(t, "deviations.json"))
	if err != nil {
		t.Fatalf("MapDeviations: %v", err)
	}
	for _, r := range records {
		if r.Authority == ledger.AuthorityDerived {
			t.Errorf("%s: authority = derived, want never derived from an import", r.ID)
		}
		if r.Origin.Kind == ledger.OriginEngine || r.Origin.Kind == ledger.OriginValidator {
			t.Errorf("%s: origin.kind = %s, want never engine/validator from an import", r.ID, r.Origin.Kind)
		}
	}
}

// TestMapDeviations_MalformedDeliveriesAreRefused mirrors
// TestMapPlanShow_MalformedPlansAreRefused for deliveries.
func TestMapDeviations_MalformedDeliveriesAreRefused(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"missing_plan_slug", `{"deviations": []}`},
		{
			"deviation_missing_id",
			`{"plan_slug": "p", "deviations": [{"what": "x", "task_ref": "t1", "reason": "r", "origin": "user", "status": "approved"}]}`,
		},
		{
			"deviation_missing_task_ref",
			`{"plan_slug": "p", "deviations": [{"id": "d1", "what": "x", "reason": "r", "origin": "user", "status": "approved"}]}`,
		},
		{
			"duplicate_deviation_id",
			`{"plan_slug": "p", "deviations": [
				{"id": "d1", "what": "a", "task_ref": "t1", "reason": "r", "origin": "user", "status": "approved"},
				{"id": "d1", "what": "b", "task_ref": "t1", "reason": "r", "origin": "user", "status": "approved"}
			]}`,
		},
		{
			"unknown_origin",
			`{"plan_slug": "p", "deviations": [{"id": "d1", "what": "x", "task_ref": "t1", "reason": "r", "origin": "oracle", "status": "approved"}]}`,
		},
		{
			"unknown_status",
			`{"plan_slug": "p", "deviations": [{"id": "d1", "what": "x", "task_ref": "t1", "reason": "r", "origin": "user", "status": "pending"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := devague.ParseDeviations([]byte(tc.json)); err == nil {
				t.Fatal("ParseDeviations accepted a malformed delivery, want an error")
			}
			if _, err := devague.MapDeviations([]byte(tc.json)); err == nil {
				t.Fatal("MapDeviations accepted a malformed delivery, want an error")
			}
		})
	}
}

// TestMapDeviations_Deterministic mirrors TestMapFrameClaims_Deterministic.
func TestMapDeviations_Deterministic(t *testing.T) {
	delivery := readTestdata(t, "deviations.json")

	first, err := devague.MapDeviations(delivery)
	if err != nil {
		t.Fatalf("MapDeviations (first): %v", err)
	}
	second, err := devague.MapDeviations(delivery)
	if err != nil {
		t.Fatalf("MapDeviations (second): %v", err)
	}
	assertRecordsIdentical(t, first, second)
}
