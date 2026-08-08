package sqs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/events"
	"github.com/agentculture/culture-nodes/internal/queue"
	queuesqs "github.com/agentculture/culture-nodes/internal/queue/sqs"
	"github.com/agentculture/culture-nodes/internal/store"
	storepg "github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// These four tests are the deliverable's chaos suite; see doc.go's "Chaos
// tests map to the §20.4 recovery matrix" section for what each one proves
// and why.

// TestChaosDuplicateDeliveryClaimedExactlyOnce proves prd-spec §20.4's "SQS
// signal is duplicated | PostgreSQL claim permits one current owner": 20
// real work_items rows exist, 20 WorkRefs are published, forced duplication
// pushes total deliveries above 20, and a consumer loop that performs a
// real fenced Postgres claim on every delivery still completes every work
// item exactly once -- because the extra deliveries' claims find nothing
// left and are refused, not because the queue prevented the duplicate from
// arriving in the first place.
func TestChaosDuplicateDeliveryClaimedExactlyOnce(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "test-sqs-chaos-duplicate")

	const n = 20
	runID := mustWorkflowRun(t, s, ns.ID)
	for i := 0; i < n; i++ {
		if err := s.EnqueueWork(ctx, storepg.WorkItem{
			NamespaceID: ns.ID,
			NodeRunID:   mustNodeRunForRun(t, s, ns.ID, runID),
		}); err != nil {
			t.Fatalf("EnqueueWork #%d: %v", i, err)
		}
	}

	f := newFakeSQS(t)
	// Force every delivered message to be duplicated exactly once (see
	// fakeMessage.duplicated), for a deterministic total of 2*n
	// deliveries across n published refs.
	f.chaos.duplicateProbability = 1.0
	d := f.driver(t, queuesqs.Config{})

	for i := 0; i < n; i++ {
		ref := queue.WorkRef{WorkID: fmt.Sprintf("wrk_dup_%02d", i), NamespaceID: ns.ID}
		if err := d.Publish(ctx, ref); err != nil {
			t.Fatalf("Publish #%d: %v", i, err)
		}
	}

	const wantDeliveries = 2 * n
	const workerID = "chaos-duplicate-worker"

	totalDeliveries := 0
	claimRefused := 0
	completed := make(map[string]bool)

	deadline := time.Now().Add(20 * time.Second)
	for totalDeliveries < wantDeliveries && time.Now().Before(deadline) {
		deliveries, err := d.Receive(ctx, 10, 500*time.Millisecond)
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		for _, del := range deliveries {
			totalDeliveries++

			// The fenced claim simulation the task asks for: a delivery
			// is never trusted on its own (see internal/queue's package
			// doc) -- the consumer always performs a real Postgres claim
			// against work_items regardless of which ref the signal
			// named.
			claimed, err := s.ClaimWork(ctx, workerID, 30*time.Second, 1)
			if err != nil {
				t.Fatalf("ClaimWork: %v", err)
			}
			if len(claimed) == 0 {
				claimRefused++
			} else {
				cw := claimed[0]
				if completed[cw.ID] {
					t.Fatalf("work item %s claimed and completed more than once", cw.ID)
				}
				if err := s.CompleteWork(ctx, cw.ID, workerID, cw.FencingToken, int(cw.Attempt)); err != nil {
					t.Fatalf("CompleteWork(%s): %v", cw.ID, err)
				}
				completed[cw.ID] = true
			}

			if err := d.Ack(ctx, del); err != nil {
				t.Fatalf("Ack: %v", err)
			}
		}
	}

	if totalDeliveries != wantDeliveries {
		t.Fatalf("observed %d total deliveries, want exactly %d (forced 1x duplication of %d refs)", totalDeliveries, wantDeliveries, n)
	}
	if len(completed) != n {
		t.Fatalf("completed %d distinct work items, want exactly %d", len(completed), n)
	}
	if claimRefused == 0 {
		t.Fatal("claimRefused == 0, want at least one duplicate delivery's fenced claim to have found nothing left to claim")
	}
	if got := countWorkItemsInState(t, s, ns.ID, "completed"); got != n {
		t.Fatalf("work_items rows in state completed = %d, want %d (direct DB check, not just the test's own bookkeeping)", got, n)
	}
}

// TestChaosReorderedDeliveryAllEventuallyProcessed proves the design's
// "a message received out of order cannot overwrite newer state" property
// from the receiving side: a bounded reorder window scrambles delivery
// order relative to publish order, yet every published ref is still
// received exactly once, with no crash and no assumption about ordering
// anywhere in this test itself.
func TestChaosReorderedDeliveryAllEventuallyProcessed(t *testing.T) {
	f := newFakeSQS(t)
	f.chaos.reorderWindow = 5
	d := f.driver(t, queuesqs.Config{})
	ctx := context.Background()

	const n = 15
	publishOrder := make([]string, n)
	for i := 0; i < n; i++ {
		wid := fmt.Sprintf("wrk_reorder_%02d", i)
		publishOrder[i] = wid
		if err := d.Publish(ctx, queue.WorkRef{WorkID: wid, NamespaceID: "ns_reorder"}); err != nil {
			t.Fatalf("Publish #%d: %v", i, err)
		}
	}

	var deliveredOrder []string
	seen := make(map[string]bool)
	deadline := time.Now().Add(10 * time.Second)
	for len(seen) < n && time.Now().Before(deadline) {
		deliveries, err := d.Receive(ctx, 3, 500*time.Millisecond)
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		for _, del := range deliveries {
			if seen[del.WorkID] {
				t.Fatalf("received %s more than once (no duplicate chaos configured for this test)", del.WorkID)
			}
			seen[del.WorkID] = true
			deliveredOrder = append(deliveredOrder, del.WorkID)
			if err := d.Ack(ctx, del); err != nil {
				t.Fatalf("Ack: %v", err)
			}
		}
	}

	if len(seen) != n {
		t.Fatalf("received %d/%d distinct refs before the deadline, want all %d eventually processed despite reordering", len(seen), n, n)
	}
	if reflect.DeepEqual(deliveredOrder, publishOrder) {
		t.Fatal("delivered order exactly matched publish order -- the reorder buffer chaos knob did not actually fire, so this test proved nothing")
	}
}

// TestChaosDroppedSendRepairedByOutboxRelay proves prd-spec §20.4's "SQS
// publication is missed | Outbox republishes": a forced SendMessage failure
// (this driver's Publish returning an error) leaves internal/events.Relay's
// outbox row 'pending' rather than 'published' -- Relay's own documented
// at-least-once behavior, see relay.go's doc comment -- and once the chaos
// is lifted, a later Relay.Run republishes it and Receive picks it up.
func TestChaosDroppedSendRepairedByOutboxRelay(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "test-sqs-chaos-drop")

	row, err := s.InsertOutbox(ctx, storepg.InsertOutboxInput{
		NamespaceID: ns.ID,
		Topic:       events.TypeNodeRunReady,
		Payload:     json.RawMessage(`{"node_run_id":"nr_chaos_drop"}`),
	})
	if err != nil {
		t.Fatalf("InsertOutbox: %v", err)
	}

	f := newFakeSQS(t)
	f.setDropSendFor(row.ID)
	// No retries: the point of this test is to observe one forced
	// SendMessage failure directly, not have the SDK's own retry policy
	// mask it.
	d := f.driver(t, queuesqs.Config{MaxAttempts: 1})

	sink := func(context.Context, events.Envelope) error { return nil }
	relay := events.NewRelay(s.Pool(), d, sink, events.RelayOptions{Source: "nodes-test"})

	if err := relay.Run(ctx); err == nil {
		t.Fatal("Run with drop-on-send chaos active returned nil, want the simulated SendMessage failure to surface")
	}

	if status := outboxStatus(t, s, row.ID); status != "pending" {
		t.Fatalf("outbox row status = %q after a dropped send, want pending (not yet repaired)", status)
	}

	deliveries, err := d.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive before repair: %v", err)
	}
	if hasDelivery(deliveries, row.ID) {
		t.Fatalf("Receive saw a signal for outbox row %s despite the simulated drop", row.ID)
	}

	// Lift the chaos: a later relay run (e.g. the next scheduler tick)
	// must repair the gap on its own, with no special-cased "retry the
	// dropped one" logic anywhere in this test.
	f.clearDropSendFor()

	if err := relay.Run(ctx); err != nil {
		t.Fatalf("Run after lifting chaos: %v", err)
	}

	if status := outboxStatus(t, s, row.ID); status != "published" {
		t.Fatalf("outbox row status = %q after repair, want published", status)
	}

	deliveries, err = d.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive after repair: %v", err)
	}
	if !hasDelivery(deliveries, row.ID) {
		t.Fatalf("Receive after repair did not return a signal for outbox row %s, want the relay to have repaired the dropped publication", row.ID)
	}
}

// TestChaosUnknownSchemaVersionSkippedDiagnosticNotCrash proves the
// forward-compatibility rule from the package doc's "Message body" section
// directly: a message with an unrecognized "v", and one whose body is not
// JSON at all, both sit among otherwise-normal messages. Receive must skip
// both (each producing a diagnostic through Config.Logf) and still return
// every well-formed delivery, never erroring or panicking because of the
// bad ones.
func TestChaosUnknownSchemaVersionSkippedDiagnosticNotCrash(t *testing.T) {
	f := newFakeSQS(t)

	var mu sync.Mutex
	var diagnostics []string
	d := f.driver(t, queuesqs.Config{
		Logf: func(format string, args ...any) {
			mu.Lock()
			diagnostics = append(diagnostics, fmt.Sprintf(format, args...))
			mu.Unlock()
		},
	})
	ctx := context.Background()

	good1 := queue.WorkRef{WorkID: "wrk_good_1", NamespaceID: "ns_schema"}
	good2 := queue.WorkRef{WorkID: "wrk_good_2", NamespaceID: "ns_schema"}

	if err := d.Publish(ctx, good1); err != nil {
		t.Fatalf("Publish good1: %v", err)
	}
	// A message written by an incompatible producer: same envelope shape,
	// a schema version this driver does not understand.
	f.injectRawMessage(`{"v":99,"work_id":"wrk_from_the_future","namespace_id":"ns_schema"}`)
	if err := d.Publish(ctx, good2); err != nil {
		t.Fatalf("Publish good2: %v", err)
	}
	// A message that is not even valid JSON.
	f.injectRawMessage(`not-json-at-all`)

	var deliveries []queue.Delivery
	seen := make(map[string]bool)
	deadline := time.Now().Add(5 * time.Second)
	for len(seen) < 2 && time.Now().Before(deadline) {
		batch, err := d.Receive(ctx, 10, 500*time.Millisecond)
		if err != nil {
			t.Fatalf("Receive: %v, want it to skip the bad messages rather than error", err)
		}
		for _, del := range batch {
			seen[del.WorkID] = true
		}
		deliveries = append(deliveries, batch...)
	}

	if !seen[good1.WorkID] || !seen[good2.WorkID] {
		t.Fatalf("did not receive both well-formed refs (seen=%v), want the bad messages to be skipped without blocking the good ones", seen)
	}
	if len(deliveries) != 2 {
		t.Fatalf("Receive returned %d deliveries, want exactly 2 (the unsupported-version and invalid-JSON messages must never surface as deliveries)", len(deliveries))
	}

	mu.Lock()
	gotDiagnostics := len(diagnostics)
	mu.Unlock()
	if gotDiagnostics < 2 {
		t.Fatalf("logged %d diagnostics, want at least 2 (one per skipped message): %v", gotDiagnostics, diagnostics)
	}
}

// mustWorkflowRun creates the fixture chain a work_items row's
// node_run_id foreign key requires above the node_run itself: a
// workflow_version and a run. This mirrors
// internal/store/postgres/claiming_test.go's mustNodeRun (unexported in
// that package, so not importable from here) -- runs/node_runs are
// out of this task's scope (t9 owns their typed Store methods), so this
// inserts them with raw SQL via s.Pool(), the same escape hatch that
// package's own tests use.
func mustWorkflowRun(t *testing.T, s *storepg.Store, namespaceID string) string {
	t.Helper()
	ctx := context.Background()

	wv, err := s.CreateWorkflowVersion(ctx, storepg.CreateWorkflowVersionInput{
		NamespaceID:   namespaceID,
		WorkflowKey:   "test-sqs-chaos-workflow-" + store.NewULID(),
		Version:       1,
		SourceFormat:  "yaml",
		Source:        "entrypoint: intake\n",
		ContentDigest: "sha256:" + store.NewULID(),
	})
	if err != nil {
		t.Fatalf("mustWorkflowRun: CreateWorkflowVersion: %v", err)
	}

	runID := store.NewULID()
	if _, err := s.Pool().Exec(ctx,
		`INSERT INTO runs (id, namespace_id, workflow_version_id) VALUES ($1, $2, $3)`,
		runID, namespaceID, wv.ID,
	); err != nil {
		t.Fatalf("mustWorkflowRun: insert run: %v", err)
	}
	return runID
}

// mustNodeRunForRun inserts one node_run against an already-created run --
// TestChaosDuplicateDeliveryClaimedExactlyOnce needs 20 distinct
// node_run_id values (work_items has no unique constraint on node_run_id,
// but giving every enqueued item its own keeps the fixture unambiguous),
// all sharing one workflow_version/run so the test does not pay for 20
// separate fixture chains.
func mustNodeRunForRun(t *testing.T, s *storepg.Store, namespaceID, runID string) string {
	t.Helper()
	nodeRunID := store.NewULID()
	if _, err := s.Pool().Exec(context.Background(),
		`INSERT INTO node_runs (id, namespace_id, run_id, node_key) VALUES ($1, $2, $3, 'intake')`,
		nodeRunID, namespaceID, runID,
	); err != nil {
		t.Fatalf("mustNodeRunForRun: insert node_run: %v", err)
	}
	return nodeRunID
}

// countWorkItemsInState is a direct DB check (bypassing any typed Store
// method) so TestChaosDuplicateDeliveryClaimedExactlyOnce verifies what was
// actually committed, not just what the test's own bookkeeping observed.
func countWorkItemsInState(t *testing.T, s *storepg.Store, namespaceID, state string) int {
	t.Helper()
	var count int
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM work_items WHERE namespace_id = $1 AND state = $2`,
		namespaceID, state,
	).Scan(&count); err != nil {
		t.Fatalf("count work_items: %v", err)
	}
	return count
}

// outboxStatus reads status directly from the outbox table, mirroring
// internal/events's own relay_test.go helper.
func outboxStatus(t *testing.T, s *storepg.Store, id string) string {
	t.Helper()
	var st string
	var pub pgtype.Timestamptz
	if err := s.Pool().QueryRow(context.Background(),
		`SELECT status, published_at FROM outbox WHERE id = $1`, id,
	).Scan(&st, &pub); err != nil {
		t.Fatalf("query outbox row %s: %v", id, err)
	}
	return st
}
