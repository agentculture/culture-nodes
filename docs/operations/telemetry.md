# Telemetry: turning the export on, querying it, and what a trace does not say

Task t13 (close-the-backlog), issue #5.

`internal/telemetry` instruments three seams and has since the phase-2
cycle: the engine's §12.5 completion transaction
(`engine.transition_commit`), the worker's actor dispatch
(`worker.dispatch`), and the actor callback ingest (`actors.callback`).
Every exporter it builds is gated on one variable. What was missing was a
collector to export to. This page is the deployment half.

## The one variable

`OTEL_EXPORTER_OTLP_ENDPOINT` decides everything:

- **Unset or empty** — `telemetry.New` returns `NoOp()`. No exporter is
  constructed, no background goroutine starts, nothing dials out, nothing is
  logged. This is the default in every profile and the state every
  deployment was in before t13.
- **Set** — the process builds a real OTLP/gRPC trace exporter and metric
  exporter and sends to that address. A `http://` scheme means plaintext;
  `https://` means TLS. Nothing else changes.

Pointing a deployment at a different collector is that value and nothing
else — no image rebuild, no manifest edit, no code change. The three roles
that carry an instrumented seam all read it: `api`, `worker`, `scheduler`
on thor and in the local profile, and `worker` on orin (whose dispatches are
half the spans a trace across the production pair contains).

### Local

```bash
cd deploy/compose
cp .env.example .env      # already carries the two lines below, commented
COMPOSE_PROFILES=bundled-postgres,telemetry \
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 \
  docker compose up -d
```

### Production (thor)

Add one line to `~/.culture-nodes/prod.env`:

```text
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
```

and start the collector alongside the stack:

```bash
cd <repo>/deploy/prod
COMPOSE_PROFILES=telemetry docker compose --env-file ~/.culture-nodes/prod.env \
  -f compose.thor.yml up -d
```

orin's worker points at thor's collector over the same LAN its database
connection already crosses — `OTEL_EXPORTER_OTLP_ENDPOINT=http://thor:4317`
in orin's `prod.env`. Leaving it unset there is a supported state: thor
exports, orin does not, and the missing spans are missing rather than wrong.

Turning telemetry off again is deleting that line. The `telemetry` profile
means the collector is not started by a plain `docker compose up` either, so
the feature is off twice over unless someone asks for it.

## The collector

`deploy/otel-collector.yaml` is one file, mounted read-only by both the
local and the production profile. It accepts OTLP/gRPC on `:4317` and
exports OTLP/JSON to its own stdout, which makes
`docker compose logs otel-collector` the query surface — no volume to
provision, no ownership to arrange for the image's non-root user, nothing on
disk to rotate.

It is the DEFAULT sink, not the only one. A deployment that wants durable,
searchable traces points the variable at Jaeger, Tempo, or a vendor endpoint
and leaves this service down.

Both the `traces` and the `metrics` pipeline are load-bearing:
`telemetry.New` builds an OTLP metric exporter alongside the trace one, and
against a collector with only a traces pipeline every control-plane
process's `Shutdown` fails with `unknown service
opentelemetry.proto.collector.metrics.v1.MetricsService`. That is a measured
failure, not a hypothetical; `tests/deploy/otelcollector_test.go` asserts
both pipelines exist.

## Querying it

```bash
docker compose logs --no-log-prefix otel-collector \
  | grep '^{"resourceSpans"' \
  | jq -c '.resourceSpans[].scopeSpans[].spans[]
      | {trace: .traceId, name: .name,
         run: ([.attributes[]? | select(.key == "run_id") | .value.stringValue] | first)}'
```

`deploy/compose/otel-smoke.sh` is that query wrapped in the three-phase
proof #5 asks for: telemetry on (all three seams arrive), telemetry off
(the same query returns nothing), telemetry pointed elsewhere (a collector
started by a plain `docker run` receives them instead, and the bundled one
receives nothing).

A span may take up to ~30s to arrive after the run that produced it
finished: the SDK's batch span processor flushes on its own schedule and the
collector batches again on top. A query fired the instant a run completes is
asking too early, which looks exactly like an export that did not happen.

## One run, three trace ids — read this before believing a trace

The three seams do not share a trace id, and no configuration of the
collector changes that. Measured on spark, run `01M03BTCFR28PEBCBC17RMD3Y2`,
`deploy/compose/otel-smoke.sh` phase 1:

```text
50270e7b3634157573ef4f272466e914  actors.callback
50270e7b3634157573ef4f272466e914  engine.transition_commit
69fecc22489b3fe3a884ea17d665229e  worker.dispatch
b8cb2aa04caa8ce422d14ed647ad9d7e  actors.callback
```

All four spans carry `run_id=01M03BTCFR28PEBCBC17RMD3Y2`. They fall into
three traces because they were produced in two processes with no trace
context passed between them:

- `worker.dispatch` runs in the **worker** process. On the asynchronous path
  it ends when the attempt is parked (`waiting_external`) — before any
  result exists.
- `actors.callback` runs in the **api** process, started by an inbound HTTP
  request from the actor. `engine.transition_commit` is its child, which is
  why those two DO share a trace.
- Each callback event (`accepted`, then `completed`) is its own inbound
  request, so each is its own root span.

This control plane sets no W3C trace-context propagator and sends no
`traceparent`, so nothing carries the worker's span context to the actor,
and the actor's callback carries nothing back. Closing that gap is an
instrumentation change, not a deployment one, and it is an **all-backends**
change: the worker would have to inject `traceparent` into the invocation,
every bridge (`codex`, `claude-code`, `colleague`, `notify`, `human-inbox`)
would have to echo it on its callbacks, and the api would have to extract
it. Until then:

**The join key is `run_id`, not the trace id.** The attribute allowlist
carries it on every seam deliberately. Any query that groups by trace id
will see one run as three unrelated traces and conclude, wrongly, that the
dispatch and the callback are unrelated events.

## What each seam's spans carry

Only what `internal/telemetry`'s `AllowedAttributeKeys` permits: ids (run,
node, attempt, actor), enum states and outcomes, counts and durations. No
run input, no instruction text, no ledger record content — `filterAllowed`
drops anything else structurally, even an attribute built by hand. A span's
status is `ok` or `error` and never carries an error's message text.
