//go:build awslive

// This file is excluded from every default build, exactly like
// internal/runners/lambda/awslive_test.go and for the same reason: it
// exercises the driver against real SQS by hand, without pretending a CI
// lane for it exists. The fake and chaos suites in this package remain the
// default-run coverage; this file proves the same driver holds against the
// real service's latencies, visibility mechanics, and authentication.
//
//	NODES_TEST_SQS_QUEUE_URL=https://sqs.us-east-1.amazonaws.com/…/culture-nodes-awslive \
//	AWS_PROFILE=culture-nodes AWS_REGION=us-east-1 \
//	go test -tags awslive ./internal/queue/sqs/ -run TestLive -v
//
// It costs real SQS requests against real credentials from the ambient
// chain. The queue should be dedicated to this lane (deploy/aws/README.md's
// "live test lane" section) — the tests assume nothing else is consuming it,
// and stray messages from an aborted earlier run are tolerated but drained.
package sqs_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/agentculture/culture-nodes/internal/queue"
	queuesqs "github.com/agentculture/culture-nodes/internal/queue/sqs"
)

// liveDriver reads the environment, skipping rather than failing when the
// lane is not configured — an unconfigured environment is not a test
// failure.
func liveDriver(t *testing.T) *queuesqs.Driver {
	t.Helper()
	queueURL := os.Getenv("NODES_TEST_SQS_QUEUE_URL")
	if queueURL == "" {
		t.Skip("set NODES_TEST_SQS_QUEUE_URL to run the live SQS test")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}

	d, err := queuesqs.New(t.Context(), queuesqs.Config{
		QueueURL: queueURL,
		Region:   region,
		// A short visibility timeout keeps an aborted run's stray
		// deliveries from blocking the next run for the default 30s.
		VisibilityTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("sqs.New against real AWS: %v", err)
	}
	return d
}

// drain consumes and acks whatever is immediately available, so a test run
// starts from an empty queue even after an aborted predecessor. It never
// fails the test: leftovers are an at-least-once reality, not an error.
func drain(t *testing.T, d *queuesqs.Driver) {
	t.Helper()
	ctx := context.Background()
	for {
		got, err := d.Receive(ctx, 10, 0)
		if err != nil || len(got) == 0 {
			return
		}
		for _, del := range got {
			_ = d.Ack(ctx, del)
		}
	}
}

// TestLivePublishReceiveAckRoundTrip is the check the fake cannot make: the
// driver's serialisation, request shaping, and credential resolution hold
// against the real service, and an acked delivery stays gone.
func TestLivePublishReceiveAckRoundTrip(t *testing.T) {
	d := liveDriver(t)
	ctx := context.Background()
	drain(t, d)

	const n = 3
	stamp := time.Now().UTC().Format("20060102T150405Z")
	want := map[string]bool{}
	for i := 0; i < n; i++ {
		ref := queue.WorkRef{
			WorkID:      fmt.Sprintf("wrk_live_%s_%02d", stamp, i),
			NodeRunID:   fmt.Sprintf("nr_live_%02d", i),
			NamespaceID: "ns_awslive",
		}
		if err := d.Publish(ctx, ref); err != nil {
			t.Fatalf("Publish #%d: %v", i, err)
		}
		want[ref.WorkID] = true
	}

	got := map[string]queue.Delivery{}
	deadline := time.Now().Add(30 * time.Second)
	for len(got) < n && time.Now().Before(deadline) {
		deliveries, err := d.Receive(ctx, 10, 5*time.Second)
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		for _, del := range deliveries {
			if !want[del.WorkID] {
				// A stray from some other run: retire it and move on.
				_ = d.Ack(ctx, del)
				continue
			}
			if del.NamespaceID != "ns_awslive" {
				t.Errorf("delivery %s carries namespace %q, want ns_awslive", del.WorkID, del.NamespaceID)
			}
			got[del.WorkID] = del
		}
	}
	if len(got) != n {
		t.Fatalf("received %d of %d published refs before the deadline", len(got), n)
	}

	for _, del := range got {
		if err := d.Ack(ctx, del); err != nil {
			t.Fatalf("Ack %s: %v", del.WorkID, err)
		}
	}

	// An acked signal must not come back once its visibility timeout would
	// have elapsed. One post-visibility poll answers that without waiting
	// out redelivery statistics.
	time.Sleep(11 * time.Second)
	deliveries, err := d.Receive(ctx, 10, 2*time.Second)
	if err != nil {
		t.Fatalf("post-ack Receive: %v", err)
	}
	for _, del := range deliveries {
		if want[del.WorkID] {
			t.Errorf("acked signal %s was redelivered", del.WorkID)
		}
		_ = d.Ack(ctx, del)
	}
}

// TestLiveDelayWithholdsRedelivery proves Delay against the real service:
// a delayed delivery stays invisible for roughly the requested duration,
// then comes back.
func TestLiveDelayWithholdsRedelivery(t *testing.T) {
	d := liveDriver(t)
	ctx := context.Background()
	drain(t, d)

	ref := queue.WorkRef{
		WorkID:      "wrk_live_delay_" + time.Now().UTC().Format("20060102T150405Z"),
		NamespaceID: "ns_awslive",
	}
	if err := d.Publish(ctx, ref); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	first := receiveOne(t, d, ref.WorkID, 15*time.Second)
	if err := d.Delay(ctx, first, 8*time.Second); err != nil {
		t.Fatalf("Delay: %v", err)
	}

	// Immediately after the delay it must not be receivable.
	quick, err := d.Receive(ctx, 10, 2*time.Second)
	if err != nil {
		t.Fatalf("Receive during delay: %v", err)
	}
	for _, del := range quick {
		if del.WorkID == ref.WorkID {
			t.Fatalf("delayed signal %s was delivered during its delay", ref.WorkID)
		}
		_ = d.Ack(ctx, del)
	}

	// After the delay elapses it must come back; then retire it.
	second := receiveOne(t, d, ref.WorkID, 30*time.Second)
	if err := d.Ack(ctx, second); err != nil {
		t.Fatalf("Ack after redelivery: %v", err)
	}
}

// receiveOne polls until the named WorkID arrives or the deadline passes,
// acking and discarding anything else it sees along the way.
func receiveOne(t *testing.T, d *queuesqs.Driver, workID string, within time.Duration) queue.Delivery {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		deliveries, err := d.Receive(ctx, 10, 5*time.Second)
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		for _, del := range deliveries {
			if del.WorkID == workID {
				return del
			}
			_ = d.Ack(ctx, del)
		}
	}
	t.Fatalf("signal %s did not arrive within %v", workID, within)
	return queue.Delivery{}
}
