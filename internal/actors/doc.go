// Package actors is the PRD §13 actor protocol: the client that invokes an
// actor over HTTP, the attempt-scoped tokens that authorize its callbacks,
// and the ingest path that turns those callbacks into committed engine state.
//
// # What an actor is here
//
// An actor is a registered identity and an endpoint (§9.5). It may be an
// agent, a service, an isolated code runner, or a human group. This package
// never learns which: it speaks one HTTP protocol to all of them, and the
// only thing it knows about the thing on the other end is the endpoint URL,
// the credential, and what the protocol says.
//
// That is a hard boundary, not a stylistic preference. §9.5 states it
// directly: "the core engine does not branch on provider names. Provider and
// model details are telemetry metadata reported by the adapter." No code in
// this package, in internal/worker, in internal/engine, in internal/compiler,
// or in internal/api may name a model vendor or an agent product; the guard
// is a test (neutrality_test.go) that greps those trees and fails on a hit.
// The binary links no vendor SDK either, which is why the protocol here is
// plain net/http over JSON.
//
// # The three surfaces
//
//   - Client.Invoke (§13.1–13.3) POSTs an invocation and reads back either a
//     synchronous result (200) or an asynchronous acceptance (202). Every
//     failure is classified into one of §13.5's nine error classes, and only
//     the classes §13.5 calls retryable are retried.
//   - TokenSigner (token.go) mints and verifies the short-lived,
//     attempt-scoped token §13.1's callback block carries. It is an HMAC over
//     the attempt id and an expiry, verified in constant time — no third-party
//     token library, because the whole payload is two fields this package
//     already holds.
//   - HandleCallback (callback.go) is the ingest for §13.4's events. It
//     verifies the token, deduplicates by (attempt, event id), enforces the
//     per-attempt sequence, and — for a terminal event — resumes the durable
//     invocation and commits through engine.CompleteAttempt under the
//     attempt's original fencing token.
//
// # Why the callback path is so careful
//
// §13.4's last line is the whole design constraint: "completion after
// cancellation or attempt replacement is recorded as a late diagnostic event
// but cannot commit workflow state." An actor that went quiet for an hour and
// then reports success has no way to know a deadline fired and a newer
// attempt already ran. So the terminal path never trusts the callback's own
// claim about which attempt it is: it re-leases the durable work item under
// the fencing token recorded at dispatch, and a mismatch there — meaning
// something newer has since claimed the work — turns the completion into a
// diagnostic event and nothing else.
//
// Duplicates get the same treatment for the same reason. At-least-once
// delivery is assumed (§20.1), so the ingest is idempotent by event id and
// monotonic by sequence, and a repeat of either is recorded and dropped
// rather than applied twice.
package actors
