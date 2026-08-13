package postgres_test

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
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

// TestMigration0017AddsNullableExtendedUsageColumns is 0012's test for
// migrations/0017: the five columns that carry the extended per-attempt
// telemetry (cache and reasoning token counts, the model and provider
// thread the turn ran on, and how the turn terminated) are nullable with no
// default, for exactly 0012's reason -- an attempt that reported none of
// them stays NULL end to end, and a binary built before 0017 keeps
// inserting and reading attempts with its own fixed column list.
func TestMigration0017AddsNullableExtendedUsageColumns(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	for _, tc := range []struct {
		column   string
		dataType string
	}{
		{"usage_cached_input_tokens", "bigint"},
		{"usage_reasoning_tokens", "bigint"},
		{"usage_model", "text"},
		{"usage_thread_id", "text"},
		{"termination_reason", "text"},
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
			t.Errorf("attempts.%s has a default (%q), want none -- a default is not what 'not reported' means here", tc.column, hasDefault)
		}
	}
}

// TestInsertAttemptRoundTripsExtendedUsage proves the migration-0017 fields
// survive InsertAttempt/Attempts unaltered, and that each of them is
// independently absent-able:
//
//   - a bridge that reported everything round-trips everything;
//   - a bridge that reported only 0012's four fields leaves all five new
//     columns NULL rather than zero-filled (honesty h1: a backend with no
//     cache telemetry, e.g. colleague, is unmeasurable, not free);
//   - a turn that terminated for a knowable reason but produced no parseable
//     usage block records the reason with every usage_* column still NULL --
//     which is why the termination reason is NOT a field of the §13.2 usage
//     block: carrying it there would force a usage block (and therefore
//     fabricated zero token counts) onto an attempt that never reported one,
//     breaking `usage_input_tokens IS NOT NULL` as the "usage reported"
//     sentinel.
func TestInsertAttemptRoundTripsExtendedUsage(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()
	_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "a")

	cached := int64(9984)
	reasoning := int64(320)
	model := "gpt-5-codex"
	threadID := "thr_01JQZ"
	reason := "max_output_tokens"

	cases := []struct {
		name              string
		usage             *engine.Usage
		terminationReason *string
	}{
		{
			name: "every extended field reported",
			usage: &engine.Usage{
				InputTokens: 13880, OutputTokens: 512,
				CachedInputTokens: &cached, ReasoningTokens: &reasoning,
				Model: &model, ThreadID: &threadID,
			},
			terminationReason: &reason,
		},
		{
			name:  "0012 fields only: the extended columns stay NULL",
			usage: &engine.Usage{InputTokens: 10, OutputTokens: 20},
		},
		{
			name:              "termination reason without any usage block",
			terminationReason: &reason,
		},
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
					Number: number, Status: engine.StatusSucceeded,
					Usage: tc.usage, TerminationReason: tc.terminationReason,
				})
			})
			if err != nil {
				t.Fatalf("InsertAttempt: %v", err)
			}

			got := findAttempt(t, es, nodeRunID, attemptID)

			if !equalStringPtr(got.TerminationReason, tc.terminationReason) {
				t.Errorf("TerminationReason = %v, want %v",
					derefString(got.TerminationReason), derefString(tc.terminationReason))
			}
			if tc.usage == nil {
				if got.Usage != nil {
					t.Fatalf("Usage = %+v, want nil for an attempt that reported none", got.Usage)
				}
				assertUsageColumnsNull(t, attemptID)
				return
			}
			if got.Usage == nil {
				t.Fatalf("Usage = nil, want %+v", tc.usage)
			}
			if !equalInt64Ptr(got.Usage.CachedInputTokens, tc.usage.CachedInputTokens) {
				t.Errorf("CachedInputTokens = %v, want %v",
					derefInt64(got.Usage.CachedInputTokens), derefInt64(tc.usage.CachedInputTokens))
			}
			if !equalInt64Ptr(got.Usage.ReasoningTokens, tc.usage.ReasoningTokens) {
				t.Errorf("ReasoningTokens = %v, want %v",
					derefInt64(got.Usage.ReasoningTokens), derefInt64(tc.usage.ReasoningTokens))
			}
			if !equalStringPtr(got.Usage.Model, tc.usage.Model) {
				t.Errorf("Model = %v, want %v", derefString(got.Usage.Model), derefString(tc.usage.Model))
			}
			if !equalStringPtr(got.Usage.ThreadID, tc.usage.ThreadID) {
				t.Errorf("ThreadID = %v, want %v", derefString(got.Usage.ThreadID), derefString(tc.usage.ThreadID))
			}
		})
	}
}

// assertUsageColumnsNull proves an attempt row's four 0012 usage columns are
// NULL in the database, not merely nil at the Go level.
func assertUsageColumnsNull(t *testing.T, attemptID string) {
	t.Helper()
	s := requireStore(t)
	var inputTokens, outputTokens, cached, reasoning, model, threadID any
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT usage_input_tokens, usage_output_tokens, usage_cached_input_tokens,
		        usage_reasoning_tokens, usage_model, usage_thread_id
		 FROM attempts WHERE id = $1`,
		attemptID,
	).Scan(&inputTokens, &outputTokens, &cached, &reasoning, &model, &threadID); err != nil {
		t.Fatalf("read attempt row: %v", err)
	}
	if inputTokens != nil || outputTokens != nil || cached != nil || reasoning != nil || model != nil || threadID != nil {
		t.Errorf("usage columns = (%v, %v, %v, %v, %v, %v), want all NULL",
			inputTokens, outputTokens, cached, reasoning, model, threadID)
	}
}

// findAttempt reads one attempt back through the normal Attempts read path.
func findAttempt(t *testing.T, es *postgres.EngineStore, nodeRunID, attemptID string) engine.Attempt {
	t.Helper()
	attempts, err := es.Attempts(context.Background(), nodeRunID)
	if err != nil {
		t.Fatalf("Attempts: %v", err)
	}
	for i := range attempts {
		if attempts[i].ID == attemptID {
			return attempts[i]
		}
	}
	t.Fatalf("attempt %s not found among %d attempts", attemptID, len(attempts))
	return engine.Attempt{}
}

func equalStringPtr(a, b *string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func equalInt64Ptr(a, b *int64) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func derefString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func derefInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
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
