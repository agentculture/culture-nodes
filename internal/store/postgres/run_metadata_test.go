package postgres_test

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// TestMigration0013AddsNullableRunMetadataColumns proves migrations/0013
// shipped three nullable columns on `runs` -- name, description, category
// -- and nothing else: expand-only (docs/adr/0002-migration-policy.md). A
// binary built before 0013 existed still inserts runs with its own fixed
// column list (internal/store/postgres/engine_store.go's insertRunSQL never
// writes a bare `INSERT INTO runs ...`) and still selects with its own
// fixed column list (selectRunSQL), so three new nullable columns with no
// default change nothing it reads or writes -- the N-1 compatibility
// promise this migration makes.
func TestMigration0013AddsNullableRunMetadataColumns(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	for _, column := range []string{"name", "description", "category"} {
		var dataType, nullable, hasDefault string
		err := s.Pool().QueryRow(ctx,
			`SELECT data_type, is_nullable, COALESCE(column_default, '')
			 FROM information_schema.columns
			 WHERE table_name = 'runs' AND column_name = $1`,
			column,
		).Scan(&dataType, &nullable, &hasDefault)
		if err != nil {
			t.Fatalf("runs.%s: %v", column, err)
		}
		if dataType != "text" {
			t.Errorf("runs.%s data_type = %q, want text", column, dataType)
		}
		if nullable != "YES" {
			t.Errorf("runs.%s is_nullable = %q, want YES (expand-only: nullable)", column, nullable)
		}
		if hasDefault != "" {
			t.Errorf("runs.%s has a default (%q), want none", column, hasDefault)
		}
	}
}

// TestInsertRunWithoutMetadataLeavesColumnsNull proves a run inserted
// through engine_store.go's original fixed column list (no name/
// description/category, exactly what an N-1 binary or any pre-t3 caller
// still does) leaves all three columns NULL in the database -- not an
// empty-string placeholder.
func TestInsertRunWithoutMetadataLeavesColumnsNull(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := mustNamespace(t, s, "run-metadata-null")

	wv, err := s.CreateWorkflowVersion(ctx, postgres.CreateWorkflowVersionInput{
		NamespaceID:   ns.ID,
		WorkflowKey:   "run-metadata-null-fixture",
		Version:       1,
		SourceFormat:  "yaml",
		Source:        "entrypoint: intake\n",
		ContentDigest: "sha256:" + store.NewULID(),
	})
	if err != nil {
		t.Fatalf("CreateWorkflowVersion: %v", err)
	}

	runID := store.NewULID()
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO runs (id, namespace_id, workflow_version_id, status)
		VALUES ($1, $2, $3, 'running')
	`, runID, ns.ID, wv.ID); err != nil {
		t.Fatalf("insert fixture run: %v", err)
	}

	var name, description, category any
	if err := s.Pool().QueryRow(ctx,
		`SELECT name, description, category FROM runs WHERE id = $1`, runID,
	).Scan(&name, &description, &category); err != nil {
		t.Fatalf("read run row: %v", err)
	}
	if name != nil || description != nil || category != nil {
		t.Errorf("runs metadata = (%v, %v, %v), want all NULL", name, description, category)
	}
}
