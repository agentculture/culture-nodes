package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/agentculture/culture-nodes/internal/events"
	"github.com/agentculture/culture-nodes/internal/queue"
	queuepg "github.com/agentculture/culture-nodes/internal/queue/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

// fakeQueue is an in-memory queue.Queue that records every Publish call (in
// order, including calls that are about to fail) and can run an optional
// hook to inject a failure partway through a batch -- used to
// deterministically simulate a relay crash without real timing races.
type fakeQueue struct {
	mu        sync.Mutex
	calls     []queue.WorkRef
	onPublish func(ref queue.WorkRef) error
}

func (f *fakeQueue) Publish(_ context.Context, ref queue.WorkRef) error {
	f.mu.Lock()
	f.calls = append(f.calls, ref)
	f.mu.Unlock()
	if f.onPublish != nil {
		return f.onPublish(ref)
	}
	return nil
}

func (f *fakeQueue) Receive(context.Context, int, time.Duration) ([]queue.Delivery, error) {
	return nil, nil
}
func (f *fakeQueue) Ack(context.Context, queue.Delivery) error { return nil }
func (f *fakeQueue) Delay(context.Context, queue.Delivery, time.Duration) error {
	return nil
}

func (f *fakeQueue) publishedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, len(f.calls))
	for i, c := range f.calls {
		ids[i] = c.WorkID
	}
	return ids
}

var _ queue.Queue = (*fakeQueue)(nil)

// recordingSink is an events.EventSink recording every envelope handed to
// it, in order, so tests can assert on delivery counts and ID stability.
type recordingSink struct {
	mu     sync.Mutex
	events []events.Envelope
}

func (s *recordingSink) Handle(_ context.Context, env events.Envelope) error {
	s.mu.Lock()
	s.events = append(s.events, env)
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, len(s.events))
	for i, e := range s.events {
		ids[i] = e.ID
	}
	return ids
}

// TestRelayPublishesEachPendingRowExactlyOnceWithStableIDs proves the
// non-crash path: every pending outbox row is handed to both the queue and
// the sink exactly once, and the event ID the sink sees equals the WorkID
// the queue sees equals the outbox row's own id.
func TestRelayPublishesEachPendingRowExactlyOnceWithStableIDs(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "test-relay-basic")

	var inserted []postgres.OutboxRecord
	for i := 0; i < 3; i++ {
		row, err := s.InsertOutbox(ctx, postgres.InsertOutboxInput{
			NamespaceID: ns.ID,
			Topic:       events.TypeRunCreated,
			Payload:     json.RawMessage(`{"node_run_id":"nr_test_basic"}`),
		})
		if err != nil {
			t.Fatalf("InsertOutbox #%d: %v", i, err)
		}
		inserted = append(inserted, row)
	}

	q := &fakeQueue{}
	sink := &recordingSink{}
	relay := events.NewRelay(s.Pool(), q, sink.Handle, events.RelayOptions{BatchSize: 2, Source: "nodes-test"})

	if err := relay.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, row := range inserted {
		status, publishedAt := outboxRowState(t, s, row.ID)
		if status != "published" {
			t.Fatalf("outbox row %s status = %q, want published", row.ID, status)
		}
		if publishedAt == nil {
			t.Fatalf("outbox row %s published_at is nil after Run", row.ID)
		}
	}

	queueCounts := countByID(q.publishedIDs())
	sinkCounts := countByID(sink.ids())
	for _, row := range inserted {
		if queueCounts[row.ID] != 1 {
			t.Fatalf("queue received %d WorkRefs for outbox row %s, want exactly 1", queueCounts[row.ID], row.ID)
		}
		if sinkCounts[row.ID] != 1 {
			t.Fatalf("sink received %d events for outbox row %s, want exactly 1", sinkCounts[row.ID], row.ID)
		}
	}
}

// TestRelayCrashRecoveryPublishesAtLeastOnceAndMarksExactlyOnce simulates a
// relay crashing partway through draining the outbox (cancelling its
// context, mirroring a killed process) and re-running: every row must end
// up published exactly once in the outbox's own bookkeeping, even though
// the row whose batch was interrupted was handed to the queue/sink more
// than once -- at-least-once delivery with a stable, reused event ID is
// the documented, expected outcome (see Relay's doc comment), not a bug to
// paper over.
func TestRelayCrashRecoveryPublishesAtLeastOnceAndMarksExactlyOnce(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	bg := context.Background()
	ns := pgtest.MustNamespace(t, s, "test-relay-crash")

	const n = 5
	var inserted []postgres.OutboxRecord
	for i := 0; i < n; i++ {
		row, err := s.InsertOutbox(bg, postgres.InsertOutboxInput{
			NamespaceID: ns.ID,
			Topic:       events.TypeRunCreated,
			Payload:     json.RawMessage(`{"node_run_id":"nr_test_crash"}`),
		})
		if err != nil {
			t.Fatalf("InsertOutbox #%d: %v", i, err)
		}
		inserted = append(inserted, row)
	}

	ctx, cancel := context.WithCancel(bg)
	q := &fakeQueue{}
	sink := &recordingSink{}
	errSimulatedCrash := errors.New("simulated crash")

	calls := 0
	q.onPublish = func(queue.WorkRef) error {
		calls++
		if calls == 3 {
			// Simulate the process dying right after the 3rd row's signal
			// was already handed off (recorded above) but before the
			// relay could finish that row's batch and commit it as
			// published -- the exact window Relay's doc comment names as
			// producing at-least-once, not exactly-once, delivery.
			cancel()
			return errSimulatedCrash
		}
		return nil
	}

	// batchSize=1 so each outbox row is its own transaction/commit unit,
	// making which row was "in flight" at the simulated crash deterministic.
	relay := events.NewRelay(s.Pool(), q, sink.Handle, events.RelayOptions{BatchSize: 1, Source: "nodes-test"})

	err := relay.Run(ctx)
	if err == nil {
		t.Fatal("Run after a simulated crash returned nil, want an error")
	}
	if !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("Run error = %v, want it to wrap the simulated crash error", err)
	}

	publishedAfterCrash := 0
	for _, row := range inserted {
		status, _ := outboxRowState(t, s, row.ID)
		if status == "published" {
			publishedAfterCrash++
		}
	}
	if publishedAfterCrash == 0 || publishedAfterCrash >= n {
		t.Fatalf("published %d/%d rows immediately after the simulated crash, want a partial amount (some, not all, not none)", publishedAfterCrash, n)
	}

	// Recover: fresh context, no more induced failures. The relay must
	// finish draining everything the crash left behind.
	q.onPublish = nil
	if err := relay.Run(context.Background()); err != nil {
		t.Fatalf("Run after recovery: %v", err)
	}

	for _, row := range inserted {
		status, publishedAt := outboxRowState(t, s, row.ID)
		if status != "published" {
			t.Fatalf("outbox row %s status = %q after recovery, want published", row.ID, status)
		}
		if publishedAt == nil {
			t.Fatalf("outbox row %s published_at is nil after recovery", row.ID)
		}
	}

	// At-least-once: at least one row (the one whose batch the simulated
	// crash interrupted) was hand off to the queue more than once.
	queueCounts := countByID(q.publishedIDs())
	duplicated := false
	for _, row := range inserted {
		if queueCounts[row.ID] == 0 {
			t.Fatalf("queue never received a WorkRef for outbox row %s", row.ID)
		}
		if queueCounts[row.ID] > 1 {
			duplicated = true
		}
	}
	if !duplicated {
		t.Fatal("no outbox row was handed to the queue more than once across the crash/recovery, want the interrupted row to show at-least-once duplication")
	}

	// Stable IDs: every occurrence recorded by the sink -- including
	// duplicates -- carries the outbox row's own id, never a freshly
	// minted one.
	sinkCounts := countByID(sink.ids())
	for _, row := range inserted {
		if sinkCounts[row.ID] == 0 {
			t.Fatalf("sink never received an event with the stable id %s", row.ID)
		}
	}
}

// TestRelayRepairsDroppedPublication proves the outbox alone is not a
// publisher: inserting a row produces no queue signal until the relay
// actually runs, and once it does, the signal becomes receivable through
// the real Postgres queue driver (not a fake) -- an end-to-end proof of
// the "lost publication can be repaired from the outbox" property
// (prd-spec §12.3).
func TestRelayRepairsDroppedPublication(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()
	ns := pgtest.MustNamespace(t, s, "test-relay-repair")

	row, err := s.InsertOutbox(ctx, postgres.InsertOutboxInput{
		NamespaceID: ns.ID,
		Topic:       events.TypeNodeRunReady,
		Payload:     json.RawMessage(`{"node_run_id":"nr_test_repair"}`),
	})
	if err != nil {
		t.Fatalf("InsertOutbox: %v", err)
	}

	driver := queuepg.New(s.Pool())

	deliveries, err := driver.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive before relay: %v", err)
	}
	if hasDeliveryFor(deliveries, row.ID) {
		t.Fatalf("Receive saw a signal for outbox row %s before the relay ever ran", row.ID)
	}

	sink := &recordingSink{}
	relay := events.NewRelay(s.Pool(), driver, sink.Handle, events.RelayOptions{Source: "nodes-test"})
	if err := relay.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	deliveries, err = driver.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive after relay: %v", err)
	}
	if !hasDeliveryFor(deliveries, row.ID) {
		t.Fatalf("Receive after relay did not see a signal for outbox row %s, want the relay to have repaired the dropped publication", row.ID)
	}
}

func hasDeliveryFor(deliveries []queue.Delivery, workID string) bool {
	for _, d := range deliveries {
		if d.WorkID == workID {
			return true
		}
	}
	return false
}

func countByID(ids []string) map[string]int {
	counts := make(map[string]int, len(ids))
	for _, id := range ids {
		counts[id]++
	}
	return counts
}

// outboxRowState reads status and published_at directly from the outbox
// table, bypassing any typed Store method, so these tests observe exactly
// what the relay committed rather than trusting a wrapper around it.
func outboxRowState(t *testing.T, s *postgres.Store, id string) (status string, publishedAt *time.Time) {
	t.Helper()
	var st string
	var pub pgtype.Timestamptz
	err := s.Pool().QueryRow(context.Background(),
		`SELECT status, published_at FROM outbox WHERE id = $1`, id,
	).Scan(&st, &pub)
	if err != nil {
		t.Fatalf("query outbox row %s: %v", id, err)
	}
	if pub.Valid {
		v := pub.Time
		return st, &v
	}
	return st, nil
}
