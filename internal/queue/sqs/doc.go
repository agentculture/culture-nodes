// Package sqs implements internal/queue.Queue over Amazon SQS (Standard
// queues), the "SQS in a later task" alternative internal/queue's package
// doc names alongside the Postgres driver
// (docs/initial-design/culture-nodes-prd-spec.md §12.3).
//
// This package is one of the sanctioned AWS SDK import sites in this repo
// (alongside internal/artifacts/s3 and, later, internal/runners/lambda --
// see task t17's depguard-style lint). Every other package must reach AWS
// only indirectly, through drivers like this one.
//
// # A delivery grants nothing
//
// This is the same load-bearing rule internal/queue's package doc states for
// every driver, restated here because it is the reason this file's chaos
// tests are allowed to be as aggressive as they are: Receive returns
// WorkRefs, full stop. It never claims, leases, or fences anything, and a
// caller must always follow it with a fenced claim against PostgreSQL (task
// t7's Store.ClaimWork) before treating the referenced work as theirs. SQS
// Standard queues do not promise exactly-once, ordered, or even
// single delivery -- a message can be delivered more than once, out of
// publish order, or (rarely) not at all -- and that is fine here precisely
// *because* nothing downstream of Receive trusts the delivery itself:
//
//   - a duplicated message's second (or third, or Nth) fenced claim finds
//     nothing left to claim and is refused, not double-applied;
//   - a message delivered out of order carries no ordering promise for the
//     fenced claim to violate -- the claim compares against PostgreSQL's
//     current state, never against delivery order;
//   - a message SQS never delivers at all (a genuinely lost SendMessage, or
//     one this driver's Publish failed to send) is repaired the same way
//     any other lost publication is: internal/events.Relay's outbox row
//     stays 'pending' until a hand-off actually succeeds, so the next
//     Relay.Run republishes it.
//
// # Message body: a versioned, opaque reference only
//
// Publish's message body is compact JSON, {"v":1,"work_id":...,
// "node_run_id":...,"namespace_id":...} -- the same three WorkRef fields
// every driver in this package's parent carries, nothing more (see
// internal/queue's "signals are references, never payloads" rule). The "v"
// field is a schema version, not a feature flag: Receive skips (with a
// diagnostic, never an error or a panic) any message whose "v" this version
// of the driver does not recognize, so a future incompatible body shape
// rolls out safely -- an old worker fleet degrades to "ignore messages I
// don't understand" rather than crashing on them. A skipped message is
// deliberately never deleted: it is left for SQS's own redrive/DLQ
// configuration (a deployment concern, out of this driver's scope) rather
// than silently discarded here.
//
// # The MessageDeduplicationId attribute is informational only
//
// Publish also attaches a "MessageDeduplicationId" *message attribute*
// (not the API's MessageDeduplicationId *request parameter*, which is a
// FIFO-queue-only field a Standard queue's SendMessage call rejects). It
// exists purely so an operator inspecting a message in the SQS console or
// via GetQueueAttributes can see which WorkID produced it; Standard queues
// perform no deduplication on it or anything else. Deduplication of the
// underlying work is PostgreSQL's job (the fenced claim above), never this
// attribute's.
//
// # Chaos tests map to the §20.4 recovery matrix
//
// chaos_test.go's four tests are each written to prove a specific property,
// three of them named rows of prd-spec §20.4's recovery matrix:
//
//   - TestChaosDuplicateDeliveryClaimedExactlyOnce proves "SQS signal is
//     duplicated | PostgreSQL claim permits one current owner": 20 real
//     work_items rows, 20 published WorkRefs, forced duplicate delivery
//     driving total deliveries above 20 -- every work item still completes
//     exactly once, and at least one delivery's fenced claim comes back
//     empty (a refused duplicate), proving the refusal actually happened
//     rather than merely not being contradicted.
//   - TestChaosReorderedDeliveryAllEventuallyProcessed proves the same
//     underlying invariant from the other direction: §20.4 has no row named
//     "reorder" because the design never promises ordered delivery in the
//     first place (prd-spec §12.3: "a message received out of order cannot
//     overwrite newer state"). This test publishes N refs through a fake
//     with a reorder buffer, asserts delivery order differs from publish
//     order (so the chaos knob is proven to have fired), and asserts every
//     ref is still eventually received exactly once in the harmless sense
//     (no crash, no lost ref) -- ordering was never a property to violate.
//   - TestChaosDroppedSendRepairedByOutboxRelay proves "SQS publication is
//     missed | Outbox republishes": forced SendMessage failures leave outbox
//     rows 'pending' (internal/events.Relay's documented at-least-once
//     behavior -- see relay.go's doc comment), and a later Relay.Run with
//     the chaos lifted republishes them, after which Receive sees them.
//   - TestChaosUnknownSchemaVersionSkippedDiagnosticNotCrash proves the
//     forward-compatibility rule above directly: one message with an
//     unrecognized "v" sits among otherwise-normal messages; Receive skips
//     it (with a diagnostic through Config.Logf), returns the rest, and
//     never errors or panics.
//
// # Test backend: a fake, not LocalStack
//
// fake_test.go implements the four AWS JSON 1.0 RPC operations this driver
// calls (SendMessage, ReceiveMessage, DeleteMessage,
// ChangeMessageVisibility) as an httptest.Server, with the chaos knobs the
// tests above configure directly (duplicate probability/forced duplication,
// a bounded reorder window, forced send failures). It exists so this
// package's test suite needs neither real AWS credentials nor a LocalStack
// container: chaos here is an explicit, deterministic knob a test sets,
// not a race it hopes to provoke against a real (or emulated) network
// service. Driver.Publish/Receive/Ack/Delay talk to it exactly as they
// would talk to real SQS -- Config.Endpoint is the only thing that differs
// between a test run and production.
package sqs
