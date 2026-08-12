package postgres_test

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
)

// TestMigration0012AddsNullableAttemptUsageColumns proves migrations/0012
// shipped four nullable telemetry columns on `attempts` and nothing else --
// expand-only (docs/adr/0002-migration-policy.md). A binary built before
// 0012 existed still inserts and reads attempts with its own fixed column
// list (internal/store/postgres/engine_store.go never writes a bare
// `INSERT INTO attempts`), so four new nullable columns with no default
// change nothing it does -- the N-1 compatibility promise this migration
// makes.
func TestMigration0012AddsNullableAttemptUsageColumns(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	for _, tc := range []struct {
		column   string
		dataType string
	}{
		{"usage_input_tokens", "bigint"},
		{"usage_output_tokens", "bigint"},
		{"usage_cost", "double precision"},
		{"usage_currency", "text"},
	} {
		var dataType, nullable, hasDefault string
		err := s.Pool().QueryRow(ctx,
			`SELECT data_type, is_nullable, COALESCE(column_default, '')
			 FROM information_schema.columns
			 WHERE table_name = 'attempts' AND column_name = $1`,
			tc.column,
		).Scan(&dataType, &nullable, &hasDefault)
		if err != nil {
			t.Fatalf("attempts.%s: %v", tc.column, err)
		}
		if dataType != tc.dataType {
			t.Errorf("attempts.%s data_type = %q, want %q", tc.column, dataType, tc.dataType)
		}
		if nullable != "YES" {
			t.Errorf("attempts.%s is_nullable = %q, want YES (expand-only: nullable)", tc.column, nullable)
		}
		if hasDefault != "" {
			t.Errorf("attempts.%s has a default (%q), want none -- a default is not what 'no usage reported' means here", tc.column, hasDefault)
		}
	}
}

// TestInsertAttemptRoundTripsUsage proves engine.EngineStore's hand-written
// InsertAttempt/Attempts pair (not sqlc-generated -- this table has always
// been hand-written raw SQL in engine_store.go) persists the §13.2 Usage
// block exactly, including the independent nullability of Cost and
// Currency, and that an attempt recorded with no Usage block at all reads
// back with a nil Usage rather than a zero-valued one.
func TestInsertAttemptRoundTripsUsage(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()
	_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "a")

	cost := 0.0042
	currency := "USD"

	cases := []struct {
		name  string
		usage *engine.Usage
	}{
		{"no usage block at all", nil},
		{"tokens reported, actor did not price its work", &engine.Usage{InputTokens: 10, OutputTokens: 20}},
		{"tokens and price both reported", &engine.Usage{InputTokens: 100, OutputTokens: 200, Cost: &cost, Currency: &currency}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attemptID := store.NewULID()
			err := es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
				number, err := tx.NextAttemptNumber(ctx, nodeRunID)
				if err != nil {
					return err
				}
				return tx.InsertAttempt(ctx, engine.Attempt{
					ID: attemptID, NamespaceID: ns.ID, NodeRunID: nodeRunID,
					Number: number, Status: engine.StatusSucceeded, Usage: tc.usage,
				})
			})
			if err != nil {
				t.Fatalf("InsertAttempt: %v", err)
			}

			attempts, err := es.Attempts(ctx, nodeRunID)
			if err != nil {
				t.Fatalf("Attempts: %v", err)
			}
			var got *engine.Attempt
			for i := range attempts {
				if attempts[i].ID == attemptID {
					got = &attempts[i]
				}
			}
			if got == nil {
				t.Fatalf("attempt %s not found among %d attempts", attemptID, len(attempts))
			}

			if tc.usage == nil {
				if got.Usage != nil {
					t.Fatalf("Usage = %+v, want nil for an attempt that reported none", got.Usage)
				}
				return
			}
			if got.Usage == nil {
				t.Fatalf("Usage = nil, want %+v", tc.usage)
			}
			if got.Usage.InputTokens != tc.usage.InputTokens || got.Usage.OutputTokens != tc.usage.OutputTokens {
				t.Errorf("tokens = %d/%d, want %d/%d",
					got.Usage.InputTokens, got.Usage.OutputTokens, tc.usage.InputTokens, tc.usage.OutputTokens)
			}
			if (tc.usage.Cost == nil) != (got.Usage.Cost == nil) {
				t.Fatalf("Cost = %v, want %v (nullability must round-trip independently of the token counts)", got.Usage.Cost, tc.usage.Cost)
			}
			if tc.usage.Cost != nil && *got.Usage.Cost != *tc.usage.Cost {
				t.Errorf("Cost = %v, want %v", *got.Usage.Cost, *tc.usage.Cost)
			}
			if (tc.usage.Currency == nil) != (got.Usage.Currency == nil) {
				t.Fatalf("Currency = %v, want %v", got.Usage.Currency, tc.usage.Currency)
			}
			if tc.usage.Currency != nil && *got.Usage.Currency != *tc.usage.Currency {
				t.Errorf("Currency = %v, want %v", *got.Usage.Currency, *tc.usage.Currency)
			}
		})
	}
}

// TestInsertAttemptWithoutUsageLeavesColumnsNull proves the raw row a
// technical-failure completion writes (no Usage block reported) has every
// usage_* column NULL in the database, not merely nil at the Go level --
// the literal claim task t1's acceptance makes ("attempts completed
// without usage stay NULL").
func TestInsertAttemptWithoutUsageLeavesColumnsNull(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()
	_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "a")

	attemptID := store.NewULID()
	err := es.InTx(ctx, func(ctx context.Context, tx engine.Tx) error {
		return tx.InsertAttempt(ctx, engine.Attempt{
			ID: attemptID, NamespaceID: ns.ID, NodeRunID: nodeRunID,
			Number: 1, Status: engine.StatusFailed,
		})
	})
	if err != nil {
		t.Fatalf("InsertAttempt: %v", err)
	}

	var (
		inputTokens, outputTokens, cost, currency any
	)
	s := requireStore(t)
	if err := s.Pool().QueryRow(ctx,
		`SELECT usage_input_tokens, usage_output_tokens, usage_cost, usage_currency FROM attempts WHERE id = $1`,
		attemptID,
	).Scan(&inputTokens, &outputTokens, &cost, &currency); err != nil {
		t.Fatalf("read attempt row: %v", err)
	}
	if inputTokens != nil || outputTokens != nil || cost != nil || currency != nil {
		t.Errorf("usage columns = (%v, %v, %v, %v), want all NULL", inputTokens, outputTokens, cost, currency)
	}
}
