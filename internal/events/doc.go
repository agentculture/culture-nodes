// Package events defines the CloudEvents-1.0-compatible envelope emitted
// for every durable state change (docs/initial-design/culture-nodes-prd-spec.md
// §15.1), plus the outbox relay that is the sole publisher of those events
// and of internal/queue work signals.
//
// # Envelope
//
// Envelope carries the CloudEvents core context attributes this system
// needs: ID, Source, SpecVersion, Type, Subject, Time, DataContentType, and
// Data. Type is always a "dev.culture.nodes.*" string; the Type* constants
// in this package are the documented set from §15.1, not a closed
// enumeration -- later tasks add more as new state changes need audit
// events.
//
// # Event data carries IDs and safe metadata only
//
// This is a hard rule, not a style preference: Data must never carry
// workflow input, node output, ledger record content, secrets, or anything
// else large or sensitive. Large or sensitive content is referenced (an
// artifact:// URI, a ledger record id) and fetched separately by whoever
// needs it, never copied into the event itself. Events are broadcast wide
// (the outbox relay, an eventual SSE stream, integrations) and are
// expected to be cheap to produce and safe to log; a producer that starts
// putting real content in Data has turned an audit trail into a second,
// unversioned copy of authoritative state.
//
// # The outbox relay
//
// Relay (relay.go) is the only process that ever reads unpublished outbox
// rows and turns them into published events and queue signals -- see
// Relay's doc comment for the crash-safety and idempotency guarantees that
// follow from that.
package events
