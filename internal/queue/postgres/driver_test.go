package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/queue"
	queuepg "github.com/agentculture/culture-nodes/internal/queue/postgres"
	"github.com/agentculture/culture-nodes/internal/store"
	"github.com/agentculture/culture-nodes/internal/store/postgres/pgtest"
)

func requireDriver(t *testing.T, namespaceID string) *queuepg.Driver {
	t.Helper()
	s := pgtest.RequireStore(t, testStore)
	return queuepg.New(s.Pool(), namespaceID)
}

// TestPublishReceiveAckDelayRoundTrip proves the basic four-method
// round-trip: a published WorkRef is receivable, and once acked is gone.
func TestPublishReceiveAckDelayRoundTrip(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()

	ns := pgtest.MustNamespace(t, s, "test-queue-roundtrip")
	d := requireDriver(t, ns.ID)
	ref := queue.WorkRef{
		WorkID:      "wrk_" + store.NewULID(),
		NodeRunID:   "nr_" + store.NewULID(),
		NamespaceID: ns.ID,
	}

	if err := d.Publish(ctx, ref); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	deliveries, err := d.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	del := findDelivery(t, deliveries, ref.WorkID)
	if del.NodeRunID != ref.NodeRunID || del.NamespaceID != ref.NamespaceID {
		t.Fatalf("Receive returned %+v, want a delivery matching %+v", del, ref)
	}
	if del.Receipt == "" {
		t.Fatal("Receive returned a delivery with an empty Receipt")
	}

	if err := d.Ack(ctx, del); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	deliveries, err = d.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive after Ack: %v", err)
	}
	if hasDelivery(deliveries, ref.WorkID) {
		t.Fatalf("Receive after Ack still returned %s, want it gone", ref.WorkID)
	}
}

// TestAckIsIdempotent proves acking an already-acked delivery is a harmless
// no-op, not an error -- required because delivery is at-least-once and a
// caller (or two racing callers) may ack the same WorkRef twice.
func TestAckIsIdempotent(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()

	ns := pgtest.MustNamespace(t, s, "test-queue-ack-idempotent")
	d := requireDriver(t, ns.ID)
	ref := queue.WorkRef{WorkID: "wrk_" + store.NewULID(), NamespaceID: ns.ID}
	mustPublish(t, d, ctx, ref)

	del := queue.Delivery{WorkRef: ref, Receipt: ref.WorkID}
	if err := d.Ack(ctx, del); err != nil {
		t.Fatalf("first Ack: %v", err)
	}
	if err := d.Ack(ctx, del); err != nil {
		t.Fatalf("second Ack (already acked): %v, want no error", err)
	}

	// Acking a WorkID that was never published is also harmless.
	never := queue.Delivery{WorkRef: queue.WorkRef{WorkID: "wrk_" + store.NewULID(), NamespaceID: ns.ID}, Receipt: "wrk_" + store.NewULID()}
	if err := d.Ack(ctx, never); err != nil {
		t.Fatalf("Ack of a never-published receipt: %v, want no error", err)
	}
}

// TestDelayThenReceiveAgain proves Delay pushes a signal's availability
// into the future (Receive stops returning it immediately) and Receive
// picks it back up once the delay elapses -- and that calling Receive again
// in between (a duplicate/early poll) is harmless, per the queue's
// at-least-once, duplicates-are-harmless contract.
func TestDelayThenReceiveAgain(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()

	ns := pgtest.MustNamespace(t, s, "test-queue-delay")
	d := requireDriver(t, ns.ID)
	ref := queue.WorkRef{WorkID: "wrk_" + store.NewULID(), NamespaceID: ns.ID}
	mustPublish(t, d, ctx, ref)

	deliveries, err := d.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	del := findDelivery(t, deliveries, ref.WorkID)

	if err := d.Delay(ctx, del, 200*time.Millisecond); err != nil {
		t.Fatalf("Delay: %v", err)
	}

	// Immediately after Delay, a no-wait Receive must not return it.
	deliveries, err = d.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive immediately after Delay: %v", err)
	}
	if hasDelivery(deliveries, ref.WorkID) {
		t.Fatalf("Receive returned %s immediately after Delay, want it withheld", ref.WorkID)
	}

	// Delaying it again (an early/duplicate Delay call) is also harmless.
	if err := d.Delay(ctx, del, 50*time.Millisecond); err != nil {
		t.Fatalf("second Delay: %v, want no error", err)
	}

	// A bounded wait picks it back up once the delay elapses, without
	// busy-looping (Receive uses a ticker internally).
	deliveries, err = d.Receive(ctx, 10, 2*time.Second)
	if err != nil {
		t.Fatalf("Receive with wait: %v", err)
	}
	if !hasDelivery(deliveries, ref.WorkID) {
		t.Fatalf("Receive with wait did not return %s after its delay elapsed", ref.WorkID)
	}
}

// TestPublishIsIdempotentByWorkID proves re-publishing the same WorkID
// (the crash-retry case internal/events.Relay depends on) does not create a
// duplicate signal.
func TestPublishIsIdempotentByWorkID(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()

	ns := pgtest.MustNamespace(t, s, "test-queue-publish-idempotent")
	d := requireDriver(t, ns.ID)
	ref := queue.WorkRef{WorkID: "wrk_" + store.NewULID(), NamespaceID: ns.ID}

	if err := d.Publish(ctx, ref); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	if err := d.Publish(ctx, ref); err != nil {
		t.Fatalf("second Publish (same WorkID): %v, want no error", err)
	}

	deliveries, err := d.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	count := 0
	for _, del := range deliveries {
		if del.WorkID == ref.WorkID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("Receive returned %d deliveries for %s after a duplicate Publish, want exactly 1", count, ref.WorkID)
	}
}

// TestReceiveNoWaitReturnsEmptyWithoutBlocking proves Receive with wait=0
// returns immediately (empty, nil error) when nothing is ready, rather than
// blocking -- the "check once, do not wait" contract documented on
// queue.Queue.
func TestReceiveNoWaitReturnsEmptyWithoutBlocking(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ns := pgtest.MustNamespace(t, s, "test-queue-empty")
	d := requireDriver(t, ns.ID)
	ctx := context.Background()

	start := time.Now()
	deliveries, err := d.Receive(ctx, 10, 0)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(deliveries) != 0 {
		// Another test in this package may have left ready signals behind
		// only if it failed to ack/delay; that's a signal of a bug
		// elsewhere, not this test's concern, so just log it.
		t.Logf("Receive returned %d unrelated deliveries", len(deliveries))
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Receive with wait=0 took %v, want it to return promptly", elapsed)
	}
}

// TestReceiveIsScopedToNamespace proves unrelated ready rows cannot fill a
// receive batch and hide work belonging to this driver. A namespace-agnostic
// LIMIT query returns only the ten older foreign rows and fails this test.
func TestReceiveIsScopedToNamespace(t *testing.T) {
	s := pgtest.RequireStore(t, testStore)
	ctx := context.Background()
	own := pgtest.MustNamespace(t, s, "test-queue-scoped-own")
	foreign := pgtest.MustNamespace(t, s, "test-queue-scoped-foreign")
	d := requireDriver(t, own.ID)

	for i := 0; i < 10; i++ {
		mustPublish(t, d, ctx, queue.WorkRef{
			WorkID: "wrk_" + store.NewULID(), NamespaceID: foreign.ID,
		})
	}
	want := queue.WorkRef{WorkID: "wrk_" + store.NewULID(), NamespaceID: own.ID}
	mustPublish(t, d, ctx, want)

	deliveries, err := d.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].WorkID != want.WorkID {
		t.Fatalf("Receive returned %+v, want only %s from namespace %s", deliveries, want.WorkID, own.ID)
	}
}

func mustPublish(t *testing.T, d *queuepg.Driver, ctx context.Context, ref queue.WorkRef) {
	t.Helper()
	if err := d.Publish(ctx, ref); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

func findDelivery(t *testing.T, deliveries []queue.Delivery, workID string) queue.Delivery {
	t.Helper()
	for _, del := range deliveries {
		if del.WorkID == workID {
			return del
		}
	}
	t.Fatalf("no delivery for WorkID %s among %d deliveries", workID, len(deliveries))
	return queue.Delivery{}
}

func hasDelivery(deliveries []queue.Delivery, workID string) bool {
	for _, del := range deliveries {
		if del.WorkID == workID {
			return true
		}
	}
	return false
}
