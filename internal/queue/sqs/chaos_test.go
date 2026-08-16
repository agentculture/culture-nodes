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
			claimed, err := s.ClaimWork(ctx, ns.ID, workerID, 30*time.Second, 1)
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
//
// # This test is not alone in the outbox (issue #126)
//
// internal/events.Relay selects pending outbox rows with NO namespace filter
// (see relay.go's runBatch). That is deliberate product behavior, not a bug:
// the relay is a deployment-level component and draining the whole outbox is
// exactly its job. The consequence for tests is that "the outbox" is shared
// state, and how much it is shared depends on how the suite is run:
//
//   - locally, pgtest.Run starts a private PostgreSQL per test *package*, so
//     this package's outbox really does contain only this package's rows;
//   - in CI, .github/workflows/tests.yml points every package at ONE
//     NODES_TEST_DATABASE_URL, so every package's rows land in the same
//     outbox table and every package's relay drains all of them.
//
// Under CI that produces two distinct ways for a namespace-blind assumption
// to fail. Foreign pending rows get published into *this* test's fake queue
// and crowd this test's own signal out of a single ten-message Receive page;
// and another package's relay can drain this test's row first, publishing it
// into a queue this test cannot read and marking it published.
//
// So this test must not assume it is alone in the outbox. It seeds foreign
// pending rows itself (see foreignPendingRows) so the crowding-out case is
// reproduced deterministically on every run rather than only under CI, it
// drains until its own row's signal arrives instead of demanding that signal
// be in the first page, and every assertion a foreign relay could invalidate
// names that possibility in its failure message rather than blaming the relay
// under test. The fix is defence-in-depth: it makes this test honest whatever
// the suite's database isolation happens to be. Refs #126, #122.
func TestChaosDroppedSendRepairedByOutboxRelay(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "test-sqs-chaos-drop")

	// Pending outbox rows this test does not own, standing in for whatever
	// else shares the outbox. They are inserted *before* this test's own row
	// so that -- store.NewULID being monotonic within a process and
	// relay.runBatch ordering by id -- the relay publishes every one of them
	// ahead of it, and there are deliberately more of them than
	// receivePageSize so this test's row cannot land in a single page. This
	// is the regression guard, not scaffolding: without it the pre-#126
	// "it must be in the first page" assertion passes locally and fails only
	// in CI; with it, that assertion fails everywhere.
	//
	// The cost, stated plainly: under a shared CI database these rows are
	// themselves foreign traffic for whatever else is running. They are
	// pending only between this insert and the repair Run below (tens of
	// milliseconds) and end up published like any other row, which is a small
	// price for reproducing the real namespace-blind relay path rather than
	// faking the symptom by injecting messages straight into the fake queue.
	const foreignPendingRows = 15
	foreignNS := pgtest.MustNamespace(t, s, "test-sqs-chaos-drop-foreign")
	for i := 0; i < foreignPendingRows; i++ {
		if _, err := s.InsertOutbox(ctx, storepg.InsertOutboxInput{
			NamespaceID: foreignNS.ID,
			Topic:       events.TypeNodeRunReady,
			Payload:     json.RawMessage(fmt.Sprintf(`{"node_run_id":"nr_chaos_drop_foreign_%02d"}`, i)),
		}); err != nil {
			t.Fatalf("InsertOutbox foreign row #%d: %v", i, err)
		}
	}

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
		t.Fatalf("Run with drop-on-send chaos active returned nil, want the simulated SendMessage failure to surface; outbox row %s now reads %q (if that is published, another relay sharing this outbox drained the row before this one reached it -- see this test's doc comment)",
			row.ID, outboxStatus(t, s, row.ID))
	}

	if status := outboxStatus(t, s, row.ID); status != "pending" {
		t.Fatalf("outbox row %s status = %q after a dropped send, want pending (not yet repaired); this relay's Publish for that row failed, so it cannot have marked it published itself -- %q means another relay sharing this outbox published it into a queue this test cannot read (see this test's doc comment)",
			row.ID, status, status)
	}

	// Drain the queue completely rather than sampling one page: the relay
	// published every foreign row ahead of this test's row before the forced
	// failure rolled the batch back (at-least-once by design -- the sends
	// happened, only the status commit did not), so those foreign signals are
	// sitting in this fake queue and a single page proves nothing about
	// absence.
	if sawSignal := drainQueue(t, d, row.ID); sawSignal {
		t.Fatalf("a signal for outbox row %s reached the queue despite the simulated drop", row.ID)
	}

	// Lift the chaos: a later relay run (e.g. the next scheduler tick)
	// must repair the gap on its own, with no special-cased "retry the
	// dropped one" logic anywhere in this test.
	f.clearDropSendFor()

	if err := relay.Run(ctx); err != nil {
		t.Fatalf("Run after lifting chaos: %v", err)
	}

	if status := outboxStatus(t, s, row.ID); status != "published" {
		t.Fatalf("outbox row %s status = %q after repair, want published", row.ID, status)
	}

	// The repaired signal is still this exact row.ID -- foreign deliveries are
	// acked and ignored on the way, never counted as the repair. What is
	// relaxed relative to the pre-#126 assertion is only *where in the stream*
	// the signal is allowed to appear, which was never a property of the relay
	// in the first place.
	if !awaitDelivery(t, d, row.ID, 10*time.Second) {
		t.Fatalf("no signal for outbox row %s arrived on this test's queue after the repair run, want the relay to have repaired the dropped publication; the row reads %q (if it reads published, another relay sharing this outbox published it into its own queue -- see this test's doc comment)",
			row.ID, outboxStatus(t, s, row.ID))
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

// receivePageSize is the batch size the drop chaos test's drains ask for. It
// is named rather than inlined because the #126 failure was precisely a
// hidden dependency on it: the old test demanded its own signal be inside one
// page of this size, which is a claim about how much *other* traffic shares
// the outbox, not a claim about the relay.
const receivePageSize = 10

// drainQueue receives from d until a page comes back empty, acking every
// delivery, and reports whether any of them carried workID. It is the
// negative-assertion counterpart to awaitDelivery: proving a signal is
// *absent* means emptying the queue, because foreign signals can sit in front
// of it (see TestChaosDroppedSendRepairedByOutboxRelay's doc comment).
func drainQueue(t *testing.T, d *queuesqs.Driver, workID string) bool {
	t.Helper()
	ctx := context.Background()

	found := false
	for {
		deliveries, err := d.Receive(ctx, receivePageSize, 0)
		if err != nil {
			t.Fatalf("Receive while draining: %v", err)
		}
		if len(deliveries) == 0 {
			return found
		}
		for _, del := range deliveries {
			if del.WorkID == workID {
				found = true
			}
			if err := d.Ack(ctx, del); err != nil {
				t.Fatalf("Ack %s while draining: %v", del.WorkID, err)
			}
		}
	}
}

// awaitDelivery drains d until a signal for workID arrives, acking and
// ignoring every foreign delivery on the way, and reports whether it found
// one before timeout. Ignoring foreign deliveries does not weaken the
// caller's assertion -- this exact workID is still required to arrive -- it
// only stops the test from insisting on a position in the stream that the
// relay never promised (see TestChaosDroppedSendRepairedByOutboxRelay's doc
// comment, and issue #126).
//
// An empty page normally ends the loop early: every Relay.Run this test makes
// has already returned by the time a caller drains, and Relay publishes
// synchronously, so nothing further can appear. timeout is the outer bound
// that keeps that reasoning from turning into a hang if it ever stops
// holding.
func awaitDelivery(t *testing.T, d *queuesqs.Driver, workID string, timeout time.Duration) bool {
	t.Helper()
	ctx := context.Background()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		deliveries, err := d.Receive(ctx, receivePageSize, 250*time.Millisecond)
		if err != nil {
			t.Fatalf("Receive while awaiting a signal for %s: %v", workID, err)
		}
		if len(deliveries) == 0 {
			return false
		}
		found := false
		for _, del := range deliveries {
			if del.WorkID == workID {
				found = true
			}
			if err := d.Ack(ctx, del); err != nil {
				t.Fatalf("Ack %s while awaiting a signal for %s: %v", del.WorkID, workID, err)
			}
		}
		if found {
			return true
		}
	}
	return false
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
