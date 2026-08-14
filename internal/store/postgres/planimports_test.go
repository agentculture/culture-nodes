package postgres_test

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// Durable plan/wave/task/deviation import state (migration 0024, task t22).
// These tests pin the properties the plan-import API route
// (internal/api/planimports.go) depends on:
//
//  1. a plan import's tasks round-trip with their REAL per-task status and
//     REAL dependency edges (never a wave approximation) — the store-level
//     half of the t22 round-trip acceptance, complementing
//     internal/devague's own package-level proof;
//  2. deviations round-trip carrying their origin;
//  3. re-importing the same slug is a new, independent snapshot row, never
//     an overwrite (migrations/0024_plan_imports.sql's "immutability by
//     convention" note);
//  4. GetPlanImport reports ErrNotFound for an unknown id, and every write
//     is namespace-scoped.

func sampleImportInput(namespaceID string) postgres.ImportPlanInput {
	wave0 := 0
	wave1 := 1
	return postgres.ImportPlanInput{
		NamespaceID:  namespaceID,
		Slug:         "t22fixture",
		Title:        "t22 fixture plan",
		SourceSlug:   "t22fixture",
		SourceStatus: "drafting",
		SourceDigest: "sha256:deadbeef",
		Tasks: []postgres.PlanImportTaskInput{
			{TaskRef: "t1", Summary: "No-dependency setup task", OriginKind: "agent", SourceStatus: "confirmed", DependsOn: nil, Wave: &wave0},
			{TaskRef: "t2", Summary: "Another independent setup task", OriginKind: "agent", SourceStatus: "proposed", DependsOn: nil, Wave: &wave0},
			// t3's real dep is ONLY t1 -- the discriminating case: t1 and t2
			// are both wave 0, so a wave-level approximation of depends_on
			// would wrongly include t2 too.
			{TaskRef: "t3", Summary: "Depends on t1 only, not on t2", OriginKind: "agent", SourceStatus: "confirmed", DependsOn: []string{"t1"}, Wave: &wave1},
			{TaskRef: "t5", Summary: "Scoped out during planning", OriginKind: "agent", SourceStatus: "rejected", DependsOn: nil, Wave: nil},
		},
		Deviations: []postgres.PlanImportDeviationInput{
			{DeviationRef: "d1", What: "Swapped t3's approach", TaskRef: "t3", Reason: "verified against the installed toolchain", OriginKind: "human", SourceStatus: "approved", Classification: "acceptable"},
			{DeviationRef: "d2", What: "Propose folding scope", TaskRef: "t1", Reason: "found while scoping", OriginKind: "agent", SourceStatus: "proposed", Classification: "needs-follow-up"},
		},
	}
}

func TestImportPlanRoundTripsRealDependencyEdgesAndPerTaskStatus(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-plan-imports-roundtrip")

	created, err := s.ImportPlan(ctx, sampleImportInput(ns.ID))
	if err != nil {
		t.Fatalf("ImportPlan: %v", err)
	}
	if created.ID == "" {
		t.Fatal("ImportPlan returned an empty id")
	}
	if len(created.Tasks) != 4 || len(created.Deviations) != 2 {
		t.Fatalf("ImportPlan returned %d tasks / %d deviations, want 4/2", len(created.Tasks), len(created.Deviations))
	}

	read, err := s.GetPlanImport(ctx, ns.ID, created.ID)
	if err != nil {
		t.Fatalf("GetPlanImport: %v", err)
	}
	if len(read.Tasks) != 4 {
		t.Fatalf("GetPlanImport returned %d tasks, want 4", len(read.Tasks))
	}

	byRef := make(map[string]postgres.PlanImportTask, len(read.Tasks))
	for _, task := range read.Tasks {
		byRef[task.TaskRef] = task
	}

	t3, ok := byRef["t3"]
	if !ok {
		t.Fatal("missing t3")
	}
	// The acceptance test: t3's real depends_on is exactly [t1], never
	// widened to include t2 (t1's wave-mate) the way a lossy wave-level
	// reading would.
	if len(t3.DependsOn) != 1 || t3.DependsOn[0] != "t1" {
		t.Fatalf("t3.DependsOn = %v, want exactly [t1]", t3.DependsOn)
	}
	if t3.Wave == nil || *t3.Wave != 1 {
		t.Fatalf("t3.Wave = %v, want 1", t3.Wave)
	}

	// Per-task status differs across the fixture, not flattened to one
	// value.
	statuses := map[string]string{}
	for _, task := range read.Tasks {
		statuses[task.TaskRef] = task.SourceStatus
	}
	want := map[string]string{"t1": "confirmed", "t2": "proposed", "t3": "confirmed", "t5": "rejected"}
	for ref, wantStatus := range want {
		if statuses[ref] != wantStatus {
			t.Errorf("task %s: SourceStatus = %q, want %q", ref, statuses[ref], wantStatus)
		}
	}

	// t5 is rejected: it occupies no wave.
	t5, ok := byRef["t5"]
	if !ok {
		t.Fatal("missing t5")
	}
	if t5.Wave != nil {
		t.Errorf("t5.Wave = %v, want nil (a rejected task occupies no wave)", *t5.Wave)
	}
}

func TestImportPlanDeviationsCarryTheirOrigin(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-plan-imports-deviation-origin")

	created, err := s.ImportPlan(ctx, sampleImportInput(ns.ID))
	if err != nil {
		t.Fatalf("ImportPlan: %v", err)
	}

	read, err := s.GetPlanImport(ctx, ns.ID, created.ID)
	if err != nil {
		t.Fatalf("GetPlanImport: %v", err)
	}
	if len(read.Deviations) != 2 {
		t.Fatalf("got %d deviations, want 2", len(read.Deviations))
	}

	byRef := make(map[string]postgres.PlanImportDeviation, len(read.Deviations))
	for _, d := range read.Deviations {
		byRef[d.DeviationRef] = d
	}

	d1, ok := byRef["d1"]
	if !ok {
		t.Fatal("missing d1")
	}
	if d1.OriginKind != "human" || d1.SourceStatus != "approved" {
		t.Errorf("d1 = %+v, want origin_kind human, source_status approved", d1)
	}
	d2, ok := byRef["d2"]
	if !ok {
		t.Fatal("missing d2")
	}
	if d2.OriginKind != "agent" || d2.SourceStatus != "proposed" {
		t.Errorf("d2 = %+v, want origin_kind agent, source_status proposed", d2)
	}
	if d1.OriginKind == d2.OriginKind {
		t.Fatal("d1 and d2 have the same origin_kind, want the fixture to exercise both human and agent")
	}
}

// A re-import of the same plan slug is a new, independent snapshot -- never
// an overwrite of the previous one (migrations/0024_plan_imports.sql's
// immutability-by-convention note).
func TestImportPlanReimportIsANewIndependentSnapshot(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-plan-imports-reimport")

	first, err := s.ImportPlan(ctx, sampleImportInput(ns.ID))
	if err != nil {
		t.Fatalf("ImportPlan (first): %v", err)
	}
	second, err := s.ImportPlan(ctx, sampleImportInput(ns.ID))
	if err != nil {
		t.Fatalf("ImportPlan (second): %v", err)
	}
	if first.ID == second.ID {
		t.Fatal("two imports of the same slug produced the same plan_imports id, want two independent rows")
	}

	all, err := s.ListPlanImports(ctx, ns.ID, "t22fixture")
	if err != nil {
		t.Fatalf("ListPlanImports: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListPlanImports returned %d rows, want 2", len(all))
	}
	// Most-recent-first.
	if all[0].ID != second.ID || all[1].ID != first.ID {
		t.Fatalf("ListPlanImports order = [%s, %s], want [second, first]", all[0].ID, all[1].ID)
	}

	// The first snapshot's tasks are unaffected by the second import.
	firstRead, err := s.GetPlanImport(ctx, ns.ID, first.ID)
	if err != nil {
		t.Fatalf("GetPlanImport(first): %v", err)
	}
	if len(firstRead.Tasks) != 4 {
		t.Fatalf("first snapshot has %d tasks, want 4 (untouched by the second import)", len(firstRead.Tasks))
	}
}

func TestGetPlanImportUnknownIDIsNotFound(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-plan-imports-not-found")

	if _, err := s.GetPlanImport(ctx, ns.ID, "does-not-exist"); err == nil {
		t.Fatal("GetPlanImport on an unknown id returned no error, want postgres.ErrNotFound")
	}
}

func TestImportPlanIsNamespaceScoped(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns1 := mustNamespace(t, s, "test-plan-imports-ns1")
	ns2 := mustNamespace(t, s, "test-plan-imports-ns2")

	created, err := s.ImportPlan(ctx, sampleImportInput(ns1.ID))
	if err != nil {
		t.Fatalf("ImportPlan: %v", err)
	}

	if _, err := s.GetPlanImport(ctx, ns2.ID, created.ID); err == nil {
		t.Fatal("GetPlanImport found a namespace-1 import from namespace 2, want ErrNotFound")
	}

	otherNSList, err := s.ListPlanImports(ctx, ns2.ID, "t22fixture")
	if err != nil {
		t.Fatalf("ListPlanImports: %v", err)
	}
	if len(otherNSList) != 0 {
		t.Fatalf("ListPlanImports in namespace 2 returned %d rows, want 0", len(otherNSList))
	}
}

// EngineStore.ImportPlan/GetPlanImport are the API-layer entry points
// (internal/api/planimports.go uses s.engineStore, an *EngineStore) --
// proven separately from the *Store-level tests above since they are a
// distinct code path (own transaction over es.pool, namespace bound at
// construction rather than passed per call).
func TestEngineStorePlanImportRoundTrips(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "test-plan-imports-enginestore")

	es, err := postgres.NewEngineStore(s, ns.ID)
	if err != nil {
		t.Fatalf("NewEngineStore: %v", err)
	}

	in := sampleImportInput("this-gets-overwritten-by-the-bound-namespace")
	created, err := es.ImportPlan(ctx, in)
	if err != nil {
		t.Fatalf("EngineStore.ImportPlan: %v", err)
	}
	if created.NamespaceID != ns.ID {
		t.Fatalf("created.NamespaceID = %q, want the bound namespace %q (the caller's own NamespaceID field must not leak through)", created.NamespaceID, ns.ID)
	}

	read, err := es.GetPlanImport(ctx, created.ID)
	if err != nil {
		t.Fatalf("EngineStore.GetPlanImport: %v", err)
	}
	if len(read.Tasks) != 4 || len(read.Deviations) != 2 {
		t.Fatalf("EngineStore.GetPlanImport returned %d tasks / %d deviations, want 4/2", len(read.Tasks), len(read.Deviations))
	}

	list, err := es.ListPlanImports(ctx, "t22fixture")
	if err != nil {
		t.Fatalf("EngineStore.ListPlanImports: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("EngineStore.ListPlanImports returned %d rows, want 1", len(list))
	}
}
