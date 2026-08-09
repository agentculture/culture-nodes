package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// TestMigration0010CreatesUpdatedAtIndexes proves migrations/0010 shipped
// exactly the two listing indexes it documents, and nothing else --
// expand-only (docs/adr/0002-migration-policy.md). A migration that only
// adds indexes changes no column, table, or constraint a binary reads or
// writes, so it needs no N-1 compatibility test beyond this: a binary built
// before 0010 existed keeps reading and writing runs/node_runs exactly as
// before, unaware the new indexes exist, and scripts/n1-compat.sh (which
// only exercises `nodes migrate`, the one code path that ever needs to know
// a migration file's contents) passes unaffected by this file.
func TestMigration0010CreatesUpdatedAtIndexes(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	for _, tc := range []struct {
		table string
		index string
	}{
		{"runs", "runs_namespace_updated_at_idx"},
		{"node_runs", "node_runs_namespace_updated_at_idx"},
	} {
		var exists bool
		err := s.Pool().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = $1 AND indexname = $2)`,
			tc.table, tc.index,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check index %s on %s: %v", tc.index, tc.table, err)
		}
		if !exists {
			t.Fatalf("index %s not found on table %s -- migrations/0010 should have created it", tc.index, tc.table)
		}
	}
}

// TestRunsUpdatedAtRangeQueryUsesIndexScan proves
// runs_namespace_updated_at_idx (migrations/0010) is the plan Postgres
// actually picks for the query shape task t11 adds to GET /v1alpha1/runs:
// a namespace-scoped `updated_at` range filter (`updated_since`/
// `updated_until`) sorted newest-first with a page limit. 2000 fixture
// rows spread over ~83 days make the range genuinely selective and the
// ORDER BY + LIMIT genuinely benefit from an already-sorted index walk, so
// the planner's own cost model -- not a forced planner setting -- is what
// picks the index. See this test's t.Log for the captured EXPLAIN text.
func TestRunsUpdatedAtRangeQueryUsesIndexScan(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-runs-updated-idx")
	wv, err := s.CreateWorkflowVersion(ctx, postgres.CreateWorkflowVersionInput{
		NamespaceID:   ns.ID,
		WorkflowKey:   "explain-fixture",
		Version:       1,
		SourceFormat:  "yaml",
		Source:        "entrypoint: intake\n",
		ContentDigest: "sha256:" + store.NewULID(),
	})
	if err != nil {
		t.Fatalf("CreateWorkflowVersion: %v", err)
	}

	const total = 2000
	base := time.Now().Add(-time.Duration(total) * time.Hour)
	ids := make([]string, total)
	updatedAts := make([]time.Time, total)
	for i := 0; i < total; i++ {
		ids[i] = store.NewULID()
		updatedAts[i] = base.Add(time.Duration(i) * time.Hour)
	}

	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO runs (id, namespace_id, workflow_version_id, status, created_at, updated_at)
		SELECT rid, $1, $2, 'completed', uat, uat
		FROM unnest($3::text[], $4::timestamptz[]) AS t(rid, uat)
	`, ns.ID, wv.ID, ids, updatedAts); err != nil {
		t.Fatalf("bulk insert fixture runs: %v", err)
	}
	if _, err := s.Pool().Exec(ctx, `ANALYZE runs`); err != nil {
		t.Fatalf("ANALYZE runs: %v", err)
	}

	// A narrow, recent window: the last 24 rows out of 2000 -- selective
	// enough (and small enough relative to LIMIT) that a sorted index walk
	// beats a sequential scan + sort under the planner's own cost model.
	windowStart := updatedAts[total-24]
	windowEnd := updatedAts[total-1]

	plan := explainText(t, s, ctx, `
		SELECT id FROM runs
		WHERE namespace_id = $1 AND updated_at >= $2 AND updated_at <= $3
		ORDER BY updated_at DESC
		LIMIT 20
	`, ns.ID, windowStart, windowEnd)

	t.Logf("EXPLAIN for runs updated_at range query:\n%s", plan)

	if !strings.Contains(plan, "Index Scan") {
		t.Fatalf("plan does not use an Index Scan (want runs_namespace_updated_at_idx):\n%s", plan)
	}
	if !strings.Contains(plan, "runs_namespace_updated_at_idx") {
		t.Fatalf("plan uses an Index Scan but not runs_namespace_updated_at_idx:\n%s", plan)
	}
}

// TestNodeRunsUpdatedAtRangeQueryUsesIndexScan is
// TestRunsUpdatedAtRangeQueryUsesIndexScan's sibling for the cross-run
// node_runs "jobs timeline" listing task t15 adds: same namespace-scoped
// `updated_at` range + newest-first + LIMIT shape, over node_runs instead
// of runs, proving node_runs_namespace_updated_at_idx (migrations/0010) is
// the plan Postgres picks.
func TestNodeRunsUpdatedAtRangeQueryUsesIndexScan(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-node-runs-updated-idx")
	wv, err := s.CreateWorkflowVersion(ctx, postgres.CreateWorkflowVersionInput{
		NamespaceID:   ns.ID,
		WorkflowKey:   "explain-fixture",
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

	const total = 2000
	base := time.Now().Add(-time.Duration(total) * time.Hour)
	ids := make([]string, total)
	updatedAts := make([]time.Time, total)
	for i := 0; i < total; i++ {
		ids[i] = store.NewULID()
		updatedAts[i] = base.Add(time.Duration(i) * time.Hour)
	}

	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO node_runs (id, namespace_id, run_id, node_key, status, created_at, updated_at)
		SELECT nrid, $1, $2, 'fixture-node', 'completed', uat, uat
		FROM unnest($3::text[], $4::timestamptz[]) AS t(nrid, uat)
	`, ns.ID, runID, ids, updatedAts); err != nil {
		t.Fatalf("bulk insert fixture node_runs: %v", err)
	}
	if _, err := s.Pool().Exec(ctx, `ANALYZE node_runs`); err != nil {
		t.Fatalf("ANALYZE node_runs: %v", err)
	}

	windowStart := updatedAts[total-24]
	windowEnd := updatedAts[total-1]

	plan := explainText(t, s, ctx, `
		SELECT id FROM node_runs
		WHERE namespace_id = $1 AND updated_at >= $2 AND updated_at <= $3
		ORDER BY updated_at DESC
		LIMIT 20
	`, ns.ID, windowStart, windowEnd)

	t.Logf("EXPLAIN for node_runs updated_at range query:\n%s", plan)

	if !strings.Contains(plan, "Index Scan") {
		t.Fatalf("plan does not use an Index Scan (want node_runs_namespace_updated_at_idx):\n%s", plan)
	}
	if !strings.Contains(plan, "node_runs_namespace_updated_at_idx") {
		t.Fatalf("plan uses an Index Scan but not node_runs_namespace_updated_at_idx:\n%s", plan)
	}
}

// explainText runs `EXPLAIN <query>` (no ANALYZE -- this asserts the
// chosen plan shape, not runtime) and returns the plan as one newline-joined
// string.
func explainText(t *testing.T, s *postgres.Store, ctx context.Context, query string, args ...any) string {
	t.Helper()
	rows, err := s.Pool().Query(ctx, "EXPLAIN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("EXPLAIN: scan: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	return strings.Join(lines, "\n")
}
