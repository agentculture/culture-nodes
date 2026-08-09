// Package conformance is a runnable acceptance suite for PRD §13 actor
// adapters.
//
// An adapter author points it at their endpoint and finds out, in one
// command, whether the thing they built is an actor Culture Nodes can drive:
//
//	go test ./tests/conformance -args -endpoint=https://my-actor.example
//
// Without -endpoint the suite skips, so it costs nothing in a normal `go test
// ./...` run. What does run unconditionally is refactor_test.go, which stands
// up a small in-process reference actor and runs the whole kit against it —
// so the kit is itself under test, and an adapter author who fails a check
// can read a correct implementation of exactly that behaviour a few lines
// away.
//
// # What it checks, and why each check exists
//
//   - Authentication is required. §13.1 sends a scoped workload token; an
//     actor that accepts an unauthenticated invocation is an open endpoint
//     that will execute work for anyone who finds it.
//   - Re-invoking with the same Idempotency-Key returns the same result.
//     §20.3 makes this the property that keeps at-least-once delivery safe;
//     an actor that runs the work twice turns a retried network hop into
//     duplicated side effects.
//   - A 200 is a §13.2 result body with a domain outcome. An actor that
//     answers 200 with no outcome has not answered the question the node
//     asked.
//   - A 202 is a §13.3 acceptance with an invocation id, followed by §13.4
//     callbacks that carry stable event ids and a strictly increasing
//     sequence, and end in a terminal event.
//   - A non-terminal event arrives before the terminal one. §13.3's
//     heartbeat_after_seconds is a promise about liveness; an actor that
//     declares one and then goes silent has broken it.
//   - Redelivering a callback is harmless: a repeated event id carries an
//     identical payload, and an actor that is asked to retry a delivery does
//     so with the same event id rather than inventing a new one.
//   - Cancellation is reachable when the actor declared it supports it
//     (§13.6). It is best-effort — the check requires that the endpoint
//     exists and answers, not that the work actually stops.
//   - A rejected input is classified as a non-retryable failure. This is the
//     check that stops a contract failure being retried forever.
//
// # What it deliberately does not check
//
// Anything about what the actor DOES. The kit sends an input, and the actor
// is free to answer any outcome its contract declares; nothing here asserts a
// particular business result, because that is the node contract's job and not
// the protocol's.
//
// # Reaching the callback receiver
//
// The kit runs a callback receiver in-process and advertises its URL in
// §13.1's callback block. For the in-repo reference actor that is a loopback
// address and works unchanged. For a remote endpoint the actor must be able
// to reach the receiver, so Config.CallbackBaseURL overrides the advertised
// base with whatever address does reach it (a tunnel, a LAN address, a
// port-forward). An adapter author who cannot arrange that can still run
// every synchronous check; the async ones skip with an explanation rather
// than failing.
package conformance
