package queue

import (
	"context"
	"time"
)

// WorkRef references work that might be ready to claim. It is a reference
// only: WorkID, NodeRunID, and NamespaceID, never workflow input, node
// output, or any other payload. See the package doc for why that matters --
// a driver that starts carrying content stops being a disposable signal
// channel and starts being a second source of truth.
type WorkRef struct {
	// WorkID identifies the signal itself (for the Postgres driver, the
	// publishing outbox row's id -- see internal/events/relay.go). It is
	// opaque to the queue: drivers must not assume it is any particular
	// table's primary key.
	WorkID string
	// NodeRunID is the node run this signal is about, if any. It may be
	// empty for outbox rows that are events without an associated node run
	// (e.g. run.completed) -- publishing a signal for those is harmless
	// because receiving it never grants work (see the package doc).
	NodeRunID string
	// NamespaceID scopes the signal to a namespace/tenant.
	NamespaceID string
}

// Delivery is one received WorkRef plus whatever a driver needs to Ack or
// Delay it later. Receipt is opaque and driver-specific: callers must treat
// it as an unstructured handle, never parse or construct one themselves.
type Delivery struct {
	WorkRef
	Receipt string
}

// Queue is the four-method work-signal interface every driver implements:
// publish a work reference, receive up to max of them (long-polling for up
// to wait when none are immediately ready), acknowledge one, or delay one.
//
// Every method must be safe to call with a signal that has already been
// acked, delayed, or redelivered -- duplicate and out-of-order calls are
// expected, at-least-once delivery, not an error condition (see the package
// doc). Ack and Delay are expected to be idempotent: acking or delaying a
// signal that is already gone (already acked, or never existed) is a
// no-op, not an error.
type Queue interface {
	// Publish makes ref available to receive. Implementations should make
	// Publish idempotent by WorkID where practical, so a caller that
	// re-publishes the same WorkID after a crash (see
	// internal/events.Relay) does not create a duplicate signal.
	Publish(ctx context.Context, ref WorkRef) error

	// Receive returns up to max ready deliveries. If none are immediately
	// ready, it polls (never busy-loops) until one arrives or wait elapses,
	// then returns whatever it has -- possibly an empty, nil-error slice.
	// A negative or zero max is treated as 1; a negative or zero wait means
	// "check once, do not wait."
	Receive(ctx context.Context, max int, wait time.Duration) ([]Delivery, error)

	// Ack removes d from future delivery. It must not touch any
	// authoritative workflow state (work_items, node_runs, ...) -- Ack
	// only retires the signal, never the work it referenced.
	Ack(ctx context.Context, d Delivery) error

	// Delay makes d unavailable to receive again until roughly d elapses.
	// A negative delay is treated as zero (immediately available again).
	Delay(ctx context.Context, d Delivery, delay time.Duration) error
}
