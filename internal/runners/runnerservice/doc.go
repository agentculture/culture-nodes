// Package runnerservice is the reference api/runner-protocol runner service:
// it puts any runners.Runner behind the protocol's HTTP surface, with
// mandatory bearer authentication, durable per-operation status, a bounded
// worker pool, best-effort cancellation, and the optional resultless
// completion callback.
//
// The deployment cmd/nodes-runner builds wraps internal/runners/headspace,
// which is the runner PRD §13.7 actually describes. Nothing in this package
// knows that: the wrapped thing is a runners.Runner, and the protocol surface
// is identical whether it runs containers or nothing at all. That is what
// makes tests/runnerconformance able to run the same kit against the fake and
// against real Docker.
//
// # What is decided synchronously, and why it matters
//
// The 202 is a door that closes. Once it is sent there is no HTTP error
// channel left, so every refusal this service can decide is decided before
// the acceptance is issued: the credential, the declared protocol version,
// the payload size, the operation document against
// schemas/runner/operation.schema.json, an Idempotency-Key that disagrees
// with the body, a re-dispatch of the same id with a different document
// (409), and the worker queue's capacity (429). Only what the wrapped runner
// itself decides — an unregistered image digest, a requires_shell command, a
// policy field it cannot enforce — is discovered after acceptance, and
// result.go documents how that is reported honestly.
//
// # The durability choice, stated plainly
//
// The contract this service is built against
// (api/runner-protocol/README.md) says two things about status:
//
//	Hold per-operation status durably enough to answer the status endpoint
//	for at least the retention declared in the acceptance.
//
//	Never let an operation's status *disappear* before that retention
//	elapses.
//
// It does not use the word "restart". It does not have to: a process restart
// is the ordinary way a status disappears, and the protocol minimum retention
// is one hour — far longer than the interval between deploys, evictions and
// OOM kills in any real deployment. So this package reads that bar as
// including a restart, and the deployment store (NewFileStore) writes each
// record to its own file, fsynced and renamed into place, before the
// in-memory index is updated.
//
// NewMemoryStore exists too, and reports Durable() == false rather than
// pretending. cmd/nodes-runner refuses to start without either a state
// directory or an explicit --ephemeral-state flag, mirroring
// ServiceIdentity.AllowInsecureTransport: the weaker guarantee stays
// reachable, but only as a deliberate, greppable act.
//
// ## The limits of that choice, also stated plainly
//
//   - Durability is the state directory's durability. Pointed at a tmpfs or a
//     container's ephemeral filesystem, this store keeps its promise for
//     exactly as long as that filesystem lives. The service cannot detect
//     that and does not claim to.
//   - An operation that was in flight when the process died is NOT recovered
//     and resumed. It cannot be: the execution lived in that process. On
//     startup such a record is transitioned to a terminal `failed` with
//     error.kind `runner_unavailable`, and every observation on the result
//     declares measured:false, complete:false with a note saying the outcome
//     was never observed.
//   - That recovery result does not claim the work did not happen. A
//     container started by a process that then died can keep running: this
//     was observed during development, when a SIGKILL to the service left a
//     `headspace run` subprocess and its container alive. The note says side
//     effects may still have occurred, and the timing's finished_at is
//     labelled as the instant the loss was recorded, not an instant anything
//     was measured.
//   - Retention expiry is enforced on read as well as by a background sweep,
//     so a status is never served after the window this service promised.
//   - A callback offer is held in memory only and is lost on a restart. It is
//     caller-issued bearer material, and the protocol makes callbacks
//     best-effort and unnecessary — polling learns every outcome — so losing
//     one costs latency and nothing else.
//
// # Shutdown
//
// Close cancels every in-flight operation rather than waiting it out, so each
// one gets an honest terminal status (`cancelled`, produced by the runner
// that actually stopped it) before the process exits, and operations that
// were queued but never started are recorded as cancelled with nothing
// claimed. A shutdown that waited out a ten-minute container is a shutdown an
// orchestrator SIGKILLs through — which is precisely the crash the recovery
// path above then has to describe.
//
// # Open decisions this package records rather than absorbs
//
//   - Whether the protocol should REQUIRE refusals to be synchronous. The
//     contract's error table describes a 400/422 at dispatch, and a wrapped
//     runner that only refuses after acceptance is not covered by it. This
//     service reports such a refusal as a terminal `rejected` status and
//     tests/runnerconformance accepts either shape; if the protocol later
//     mandates the synchronous form, a runner will need a preflight seam
//     (a "can you refuse this without running it?" call) that
//     runners.Runner does not have today.
//   - Whether a multi-principal deployment needs 403. This service holds one
//     secret, so a missing, malformed, or wrong credential is uniformly 401;
//     403 is reserved for a deployment where a valid credential can be
//     unauthorised for a particular operation.
package runnerservice
