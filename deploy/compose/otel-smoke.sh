#!/usr/bin/env bash
# deploy/compose/otel-smoke.sh — the live telemetry proof for issue #5
# (task t13), the sibling of smoke.sh.
#
# smoke.sh proves the engine runs inside these containers. This script
# proves what the deployment EXPORTS while it does, and it proves it three
# times over, because #5's acceptance is three separate claims:
#
#   ON        with OTEL_EXPORTER_OTLP_ENDPOINT pointing at the bundled
#             collector, one real run's three instrumented seams
#             (worker.dispatch, actors.callback, engine.transition_commit)
#             arrive at that collector, carrying that run's id.
#   OFF       with the variable UNSET and nothing else changed, the same
#             run drives the same code and the same query returns NOTHING.
#             This is the control: it is what makes the ON result evidence
#             about the export rather than about the collector being noisy.
#   ELSEWHERE with the variable pointed at a DIFFERENT collector — one
#             started by a plain `docker run`, that this compose file knows
#             nothing about — the spans arrive there instead. Pointing the
#             deployment at any collector is that one variable's value.
#
# Everything runs locally: `docker compose --profile telemetry --profile
# smoke-actor`, one workflow, one run, one query per phase. Always tears
# down on exit, success or failure.
#
# What this script does NOT claim, and why: see the "one run, three trace
# ids" section of docs/operations/telemetry.md. The three seams live in two
# processes and this control plane propagates no W3C trace context across
# the actor boundary, so the spans share a RUN ID and not a trace id. The
# query below therefore joins on run_id — the join key the attribute
# allowlist deliberately carries — and prints the trace ids it found rather
# than asserting there is one.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

PROJECT="${COMPOSE_PROJECT_NAME:-t13otel}"
ENV_FILE="${COMPOSE_ENV_FILE:-.env.example}"
PROFILES="bundled-postgres,telemetry,smoke-actor"
API_PORT="${NODES_API_PORT:-8080}"
BASE_URL="http://localhost:${API_PORT}"
FIXTURE="testdata/smoke.workflow.yaml"
ACTOR_KEY="company/smoke-test"
REGISTRATION_SECRET="compose-dev-only-registration-secret"
# The throwaway collector phase 3 points at: started by `docker run`,
# joined to the compose network under a name the compose file never
# mentions.
AWAY_COLLECTOR="${PROJECT}-elsewhere-collector"

# The three seams internal/telemetry instruments, by span name. A seam
# renamed there and not here makes this script fail loudly rather than
# quietly proving two seams out of three.
SEAMS=(worker.dispatch actors.callback engine.transition_commit)

compose() {
	COMPOSE_PROFILES="$PROFILES" \
		OTEL_EXPORTER_OTLP_ENDPOINT="${OTEL_ENDPOINT:-}" \
		NODES_NAMESPACE_ID="${NAMESPACE_ID:-default}" \
		docker compose -p "$PROJECT" --env-file "$ENV_FILE" "$@"
}

log() { printf '\n>>> %s\n' "$*"; }
fail() { printf '\n!!! %s\n' "$*" >&2; exit 1; }

cleanup() {
	local status=$?
	log "tearing down (docker compose -p ${PROJECT} down -v)"
	if [ "$status" -ne 0 ]; then
		log "otel smoke failed -- capturing service logs before teardown"
		compose logs --no-color --tail 60 || true
	fi
	compose down -v --remove-orphans || true
	docker rm -f "$AWAY_COLLECTOR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- the query ------------------------------------------------------------

# spans_of <container> prints one compact JSON object per exported span:
# {trace, span, parent, name, run}. The collector's `file` exporter writes
# OTLP/JSON to its own stdout (see deploy/otel-collector.yaml), so the
# container's log stream IS the trace store and `jq` reads it directly.
spans_of() {
	docker logs "$1" 2>&1 |
		sed 's/^[^{]*//' |
		grep '^{"resourceSpans"' |
		jq -c '.resourceSpans[].scopeSpans[].spans[]
			| {trace: .traceId, span: .spanId, parent: .parentSpanId, name: .name,
			   run: ([.attributes[]? | select(.key == "run_id") | .value.stringValue] | first)}' ||
		true
}

# seams_for <container> <run-id> prints the distinct seam span names that
# collector holds for that run, one per line.
seams_for() {
	spans_of "$1" | jq -r --arg run "$2" 'select(.run == $run) | .name' | sort -u
}

# EXPORT_GRACE_SECONDS is how long a span may take to reach the collector
# after the run that produced it finished. The SDK's batch span processor
# flushes on its own schedule (5s by default) and the collector batches
# again on top of that, so a query fired the instant a run completes is
# asking too early -- which looks exactly like an export that did not
# happen. Both the ON assertion (poll until all three arrive) and the OFF
# assertion (wait the full grace, then require nothing) use it, so the
# control waits at least as long as the positive case ever does.
EXPORT_GRACE_SECONDS="${EXPORT_GRACE_SECONDS:-30}"

require_all_seams() {
	local container=$1 run=$2 found missing
	for _ in $(seq 1 "$EXPORT_GRACE_SECONDS"); do
		found=$(seams_for "$container" "$run")
		missing=""
		for seam in "${SEAMS[@]}"; do
			echo "$found" | grep -qx "$seam" || missing="$seam"
		done
		[ -z "$missing" ] && break
		sleep 1
	done
	echo "$found" | sed 's/^/    seam: /'
	[ -z "$missing" ] ||
		fail "collector ${container} holds no ${missing} span for run ${run} after ${EXPORT_GRACE_SECONDS}s"
}

# --- driving one real run -------------------------------------------------

api_up() {
	local ready=""
	for _ in $(seq 1 60); do
		if code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/v1alpha1/readyz" 2>/dev/null) &&
			[ "$code" = "200" ]; then
			ready=1
			break
		fi
		sleep 2
	done
	[ -n "$ready" ] || fail "API never answered readyz within 120s"
}

# namespace_id reads the id `nodes serve` minted on first boot. The worker
# must serve THAT namespace or it claims nothing -- the same step
# deploy/prod/deploy.sh performs after thor's first boot.
namespace_id() {
	compose exec -T postgres psql -U nodes -d nodes -Atc \
		'SELECT id FROM namespaces ORDER BY created_at LIMIT 1' | tr -d '\r'
}

register_actor() {
	local status
	status=$(curl -s -o /tmp/otel-smoke-actor.json -w '%{http_code}' \
		-X POST "${BASE_URL}/v1alpha1/actors" \
		-H 'content-type: application/json' \
		-H "authorization: Bearer ${REGISTRATION_SECRET}" \
		-d "$(jq -n --arg key "$ACTOR_KEY" \
			'{actor_key: $key, kind: "agent", protocol: "http", endpoint_ref: "http://smoke-actor:8099"}')")
	case "$status" in
	200 | 201) echo "registered ${ACTOR_KEY} -> http://smoke-actor:8099" ;;
	*) fail "registering ${ACTOR_KEY} returned HTTP ${status}: $(cat /tmp/otel-smoke-actor.json)" ;;
	esac
}

# drive_run <subject> publishes the fixture workflow, creates a run, waits
# for it to reach a terminal state, and echoes the run id. A run that does
# NOT complete has not exercised all three seams, so this refuses to
# continue on one that stalls.
drive_run() {
	local subject=$1 source_json validate_body digest run_payload run_id state
	source_json=$(jq -Rs '.' "$FIXTURE")
	validate_body=$(jq -n --argjson source "$source_json" '{format: "yaml", source: $source}')
	digest=$(curl -s -H 'content-type: application/json' -d "$validate_body" \
		"${BASE_URL}/v1alpha1/workflows" | jq -r '.digest')
	[ -n "$digest" ] && [ "$digest" != "null" ] || fail "publishing ${FIXTURE} returned no digest"

	run_payload=$(curl -s -H 'content-type: application/json' \
		-d "$(jq -n --arg d "$digest" --arg s "$subject" '{workflow_digest: $d, input: {subject: $s}}')" \
		"${BASE_URL}/v1alpha1/runs")
	run_id=$(jq -r '.id' <<<"$run_payload")
	[ -n "$run_id" ] && [ "$run_id" != "null" ] || fail "creating a run returned no id: $run_payload"

	for _ in $(seq 1 30); do
		state=$(curl -s "${BASE_URL}/v1alpha1/runs/${run_id}" | jq -r '.run.state')
		case "$state" in
		completed | failed | cancelled) break ;;
		esac
		sleep 1
	done
	[ "$state" = "completed" ] ||
		fail "run ${run_id} reached state '${state}', not completed: the actor dispatch and callback seams may not have fired"
	echo "$run_id"
}

# bring_up starts (or restarts) the stack with OTEL_ENDPOINT as configured,
# then makes sure the worker serves the real namespace.
bring_up() {
	compose up -d --no-build >/dev/null
	api_up
	NAMESPACE_ID=$(namespace_id)
	[ -n "$NAMESPACE_ID" ] || fail "could not read the namespace id from postgres"
	compose up -d --no-build --force-recreate worker >/dev/null
}

# --- phase 1: ON ----------------------------------------------------------

log "phase 1/3 — ON: OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317"
OTEL_ENDPOINT="http://otel-collector:4317"
bring_up
register_actor
RUN_ON=$(drive_run "t13 telemetry on")
echo "run ${RUN_ON} completed; querying the bundled collector"
require_all_seams "${PROJECT}-otel-collector-1" "$RUN_ON"
echo "trace ids carrying that run's spans:"
spans_of "${PROJECT}-otel-collector-1" | jq -r --arg run "$RUN_ON" \
	'select(.run == $run) | "    \(.trace)  \(.name)"' | sort

# --- phase 2: OFF ---------------------------------------------------------
#
# The control. Same code, same stack, same fixture, same query -- the ONLY
# difference is that the variable is empty, which is what
# internal/telemetry.New reads to decide whether to build an exporter at
# all. The stack is recreated from scratch so no span from phase 1 can be
# mistaken for a phase 2 result.

log "phase 2/3 — OFF: OTEL_EXPORTER_OTLP_ENDPOINT unset"
compose down -v --remove-orphans >/dev/null
OTEL_ENDPOINT=""
bring_up
register_actor
RUN_OFF=$(drive_run "t13 telemetry off")
echo "run ${RUN_OFF} completed; waiting ${EXPORT_GRACE_SECONDS}s — the full grace the ON phase gets — then querying the same collector the same way"
sleep "$EXPORT_GRACE_SECONDS"
OFF_SPANS=$(spans_of "${PROJECT}-otel-collector-1" | wc -l | tr -d ' ')
[ "$OFF_SPANS" = "0" ] ||
	fail "the collector holds ${OFF_SPANS} span(s) with the exporter variable unset; the ON result would prove nothing"
echo "    0 spans exported, for any run, with the variable unset"

# --- phase 3: ELSEWHERE ---------------------------------------------------
#
# A collector this compose file has never heard of: started by `docker run`,
# joined to the same network, named by nothing but the variable's value.

log "phase 3/3 — ELSEWHERE: the same deployment, pointed at a throwaway collector"
compose down -v --remove-orphans >/dev/null
docker run -d --rm --name "$AWAY_COLLECTOR" \
	--network "${PROJECT}_default" \
	-v "$(cd .. && pwd)/otel-collector.yaml:/etc/otelcol-contrib/config.yaml:ro" \
	otel/opentelemetry-collector-contrib:latest >/dev/null 2>&1 ||
	true # the network does not exist until the stack is up; retried below

OTEL_ENDPOINT="http://${AWAY_COLLECTOR}:4317"
bring_up
if ! docker inspect "$AWAY_COLLECTOR" >/dev/null 2>&1; then
	docker run -d --rm --name "$AWAY_COLLECTOR" \
		--network "${PROJECT}_default" \
		-v "$(cd .. && pwd)/otel-collector.yaml:/etc/otelcol-contrib/config.yaml:ro" \
		otel/opentelemetry-collector-contrib:latest >/dev/null
	# The control plane's exporter dials lazily, so a collector that starts
	# after the stack still receives everything from the first run.
	sleep 2
fi
register_actor
RUN_AWAY=$(drive_run "t13 telemetry elsewhere")
echo "run ${RUN_AWAY} completed; querying the THROWAWAY collector"
require_all_seams "$AWAY_COLLECTOR" "$RUN_AWAY"
BUNDLED_SPANS=$(spans_of "${PROJECT}-otel-collector-1" | wc -l | tr -d ' ')
[ "$BUNDLED_SPANS" = "0" ] ||
	fail "the bundled collector also received ${BUNDLED_SPANS} span(s); the variable is not what chose the destination"
echo "    and 0 spans at the bundled collector: the variable's value chose the destination"

log "OTEL SMOKE PASSED"
cat <<EOF
run with telemetry on:        ${RUN_ON}   (all three seams at the bundled collector)
run with telemetry off:       ${RUN_OFF}  (0 spans anywhere)
run pointed elsewhere:        ${RUN_AWAY} (all three seams at a collector compose never declared)
EOF
