package devague_test

import (
	"encoding/json"
	"testing"

	"github.com/agentculture/culture-nodes/internal/devague"
	"github.com/agentculture/culture-nodes/internal/ledger"
)

// TestParsePlanShow_Fixture walks testdata/plan-show.json (genuine `devague
// plan show --json` output over a five-task plan deliberately shaped so a
// wave-level dependency reading would be wrong for t3 — see
// testdata/README.md's "Regeneration: plan-show.json and deviations.json").
func TestParsePlanShow_Fixture(t *testing.T) {
	plan, err := devague.ParsePlanShow(readTestdata(t, "plan-show.json"))
	if err != nil {
		t.Fatalf("ParsePlanShow: %v", err)
	}
	if plan.Slug != "t22fixture" || plan.FrameSlug != "t22fixture" {
		t.Fatalf("plan = %+v, want slug/frame_slug t22fixture", plan)
	}
	if len(plan.Tasks) != 5 {
		t.Fatalf("got %d tasks, want 5", len(plan.Tasks))
	}

	byID := make(map[string]devague.PlanTask, len(plan.Tasks))
	for _, task := range plan.Tasks {
		byID[task.ID] = task
	}

	intPtr := func(v int) *int { return &v }
	cases := []struct {
		id           string
		sourceStatus string
		deps         []string
		wave         *int
	}{
		{"t1", "confirmed", nil, intPtr(0)},
		{"t2", "proposed", nil, intPtr(0)},
		{"t3", "confirmed", []string{"t1"}, intPtr(1)},
		{"t4", "confirmed", []string{"t1", "t2"}, intPtr(1)},
		{"t5", "rejected", nil, nil}, // rejected: no wave
	}
	for _, tc := range cases {
		task, ok := byID[tc.id]
		if !ok {
			t.Fatalf("missing task %s", tc.id)
		}
		if task.SourceStatus != tc.sourceStatus {
			t.Errorf("%s: SourceStatus = %q, want %q", tc.id, task.SourceStatus, tc.sourceStatus)
		}
		if len(task.DependsOn) != len(tc.deps) {
			t.Errorf("%s: DependsOn = %v, want %v", tc.id, task.DependsOn, tc.deps)
		}
		for i, dep := range tc.deps {
			if i >= len(task.DependsOn) || task.DependsOn[i] != dep {
				t.Errorf("%s: DependsOn = %v, want %v", tc.id, task.DependsOn, tc.deps)
			}
		}
		switch {
		case tc.wave == nil && task.Wave != nil:
			t.Errorf("%s: Wave = %d, want nil (rejected task occupies no wave)", tc.id, *task.Wave)
		case tc.wave != nil && task.Wave == nil:
			t.Errorf("%s: Wave = nil, want %d", tc.id, *tc.wave)
		case tc.wave != nil && task.Wave != nil && *task.Wave != *tc.wave:
			t.Errorf("%s: Wave = %d, want %d", tc.id, *task.Wave, *tc.wave)
		}
	}
}

// TestMapPlanShow_Fixture checks every task landed at the record type,
// origin, and base authority this package's mapping rules promise — the
// same shape claims_test.go's TestMapFrameClaims_Fixture checks for claims.
func TestMapPlanShow_Fixture(t *testing.T) {
	records, err := devague.MapPlanShow(readTestdata(t, "plan-show.json"))
	if err != nil {
		t.Fatalf("MapPlanShow: %v", err)
	}

	byID := make(map[string]ledger.Record, len(records))
	for _, r := range records {
		byID[r.ID] = r
	}

	// 5 tasks; t1/t3/t4 confirmed and t5 rejected each get a review record
	// (4 total); t2 stays proposed and gets none.
	if len(records) != 9 {
		t.Fatalf("got %d records, want 9 (5 tasks + 4 reviews)", len(records))
	}

	for _, id := range []string{"t1", "t2", "t3", "t4", "t5"} {
		rec, ok := byID["dv_t22fixture_"+id]
		if !ok {
			t.Fatalf("missing task record dv_t22fixture_%s", id)
		}
		if rec.RecordType != ledger.RecordTask {
			t.Errorf("%s: record_type = %s, want task", id, rec.RecordType)
		}
		// Every base task record stays proposed, however devague itself
		// classified the task — the same authority-honesty rule
		// claims.go/authority_test.go establish for claims.
		if rec.Authority != ledger.AuthorityProposed {
			t.Errorf("%s: authority = %s, want proposed", id, rec.Authority)
		}
	}

	if _, ok := byID["dv_t22fixture_t2_review"]; ok {
		t.Error("dv_t22fixture_t2_review exists, want no review for a still-proposed task")
	}
	for id, wantAuthority := range map[string]ledger.Authority{
		"t1": ledger.AuthorityConfirmed,
		"t3": ledger.AuthorityConfirmed,
		"t4": ledger.AuthorityConfirmed,
		"t5": ledger.AuthorityRejected,
	} {
		review, ok := byID["dv_t22fixture_"+id+"_review"]
		if !ok {
			t.Fatalf("missing review record for %s", id)
		}
		if review.RecordType != ledger.RecordReview {
			t.Errorf("%s review: record_type = %s, want review", id, review.RecordType)
		}
		if review.Origin.Kind != ledger.OriginHuman {
			t.Errorf("%s review: origin.kind = %s, want human", id, review.Origin.Kind)
		}
		if review.Authority != wantAuthority {
			t.Errorf("%s review: authority = %s, want %s", id, review.Authority, wantAuthority)
		}
		if review.SubjectRef.String() != "dv_t22fixture_"+id {
			t.Errorf("%s review: subject_ref = %q, want dv_t22fixture_%s", id, review.SubjectRef, id)
		}
	}
}

// TestMapPlanShowDoesNotDegradeToTheWavesApproximation is the t22
// acceptance test: an imported plan's dependency edges must be the REAL
// per-task deps devague recorded, not "everything in the previous wave"
// (MapPlanWaves' documented, deliberate approximation — plan.go's doc
// comment). t3 is the fixture's discriminating case: t1 and t2 are both in
// wave 0, but t3's real `deps` is only `[t1]`. If this function silently
// fell back to (or reimplemented) the waves-view reading, t3's depends_on
// would incorrectly include t2 as well, since t2 is t1's wave-mate.
func TestMapPlanShowDoesNotDegradeToTheWavesApproximation(t *testing.T) {
	records, err := devague.MapPlanShow(readTestdata(t, "plan-show.json"))
	if err != nil {
		t.Fatalf("MapPlanShow: %v", err)
	}

	var t3 ledger.Record
	found := false
	for _, r := range records {
		if r.ID == "dv_t22fixture_t3" {
			t3 = r
			found = true
		}
	}
	if !found {
		t.Fatal("missing dv_t22fixture_t3")
	}

	var data struct {
		DependsOn []string `json:"depends_on"`
	}
	if err := json.Unmarshal(t3.Data, &data); err != nil {
		t.Fatalf("decode t3 data: %v", err)
	}

	want := []string{"dv_t22fixture_t1"}
	if len(data.DependsOn) != len(want) || data.DependsOn[0] != want[0] {
		t.Fatalf("t3 depends_on = %v, want exactly %v (not widened to include t2, t1's wave-mate)", data.DependsOn, want)
	}
}

// TestMapPlanShow_PerTaskStatusDiffers is the round-trip half of the same
// acceptance: three tasks with three different devague statuses must land
// at three different ledger-vocabulary statuses, proving the mapping reads
// per-task state rather than defaulting everything to one value the way
// MapPlanWaves — which cannot see per-task status at all — necessarily
// does (every MapPlanWaves task lands "ready"; see plan.go's doc comment).
func TestMapPlanShow_PerTaskStatusDiffers(t *testing.T) {
	records, err := devague.MapPlanShow(readTestdata(t, "plan-show.json"))
	if err != nil {
		t.Fatalf("MapPlanShow: %v", err)
	}
	byID := make(map[string]ledger.Record, len(records))
	for _, r := range records {
		byID[r.ID] = r
	}

	status := func(id string) string {
		var data struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(byID["dv_t22fixture_"+id].Data, &data); err != nil {
			t.Fatalf("decode %s data: %v", id, err)
		}
		return data.Status
	}

	cases := map[string]string{
		"t1": "ready",     // devague confirmed
		"t2": "proposed",  // devague still-proposed
		"t5": "cancelled", // devague rejected
	}
	seen := map[string]bool{}
	for id, want := range cases {
		got := status(id)
		seen[got] = true
		if got != want {
			t.Errorf("%s: ledger status = %q, want %q", id, got, want)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("only %d distinct ledger statuses across t1/t2/t5, want 3 (not flattened to one value)", len(seen))
	}
}

// TestMapPlanShow_MalformedPlansAreRefused proves every structural problem
// this package can detect is refused with an error naming what is wrong —
// never a panic, never a partial import.
func TestMapPlanShow_MalformedPlansAreRefused(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{
			"missing_slug",
			`{"tasks": []}`,
		},
		{
			"task_missing_id",
			`{"slug": "p", "tasks": [{"summary": "x", "origin": "user", "status": "confirmed"}]}`,
		},
		{
			"duplicate_task_id",
			`{"slug": "p", "tasks": [
				{"id": "t1", "summary": "a", "origin": "user", "status": "confirmed"},
				{"id": "t1", "summary": "b", "origin": "user", "status": "confirmed"}
			]}`,
		},
		{
			"dependency_on_unknown_task",
			`{"slug": "p", "tasks": [
				{"id": "t1", "summary": "a", "origin": "user", "status": "confirmed", "deps": ["t99"]}
			]}`,
		},
		{
			"dependency_cycle",
			`{"slug": "p", "tasks": [
				{"id": "t1", "summary": "a", "origin": "user", "status": "confirmed", "deps": ["t2"]},
				{"id": "t2", "summary": "b", "origin": "user", "status": "confirmed", "deps": ["t1"]}
			]}`,
		},
		{
			"unknown_origin",
			`{"slug": "p", "tasks": [{"id": "t1", "summary": "a", "origin": "oracle", "status": "confirmed"}]}`,
		},
		{
			"unknown_status",
			`{"slug": "p", "tasks": [{"id": "t1", "summary": "a", "origin": "user", "status": "pending"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := devague.ParsePlanShow([]byte(tc.json)); err == nil {
				t.Fatal("ParsePlanShow accepted a malformed plan, want an error")
			}
			if _, err := devague.MapPlanShow([]byte(tc.json)); err == nil {
				t.Fatal("MapPlanShow accepted a malformed plan, want an error")
			}
		})
	}
}

// TestMapPlanShow_Deterministic mirrors
// TestMapFrameClaims_Deterministic for the plan-show mapping.
func TestMapPlanShow_Deterministic(t *testing.T) {
	show := readTestdata(t, "plan-show.json")

	first, err := devague.MapPlanShow(show)
	if err != nil {
		t.Fatalf("MapPlanShow (first): %v", err)
	}
	second, err := devague.MapPlanShow(show)
	if err != nil {
		t.Fatalf("MapPlanShow (second): %v", err)
	}
	assertRecordsIdentical(t, first, second)
}
