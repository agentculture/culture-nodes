// Package runnerconformance is a runnable acceptance suite for
// api/runner-protocol runner services — the code-execution sibling of
// tests/conformance, which does the same job for PRD §13 actors.
//
// A runner author points it at their endpoint and finds out, in one command,
// whether the thing they built is a runner service Culture Nodes can
// dispatch to:
//
//	go test ./tests/runnerconformance -args \
//	    -endpoint=https://runner.thor.internal:8443 \
//	    -auth-token=... -image-digest=sha256:... -runner-revision=sha256:...
//
// Without -endpoint the suite skips, so it costs nothing in a normal
// `go test ./...` run. What does run unconditionally is service_test.go,
// which stands up the in-repo reference service
// (internal/runners/runnerservice) over a fake runner and runs the whole kit
// against it — so the kit is itself under test, and a runner author who fails
// a check can read a correct implementation of exactly that behaviour. A
// second, skipping test (live_test.go) runs the identical kit against the
// same service wrapping the real headspace-cli bridge, which is the
// deployment the protocol document describes.
//
// # What it checks, and why each check exists
//
//   - Authentication is required, on execute AND on status. A runner service
//     accepting operations over the network is a remote-code-execution
//     surface; an unauthenticated one executes code for anyone who can reach
//     it, and an unauthenticated status read leaks what ran, where, and with
//     what digests. The check also requires that auth is decided BEFORE
//     existence: an unauthenticated read of an unknown operation must answer
//     401/403, never 404, or the endpoint is an operation-id oracle.
//   - Dispatch answers 202 with an acceptance echoing the operation id.
//     Without it there is nothing to poll.
//   - Status is answerable immediately after acceptance. Polling starts
//     before the work does.
//   - Status polls to a terminal state, and `result` is present exactly when
//     the state is terminal. A terminal status with no result is a claim
//     rather than a result; a non-terminal status carrying one is an envelope
//     disagreeing with its own contract.
//   - The terminal result is schema-valid against
//     schemas/runner/result.schema.json and its `state` matches the envelope.
//   - Re-dispatching the same operation_id returns the acceptance already
//     issued and does not start the work again — proven by comparing the
//     terminal result byte-for-byte across a re-dispatch.
//   - An unknown operation is 404 (to an authenticated caller).
//   - A refusable operation is refused rather than executed (opt-in).
//   - Cancellation reaches a terminal state when the runner declared it
//     (opt-in).
//
// # What it deliberately does not check
//
// Anything about what the runner DOES with the operation beyond the terminal
// state the caller says to expect. The kit dispatches the operation the
// caller configured; the executed program's behaviour is the node contract's
// business, not the protocol's.
//
// # The retention check is a declaration check, honestly labelled
//
// The protocol's retention promise is at least one hour
// (runners.MinStatusRetention). A test cannot wait an hour, so the kit checks
// the two things it can: that the declared status_retention_seconds meets the
// protocol minimum, and that the terminal status is still readable and
// byte-identical after a short delay. A runner that declares 24h and forgets
// at minute 59 passes this check. The declaration is taken on trust and the
// kit says so rather than implying it proved more than it did.
package runnerconformance
