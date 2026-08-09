package postgres_test

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

// TestInsertEventMonotonicSequence proves events.sequence increments 1, 2,
// 3, ... for a single aggregate under sequential inserts.
func TestInsertEventMonotonicSequence(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-events-seq")
	aggregateID := "run_" + store.NewULID()

	for want := int64(1); want <= 5; want++ {
		ev, err := s.InsertEvent(ctx, postgres.InsertEventInput{
			NamespaceID:   ns.ID,
			AggregateType: "run",
			AggregateID:   aggregateID,
			EventType:     "dev.culture.nodes.run.created",
		})
		if err != nil {
			t.Fatalf("InsertEvent #%d: %v", want, err)
		}
		if ev.Sequence != want {
			t.Fatalf("InsertEvent #%d: Sequence = %d, want %d", want, ev.Sequence, want)
		}
	}
}

// TestInsertEventConcurrentSameAggregateStaysMonotonic fires many
// concurrent InsertEvent calls at the same aggregate and asserts the
// resulting sequence numbers are exactly {1..N}, no duplicates and no
// gaps. This is the property that matters: the unique index on
// (aggregate_id, sequence) plus Store.InsertEvent's retry loop must
// serialize correctly under real contention, not just when called one at a
// time from a single goroutine.
func TestInsertEventConcurrentSameAggregateStaysMonotonic(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-events-concurrent")
	aggregateID := "run_" + store.NewULID()

	const n = 25
	var wg sync.WaitGroup
	sequences := make([]int64, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev, err := s.InsertEvent(ctx, postgres.InsertEventInput{
				NamespaceID:   ns.ID,
				AggregateType: "run",
				AggregateID:   aggregateID,
				EventType:     "dev.culture.nodes.token.transitioned",
			})
			sequences[i] = ev.Sequence
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("InsertEvent (goroutine %d): %v", i, err)
		}
	}

	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	for i, seq := range sequences {
		want := int64(i + 1)
		if seq != want {
			t.Fatalf("concurrent sequences = %v, want exactly 1..%d with no duplicates/gaps (index %d: got %d want %d)",
				sequences, n, i, seq, want)
		}
	}
}

// TestInsertEventUniqueIndexRejectsDuplicateSequence bypasses
// Store.InsertEvent's retry helper and inserts a duplicate (aggregate_id,
// sequence) pair directly, proving the unique index -- not just
// application-level discipline -- is what actually enforces monotonicity.
func TestInsertEventUniqueIndexRejectsDuplicateSequence(t *testing.T) {
	s := requireStore(t)
	ctx := context.Background()

	ns := mustNamespace(t, s, "test-events-dup")
	aggregateID := "run_" + store.NewULID()

	first, err := s.InsertEvent(ctx, postgres.InsertEventInput{
		NamespaceID:   ns.ID,
		AggregateType: "run",
		AggregateID:   aggregateID,
		EventType:     "dev.culture.nodes.run.created",
	})
	if err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	_, err = s.Pool().Exec(ctx,
		`INSERT INTO events (id, namespace_id, aggregate_type, aggregate_id, sequence, event_type)
		 VALUES ($1, $2, 'run', $3, $4, 'dev.culture.nodes.run.created')`,
		store.NewULID(), ns.ID, aggregateID, first.Sequence,
	)
	if err == nil {
		t.Fatal("raw duplicate (aggregate_id, sequence) INSERT succeeded, want the unique index to reject it")
	}
}
