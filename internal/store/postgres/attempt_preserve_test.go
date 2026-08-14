package postgres_test

import (
	"context"
	"testing"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/store"
)

// Per-attempt preserve-branch carriage (task t26, issue #49, spec claim c32
// / honesty h21, migrations/0025_attempt_preserve_branch.sql).
//
// Task t25 mints the preserve-on-failure branch and reports pushed vs
// local-only bridge-side, but stopped at the bridge (the failed
// event/error body). These tests pin the two halves that make it durable
// past the worker process that first reads that payload: the three columns
// exist, nullable, expand-only (0012/0017/0018's precedent), and the
// ordinary InsertAttempt/Attempts round trip preserves engine.Preserve
// exactly — including its absence.

// TestMigration0025AddsNullablePreserveColumns is 0018's test, repeated for
// the three new columns: text/text/boolean, all nullable, no default. A
// default would make "this attempt has no preserve branch to show" — the
// overwhelming common case, including every successful attempt —
// unrepresentable.
func TestMigration0025AddsNullablePreserveColumns(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	cases := []struct {
		column   string
		dataType string
	}{
		{column: "preserve_branch", dataType: "text"},
		{column: "preserve_pushed", dataType: "boolean"},
		{column: "preserve_remote", dataType: "text"},
	}
	for _, tc := range cases {
		t.Run(tc.column, func(t *testing.T) {
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
				t.Errorf("attempts.%s has a default (%q), want none", tc.column, hasDefault)
			}
		})
	}
}

// TestInsertAttemptRoundTripsPreserve proves engine.Attempt.Preserve
// survives the ordinary write/read path unaltered, and that an attempt
// which reported none leaves all three columns NULL — never a fabricated
// branch, never "pushed: false" standing in for "not reported".
func TestInsertAttemptRoundTripsPreserve(t *testing.T) {
	es, ns := newEngineStore(t)
	ctx := context.Background()
	_, _, nodeRunID, _ := seedRun(t, es, ns.ID, "a")

	cases := []struct {
		name     string
		preserve *engine.Preserve
	}{
		{
			name: "pushed to the configured remote",
			preserve: &engine.Preserve{
				Branch: "preserve/run-01J-att-01K-20260813T120000Z-ab12cd",
				Pushed: true,
				Remote: "origin",
			},
		},
		{
			name: "local-only: push failed or was disabled",
			preserve: &engine.Preserve{
				Branch: "preserve/run-01J-att-01L-20260813T120500Z-ef34gh",
				Pushed: false,
				Remote: "origin",
			},
		},
		{name: "the bridge reported no preserve branch: the column stays NULL", preserve: nil},
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
					Number: number, Status: engine.StatusFailed,
					Preserve: tc.preserve,
				})
			})
			if err != nil {
				t.Fatalf("InsertAttempt: %v", err)
			}

			got := findAttempt(t, es, nodeRunID, attemptID)
			if !equalPreserve(got.Preserve, tc.preserve) {
				t.Errorf("Preserve = %+v, want %+v", got.Preserve, tc.preserve)
			}

			if tc.preserve != nil {
				return
			}
			var branch, remote any
			var pushed any
			if err := requireStore(t).Pool().QueryRow(ctx,
				`SELECT preserve_branch, preserve_pushed, preserve_remote FROM attempts WHERE id = $1`, attemptID,
			).Scan(&branch, &pushed, &remote); err != nil {
				t.Fatalf("read attempt row: %v", err)
			}
			if branch != nil || pushed != nil || remote != nil {
				t.Errorf("preserve columns = (%v, %v, %v), want all NULL", branch, pushed, remote)
			}
		})
	}
}

func equalPreserve(a, b *engine.Preserve) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return a.Branch == b.Branch && a.Pushed == b.Pushed && a.Remote == b.Remote
}
