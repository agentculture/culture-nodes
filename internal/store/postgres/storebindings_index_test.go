package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// TestMigration0045CreatesResolutionIndex proves migrations/0045 shipped the
// one index it documents — expand-only (docs/adr/0002-migration-policy.md).
// An index-only migration changes no column, table, or constraint a binary
// reads or writes, so this existence check plus the plan test below is its
// whole coverage surface (the 0010 shape, updated_at_index_test.go).
func TestMigration0045CreatesResolutionIndex(t *testing.T) {
	s := requireStore(t)

	var exists bool
	err := s.Pool().QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = $1 AND indexname = $2)`,
		"store_entry_bindings", "store_entry_bindings_ref_current_idx",
	).Scan(&exists)
	if err != nil {
		t.Fatalf("check index store_entry_bindings_ref_current_idx: %v", err)
	}
	if !exists {
		t.Fatal("index store_entry_bindings_ref_current_idx not found -- migrations/0045 should have created it")
	}
}

// TestCurrentBindingsByRefQueryUsesIndexScan proves
// store_entry_bindings_ref_current_idx (migrations/0045) is the plan
// PostgreSQL actually picks for the literal query
// currentStoreEntryBindingsByRef issues (storebindings.go) — the dispatch
// path's per-entry current-binding read (PR #209 qodo finding 1). Bindings
// are insert-only records whose superseded rows accumulate by design, so the
// fixture is a genuinely grown registry: 40 entries each rebound 50 times to
// one hot ref (2000 rows), amid ten other refs' equally grown trails
// (20000 noise rows). What the index buys — and what this test pins — is
// that the read touches only the hot ref's own rows: without it the plan is
// a Seq Scan over every ref and namespace in the table, and that is the
// ever-growing cost on the dispatch path. It deliberately does NOT pin the
// plan above the index: DISTINCT ON visits every matched row either way
// (PostgreSQL has no skip scan), so the planner may take the ordered index
// walk or a bitmap scan plus an in-memory sort of the matched rows,
// whichever its own cost model — not a forced setting — prices cheaper at
// the fixture's size. The noise refs matter: a table holding only the hot
// ref makes the full-table plan spuriously competitive, exactly the
// degenerate shape a fresh install has and a grown registry does not. The
// two query strings are kept byte-for-byte in sync by hand; if
// currentStoreEntryBindingsByRef's query text changes, this test's literal
// copy must change with it.
func TestCurrentBindingsByRefQueryUsesIndexScan(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "storebinding-resolution-idx").ID

	actorID := insertBindingActor(t, s, ns, "local/codex-idx-fixture")

	const entryCount = 40
	entryIDs := make([]string, entryCount)
	for i := range entryIDs {
		entryIDs[i] = store.NewULID()
	}
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO store_entries (id, namespace_id, name, origin,
			graph_digest, graph_source_format, graph_source,
			evidence_manifest, entry_digest)
		SELECT eid, $1, 'idx-fixture-' || eid, 'local',
			'sha256:' || eid, 'yaml', 'entrypoint: intake' || chr(10),
			'{}'::jsonb, 'sha256:' || eid
		FROM unnest($2::text[]) AS t(eid)
	`, ns, entryIDs); err != nil {
		t.Fatalf("bulk insert fixture store_entries: %v", err)
	}

	// Raw bulk insert, deliberately not CreateStoreEntryBinding: the trail
	// being measured is 2000 superseded records per ref, and the create
	// path's conflict probe would issue the very query under test 4000
	// times to build it. Every row agrees on one key, as resolution
	// requires.
	const rebindsPerEntry = 50
	const perRef = entryCount * rebindsPerEntry
	refs := []string{"actor://codex@sha256:hot-fixture"}
	for i := 0; i < 10; i++ {
		refs = append(refs, fmt.Sprintf("runner://headspace@sha256:noise-fixture-%d", i))
	}
	base := time.Now().Add(-time.Duration(perRef) * time.Minute)
	for refIdx, ref := range refs {
		ids := make([]string, perRef)
		rowEntries := make([]string, perRef)
		createdAts := make([]time.Time, perRef)
		for i := 0; i < perRef; i++ {
			ids[i] = store.NewULID()
			rowEntries[i] = entryIDs[i/rebindsPerEntry]
			createdAts[i] = base.Add(time.Duration(i) * time.Minute)
		}
		kind := "actor"
		if refIdx > 0 {
			kind = "runner"
		}
		if _, err := s.Pool().Exec(ctx, `
			INSERT INTO store_entry_bindings (id, namespace_id, entry_id,
				required_ref, required_kind, bound_actor_id, bound_actor_key,
				bound_by, created_at)
			SELECT bid, $1, eid, $2, $3, $4, 'local/codex-idx-fixture',
				'operator@spark', cat
			FROM unnest($5::text[], $6::text[], $7::timestamptz[]) AS t(bid, eid, cat)
		`, ns, ref, kind, actorID, ids, rowEntries, createdAts); err != nil {
			t.Fatalf("bulk insert fixture bindings for %s: %v", ref, err)
		}
	}
	if _, err := s.Pool().Exec(ctx, `ANALYZE store_entry_bindings`); err != nil {
		t.Fatalf("ANALYZE store_entry_bindings: %v", err)
	}

	const storeEntryBindingColumns = `id, namespace_id, entry_id, required_ref, required_kind,
		bound_actor_id, bound_actor_key, bound_by, created_at`
	plan := explainText(t, s, ctx, fmt.Sprintf(`SELECT DISTINCT ON (entry_id) %s
		FROM store_entry_bindings
		WHERE namespace_id = $1 AND required_ref = $2
		ORDER BY entry_id, created_at DESC, id DESC`, storeEntryBindingColumns),
		ns, "actor://codex@sha256:hot-fixture")

	t.Logf("EXPLAIN for currentStoreEntryBindingsByRef's query:\n%s", plan)

	if !strings.Contains(plan, "store_entry_bindings_ref_current_idx") {
		t.Fatalf("plan does not use store_entry_bindings_ref_current_idx:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan") {
		t.Fatalf("plan still walks the whole bindings table -- the index should confine the read to the ref's own rows:\n%s", plan)
	}
}
