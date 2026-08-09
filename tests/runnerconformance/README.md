# tests/runnerconformance

A runnable acceptance suite for [api/runner-protocol](../../api/runner-protocol/README.md)
runner services — the code-execution sibling of [tests/conformance](../conformance/README.md),
which does the same job for PRD §13 actors. Point it at your endpoint and it
tells you whether what you built is a runner service Culture Nodes can
dispatch to.

```bash
go test ./tests/runnerconformance -args \
  -endpoint=https://runner.thor.internal:8443 \
  -auth-token="$RUNNER_SECRET" \
  -operation-file=./op.json
```

Without `-endpoint` the suite skips, so it costs nothing in a normal
`go test ./...`. What always runs is the reference check: the in-repo runner
service (`internal/runners/runnerservice`) over a fake runner, with the whole
kit run against it — so the kit is itself under test, and there is a worked
example of every check a few lines away in `service_test.go`. A second test
runs the identical kit against the same service wrapping the real
`headspace-cli` bridge; it skips unless `headspace` and Docker are both
present.

## Flags

| Flag | Meaning |
| --- | --- |
| `-endpoint` | Runner service base URL. Required; without it the suite skips. |
| `-auth-token` | The bearer secret. Required in substance: the protocol has no unauthenticated posture, so an empty value fails rather than skips. |
| `-operation-file` | Path to a JSON runner operation the service should accept and run. Required. |
| `-refused-operation-file` | An operation the service must refuse. Empty skips the policy-boundary check. |
| `-cancellable-operation-file` | A long-running operation the kit dispatches and then cancels. Empty skips the cancel check. |
| `-expect-state` | Terminal state `-operation-file` should reach (default `completed`). |
| `-timeout`, `-terminal-wait`, `-poll-interval`, `-cancel-after` | Timing knobs. |

The operation is a **file**, not a pile of flags, because an operation is a
schema document (`schemas/runner/operation.schema.json`) and a runner is
entitled to refuse one the kit assembled out of defaults it invented.

## What it checks

| Check | Why it matters |
| --- | --- |
| Execute is refused without a credential, and with a wrong one | A runner service accepting operations over the network is a remote-code-execution surface. |
| **Status** is refused too, before existence is checked | Status leaks what ran and with what digests; answering `404` to an unauthenticated read makes the endpoint an operation-id oracle. |
| No response body ever echoes the secret | Checked on every response, because the interesting place for a secret to leak is the error path. |
| Dispatch answers `202` with an acceptance the runtime accepts | Validated with `runners.Acceptance.Validate` — the id must echo, and the declared retention must meet the protocol minimum. |
| Status is answerable immediately after acceptance | Polling starts before the work does. |
| Status polls to a terminal state, with `result` present exactly when terminal | Checked on *every* sample via `runners.OperationStatus.Validate`, not just the last one. |
| The terminal result is schema-valid and agrees with its envelope | An envelope that disagrees with its own evidence is a contract failure. |
| The terminal status is still readable, and unchanged, after a delay | See the honesty note below. |
| Re-dispatching the same `operation_id` returns the same acceptance and does not re-run the work | Proven by a byte-identical terminal result across the re-dispatch. |
| An unknown operation is `404` to an authenticated caller | The runtime reads that as a dispatch error, never a completion. |
| A refusable operation is refused, not executed *(opt-in)* | The policy boundary: `requires_shell`, an unregistered digest, an unenforceable policy. |
| Cancellation reaches a terminal state when the runner declares it *(opt-in)* | Best-effort; a runner declaring `supports_cancellation: false` passes. |
| Completion works with **no** callback configured | The kit never sends `Nodes-Callback-Url`. Polling alone must be sufficient. |

## What it does not check

Anything about what the runner *does* beyond the terminal state you told it to
expect. The kit dispatches the operation you configured; what the executed
program prints or produces is the node contract's business, not the
protocol's.

## The retention check is a declaration check, labelled as one

The protocol's retention promise is at least one hour. A test cannot wait an
hour, so the kit checks the two things it can: that the acceptance's declared
`status_retention_seconds` meets the protocol minimum, and that the terminal
status is still readable and byte-identical after a short delay. **A runner
that declares 24h and forgets at minute 59 passes this check.** The
declaration is taken on trust, and the kit says so rather than implying it
proved more than it did.

## Refusals: synchronous or terminal

The protocol's error table describes a synchronous refusal — `400`/`422` at
dispatch. A runner that only discovers a refusal *after* it has answered `202`
has no HTTP error channel left; the honest answer there is a terminal
`rejected` status whose result declares that nothing was measured. The kit
accepts either shape and requires only that the operation never runs. Whether
the protocol should mandate the synchronous form is
[recorded as an open decision](../../internal/runners/runnerservice/doc.go).
