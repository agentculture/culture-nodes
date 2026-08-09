package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// TestLedgerRecordsRejectsUpdate and TestLedgerRecordsRejectsDelete prove
// ledger_records has no UPDATE/DELETE path in the database itself
// (migrations/0003_ledger.sql's ledger_records_no_update/_no_delete
// triggers), not merely by convention in application code -- ledger
// records are immutable; corrections append a new row with `supersedes`
// (docs/initial-design/culture-nodes-prd-spec.md §10.3).

func TestLedgerRecordsRejectsUpdate(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-ledger-update")
	recordID := insertTestLedgerRecord(t, s, ns.ID)

	_, err := s.Pool().Exec(ctx, `UPDATE ledger_records SET authority = 'confirmed' WHERE id = $1`, recordID)
	if err == nil {
		t.Fatal("UPDATE ledger_records succeeded, want the immutability trigger to reject it")
	}
	if !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("UPDATE error = %v, want it to mention the immutability guard", err)
	}
}

func TestLedgerRecordsRejectsDelete(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-ledger-delete")
	recordID := insertTestLedgerRecord(t, s, ns.ID)

	_, err := s.Pool().Exec(ctx, `DELETE FROM ledger_records WHERE id = $1`, recordID)
	if err == nil {
		t.Fatal("DELETE ledger_records succeeded, want the immutability trigger to reject it")
	}
	if !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("DELETE error = %v, want it to mention the immutability guard", err)
	}

	// The row must still be there: the trigger raised before the delete
	// could take effect, not after.
	var count int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM ledger_records WHERE id = $1`, recordID).Scan(&count); err != nil {
		t.Fatalf("count ledger_records: %v", err)
	}
	if count != 1 {
		t.Fatalf("ledger_records row count for %s = %d, want 1 (row must survive the rejected DELETE)", recordID, count)
	}
}

func insertTestLedgerRecord(t *testing.T, s *postgres.Store, namespaceID string) string {
	t.Helper()
	id := store.NewULID()
	_, err := s.Pool().Exec(context.Background(), `
		INSERT INTO ledger_records (
			id, namespace_id, schema_version, record_type, origin_kind, authority, data, content_digest
		) VALUES ($1, $2, 'nodes.culture.dev/ledger/v1alpha1', 'claim', 'agent', 'proposed', '{}'::jsonb, $3)
	`, id, namespaceID, "sha256:"+store.NewULID())
	if err != nil {
		t.Fatalf("insert fixture ledger_record: %v", err)
	}
	return id
}
