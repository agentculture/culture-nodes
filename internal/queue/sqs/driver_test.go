package sqs_test

import (
	"context"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/queue"
	queuesqs "github.com/agentculture/culture-nodes/internal/queue/sqs"
)

// TestPublishReceiveAckRoundTrip proves the basic four-method contract
// against the fake, mirroring internal/queue/postgres's own round-trip
// test: a published WorkRef is receivable with a matching body, and once
// acked is gone.
func TestPublishReceiveAckRoundTrip(t *testing.T) {
	f := newFakeSQS(t)
	d := f.driver(t, queuesqs.Config{})
	ctx := context.Background()

	ref := queue.WorkRef{WorkID: "wrk_001", NodeRunID: "nr_001", NamespaceID: "ns_001"}
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

// TestAckIsIdempotent proves acking an already-acked delivery -- or one
// whose receipt was never valid -- is a harmless no-op, matching
// queue.Queue's documented contract and the Postgres driver's own
// TestAckIsIdempotent.
func TestAckIsIdempotent(t *testing.T) {
	f := newFakeSQS(t)
	d := f.driver(t, queuesqs.Config{})
	ctx := context.Background()

	ref := queue.WorkRef{WorkID: "wrk_002", NamespaceID: "ns_001"}
	if err := d.Publish(ctx, ref); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	deliveries, err := d.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	del := findDelivery(t, deliveries, ref.WorkID)

	if err := d.Ack(ctx, del); err != nil {
		t.Fatalf("first Ack: %v", err)
	}
	if err := d.Ack(ctx, del); err != nil {
		t.Fatalf("second Ack (already acked): %v, want no error", err)
	}

	never := queue.Delivery{WorkRef: ref, Receipt: "receipt-never-issued"}
	if err := d.Ack(ctx, never); err != nil {
		t.Fatalf("Ack of a never-issued receipt: %v, want no error", err)
	}
}

// TestDelayThenReceiveAgain proves Delay withholds a delivery from Receive
// until the visibility window elapses, and that an early/duplicate Delay
// call is harmless -- mirroring the Postgres driver's own
// TestDelayThenReceiveAgain.
func TestDelayThenReceiveAgain(t *testing.T) {
	f := newFakeSQS(t)
	d := f.driver(t, queuesqs.Config{})
	ctx := context.Background()

	ref := queue.WorkRef{WorkID: "wrk_003", NamespaceID: "ns_001"}
	if err := d.Publish(ctx, ref); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	deliveries, err := d.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	del := findDelivery(t, deliveries, ref.WorkID)

	// Immediately after receiving (before any Delay), a no-wait Receive
	// must not return it again -- it is still within its visibility
	// timeout.
	deliveries, err = d.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive immediately after first Receive: %v", err)
	}
	if hasDelivery(deliveries, ref.WorkID) {
		t.Fatalf("Receive returned %s while still within its visibility timeout", ref.WorkID)
	}

	if err := d.Delay(ctx, del, 0); err != nil {
		t.Fatalf("Delay(0): %v", err)
	}

	deliveries, err = d.Receive(ctx, 10, 0)
	if err != nil {
		t.Fatalf("Receive after Delay(0): %v", err)
	}
	if !hasDelivery(deliveries, ref.WorkID) {
		t.Fatalf("Receive after Delay(0) did not return %s, want it immediately visible again", ref.WorkID)
	}

	// A second Delay(0) on the fresh delivery is also harmless.
	del2 := findDelivery(t, deliveries, ref.WorkID)
	if err := d.Delay(ctx, del2, 0); err != nil {
		t.Fatalf("second Delay(0): %v, want no error", err)
	}
}

// TestReceiveNoWaitReturnsEmptyWithoutBlocking proves Receive with wait<=0
// returns promptly when nothing is ready, matching the queue.Queue "check
// once, do not wait" contract.
func TestReceiveNoWaitReturnsEmptyWithoutBlocking(t *testing.T) {
	f := newFakeSQS(t)
	d := f.driver(t, queuesqs.Config{})
	ctx := context.Background()

	start := time.Now()
	deliveries, err := d.Receive(ctx, 10, 0)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(deliveries) != 0 {
		t.Fatalf("Receive returned %d deliveries, want 0", len(deliveries))
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Receive with wait=0 took %v, want it to return promptly", elapsed)
	}
}

// TestReceiveWaitReturnsOnceMessageBecomesReady proves Receive with a
// positive wait picks up a message published after Receive started
// polling, without the caller needing to poll manually -- unlike the
// Postgres driver, this relies on SQS's own long poll inside a single
// ReceiveMessage call for short waits.
func TestReceiveWaitReturnsOnceMessageBecomesReady(t *testing.T) {
	f := newFakeSQS(t)
	d := f.driver(t, queuesqs.Config{MaxWait: 2 * time.Second})
	ctx := context.Background()

	ref := queue.WorkRef{WorkID: "wrk_004", NamespaceID: "ns_001"}
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := d.Publish(context.Background(), ref); err != nil {
			t.Errorf("Publish (background): %v", err)
		}
	}()

	deliveries, err := d.Receive(ctx, 10, 3*time.Second)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if !hasDelivery(deliveries, ref.WorkID) {
		t.Fatalf("Receive with wait did not return %s published mid-wait", ref.WorkID)
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
