package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// insertRunWithWorkItem writes one run row through the engine store's own
// InsertRun, the single writer of runs.work_item (migrations/0057), so the
// column's empty-is-NULL contract is proven against the real INSERT rather
// than a hand-written one.
func insertRunWithWorkItem(t *testing.T, ctx context.Context, es *postgres.EngineStore, namespaceID, versionID, workItem string) string {
	t.Helper()
	runID := store.NewULID()
	err := es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
		return tx.InsertRun(ctx, engine.Run{
			ID: runID, NamespaceID: namespaceID, WorkflowVersionID: versionID,
			State: engine.RunRunning, Input: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(),
			WorkItem: workItem,
		})
	})
	if err != nil {
		t.Fatalf("InsertRun(work_item=%q): %v", workItem, err)
	}
	return runID
}

// TestRunWorkItemRoundTripsAndEmptyIsNull: InsertRun persists Run.WorkItem
// into runs.work_item and Run reads it back; an empty WorkItem stores SQL
// NULL (the same "absent, not empty string" rule subject/reason follow),
// and category stays NULL either way — work_item is its own column
// (decision c41), never a category overload.
func TestRunWorkItemRoundTripsAndEmptyIsNull(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()
	_, _, _, versionID := seedRun(t, es, ns.ID, "work")

	keyed := insertRunWithWorkItem(t, ctx, es, ns.ID, versionID, "SCRUM-9")
	bare := insertRunWithWorkItem(t, ctx, es, ns.ID, versionID, "")

	run, err := es.Run(ctx, keyed)
	if err != nil {
		t.Fatalf("Run(keyed): %v", err)
	}
	if run.WorkItem != "SCRUM-9" {
		t.Errorf("Run.WorkItem = %q, want SCRUM-9", run.WorkItem)
	}
	run, err = es.Run(ctx, bare)
	if err != nil {
		t.Fatalf("Run(bare): %v", err)
	}
	if run.WorkItem != "" {
		t.Errorf("Run.WorkItem = %q, want empty", run.WorkItem)
	}

	s := requireStore(t)
	var keyedNull, bareNull, keyedCategoryNull bool
	if err := s.Pool().QueryRow(ctx,
		`SELECT work_item IS NULL, category IS NULL FROM runs WHERE id = $1`, keyed,
	).Scan(&keyedNull, &keyedCategoryNull); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool().QueryRow(ctx, `SELECT work_item IS NULL FROM runs WHERE id = $1`, bare).Scan(&bareNull); err != nil {
		t.Fatal(err)
	}
	if keyedNull {
		t.Error("keyed run stored NULL work_item")
	}
	if !keyedCategoryNull {
		t.Error("keyed run's category is not NULL: work_item leaked into category")
	}
	if !bareNull {
		t.Error("empty WorkItem stored a non-NULL value, want SQL NULL")
	}
}

// TestMigration0057AddsNullableWorkItemColumn pins the shape 0057 ships:
// a nullable TEXT column with no default, plus the partial index the
// work_item list filter reads.
func TestMigration0057AddsNullableWorkItemColumn(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	var dataType, isNullable string
	var columnDefault *string
	err := s.Pool().QueryRow(ctx, `
		SELECT data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_name = 'runs' AND column_name = 'work_item'`).Scan(&dataType, &isNullable, &columnDefault)
	if err != nil {
		t.Fatalf("runs.work_item column: %v", err)
	}
	if dataType != "text" || isNullable != "YES" || columnDefault != nil {
		t.Fatalf("runs.work_item = %s nullable=%s default=%v, want text / YES / NULL", dataType, isNullable, columnDefault)
	}

	var indexDef string
	if err := s.Pool().QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE tablename = 'runs' AND indexname = 'runs_namespace_work_item_idx'`,
	).Scan(&indexDef); err != nil {
		t.Fatalf("runs_namespace_work_item_idx: %v", err)
	}
	t.Logf("runs_namespace_work_item_idx: %s", indexDef)
}
