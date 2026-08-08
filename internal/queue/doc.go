// Package queue defines the narrow, disposable work-signal abstraction
// (docs/initial-design/culture-nodes-prd-spec.md §12.3): publish a work
// reference, receive work references, acknowledge one, or delay one.
//
// # Signals are references, never payloads
//
// A WorkRef carries only WorkID, NodeRunID, and NamespaceID -- opaque
// identifiers, never workflow input, node output, or any other content.
// PostgreSQL (internal/store/postgres) is the authoritative state; a queue
// driver (this package's Postgres driver for local/single-node deployments,
// or the SQS driver added by a later task) exists only to tell a worker
// "something might be ready to claim," never to carry the thing itself.
//
// # Receiving a signal does not grant work
//
// This is the load-bearing rule the whole abstraction exists to enforce.
// Every driver in this package is required to stay "dumb": Receive returns
// WorkRefs, full stop. It never claims, leases, or fences anything. The
// worker (task t7's fenced claim against work_items) is the only thing that
// actually grants work, and it always re-derives eligibility from
// PostgreSQL rather than trusting that a signal arrived. That single design
// choice is what makes every property below hold:
//
//   - duplicated signals are harmless -- the second delivery's fenced claim
//     simply finds nothing left to claim;
//   - a signal received out of order cannot overwrite newer state -- the
//     fenced claim compares against the current state_version/fencing_token,
//     not against anything the signal carried;
//   - a lost publication can always be repaired by replaying the outbox
//     (internal/events.Relay is the only publisher, and it is idempotent by
//     the outbox row's own id -- see internal/events/relay.go);
//   - local/single-node deployments need no external queue product at all
//     (the Postgres driver in internal/queue/postgres stands in for one);
//   - switching queue drivers (Postgres today, SQS in a later task) changes
//     nothing about workflow semantics, because no driver is ever allowed to
//     be more than a notification channel.
package queue
