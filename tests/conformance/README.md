# tests/conformance

A runnable acceptance suite for PRD §13 actor adapters. Point it at your
endpoint and it tells you whether what you built is an actor Culture Nodes can
drive.

```bash
go test ./tests/conformance -args -endpoint=https://my-actor.example
```

Without `-endpoint` the suite skips, so it costs nothing in a normal
`go test ./...`. What always runs is the reference check: an in-process actor
that implements §13 correctly, with the whole kit run against it — so the kit
is itself under test, and there is a worked example of every check a few lines
away in `reference.go`.

## Flags

| Flag | Meaning |
| --- | --- |
| `-endpoint` | Actor base URL. Required; without it the suite skips. |
| `-auth-token` | Scoped workload token. Empty skips the authentication check. |
| `-input` | JSON the actor should accept and answer synchronously. |
| `-async-input` | JSON the actor should answer with a §13.3 acceptance. Empty skips every asynchronous check. |
| `-bad-input` | JSON the actor must reject. Empty skips the contract-failure check. |
| `-node-id`, `-contract-digest` | Fill §13.1's `node` block. |
| `-workflow-name`, `-workflow-digest` | Fill §13.1's `workflow` block. |
| `-callback-base-url` | Externally reachable base URL for the kit's callback receiver. Set it when the actor cannot reach loopback. |
| `-timeout`, `-callback-wait` | Per-invocation and terminal-callback deadlines. |
| `-expect-callback-retry` | Require the actor to redeliver a refused terminal callback with the same event id. |
| `-require-cancellation` | Fail rather than skip when the actor does not declare `supports_cancellation`. |

## What it checks

| Check | PRD | Why it matters |
| --- | --- | --- |
| Authentication is required | §13.1 | An actor that serves an unauthenticated invocation executes work for anyone who finds it. |
| Same `Idempotency-Key`, same result | §20.3 | Keeps at-least-once delivery from turning a retried hop into duplicated side effects. |
| A 200 is a result body with an outcome | §13.2 | An actor that answers 200 with no outcome has not answered the question the node asked. |
| A 202 carries an `invocation_id` | §13.3 | Without one there is nothing to cancel and nothing to correlate. |
| Callbacks have stable ids and an increasing sequence | §13.4 | Stable ids make a redelivery recognizable; the sequence makes a reordering recognizable. |
| A non-terminal event precedes the terminal one | §13.3 | A declared `heartbeat_after_seconds` is a promise about liveness. |
| Redelivery is idempotent | §13.4 | A repeated event id must be the same event, not a different one wearing its name. |
| Cancellation is reachable when declared | §13.6 | Best-effort: the endpoint must answer, not stop the work. |
| A rejected input is non-retryable | §13.5 | Stops a contract failure being retried forever. |

## What it does not check

Anything about what the actor *does*. The kit sends an input and the actor may
answer any outcome its contract declares. Business results are the node
contract's job, not the protocol's.

## Reaching the callback receiver

The kit runs a receiver in-process and advertises it in §13.1's callback
block. For a remote actor that address must be reachable — pass
`-callback-base-url` with whatever does reach it (a tunnel, a LAN address, a
port-forward). An author who cannot arrange that still gets every synchronous
check; the asynchronous ones skip with an explanation rather than failing.
