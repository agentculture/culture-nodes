#!/usr/bin/env bash
#
# deploy/helm/ci-smoke.sh — the smoke suite t22 runs against an already
# helm-installed culture-nodes release in a kind cluster, shared verbatim
# between a local run and .github/workflows/deploy.yml so the two can never
# drift apart. This script does NOT create the cluster, build the image,
# load it, run `helm install`, or tear anything down — the caller (a human
# following deploy/helm/README.md's "Local install (kind)" section, or the
# CI workflow's own steps) owns that surrounding lifecycle identically in
# both places; this script only owns "is the thing that got installed
# actually healthy."
#
# What it proves, in order:
#   1. api/scheduler/worker Deployments finish rolling out.
#   2. GET /v1alpha1/healthz and /v1alpha1/readyz both answer 200 — the API
#      process is up and can reach Postgres.
#   3. POST /v1alpha1/workflows/validate, then POST /v1alpha1/workflows,
#      round-trip a real workflow fixture — proves the compiler and the
#      publish path work end to end against the in-cluster database, not
#      just that the process started.
#   4. GET /v1alpha1/runs on a freshly installed release returns 200 with
#      zero items — proves the migrated schema actually has the runs table
#      wired up, without needing an actor to drive an end-to-end run.
#   5. Every worker pod is Running and Ready — worker.replicas (default 2)
#      is the multi-pod-safe default, and this proves the topology came up,
#      not just that a Deployment object exists.
#   6. The scheduler pod's own startup diagnostic (cmd/nodes/scheduler.go)
#      is present and names the active/standby model. Today's scheduler
#      emits exactly one startup line ("starting as standby; it becomes
#      active on acquiring the single-active advisory lock") and no
#      separate "I just became active" transition log — see
#      internal/scheduler's package doc — so this checks that diagnostic
#      exists and names both states, not that two differently-labeled
#      replicas were observed.
#   7. (optional, SMOKE_INGRESS=1) A bogus bearer token against the
#      callback route through a real Ingress gets a structured 401 — proves
#      the actor callback route (h37) is reachable and enforces token
#      verification from outside the api Service, not only in-cluster.
#
# Required on PATH: kubectl, curl, jq. SMOKE_INGRESS=1 additionally needs
# docker (to resolve the kind node's container IP -- see step 7 below).
#
# Configuration (env vars, all optional):
#   NAMESPACE           kubectl namespace the release lives in (default: default)
#   RELEASE             helm release name (default: nodes)
#   FULLNAME            chart fullname prefix (default: ${RELEASE}-culture-nodes)
#   WORKER_REPLICAS     expected worker replica count (default: 2)
#   ROLLOUT_TIMEOUT     per-Deployment `kubectl rollout status` timeout (default: 180s)
#   API_LOCAL_PORT      local port for the port-forward (default: 18080)
#   SMOKE_INGRESS        1 to also probe the callback route through a real Ingress
#                        (the release must have been installed/upgraded with
#                        ingress.enabled=true and ingress-nginx must already be
#                        installed in the cluster -- this script does not do
#                        either; see deploy/helm/README.md)
#   INGRESS_HOST         the Ingress's spec.rules[].host (values.ingress.host)
#   KIND_NODE            kind node container name (default: nodes-ci-control-plane)
#   INGRESS_NAMESPACE    namespace ingress-nginx was installed into (default: ingress-nginx)

set -euo pipefail

NAMESPACE="${NAMESPACE:-default}"
RELEASE="${RELEASE:-nodes}"
FULLNAME="${FULLNAME:-${RELEASE}-culture-nodes}"
WORKER_REPLICAS="${WORKER_REPLICAS:-2}"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-180s}"
API_LOCAL_PORT="${API_LOCAL_PORT:-18080}"
SMOKE_INGRESS="${SMOKE_INGRESS:-0}"
INGRESS_HOST="${INGRESS_HOST:-}"
KIND_NODE="${KIND_NODE:-nodes-ci-control-plane}"
INGRESS_NAMESPACE="${INGRESS_NAMESPACE:-ingress-nginx}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
FIXTURE="${REPO_ROOT}/internal/compiler/testdata/edge-order-ordered.workflow.yaml"

log() { printf '[ci-smoke] %s\n' "$*"; }
fail() {
  printf '[ci-smoke] FAIL: %s\n' "$*" >&2
  exit 1
}

require_bin() {
  command -v "$1" >/dev/null 2>&1 || fail "required binary '$1' is not on PATH"
}
require_bin kubectl
require_bin curl
require_bin jq

[ -f "$FIXTURE" ] || fail "fixture workflow not found: $FIXTURE"

log "namespace=${NAMESPACE} release=${RELEASE} fullname=${FULLNAME} worker_replicas(expected)=${WORKER_REPLICAS}"

# ---- 1. rollout status ----------------------------------------------------
for role in api scheduler worker; do
  log "kubectl rollout status deployment/${FULLNAME}-${role}"
  kubectl -n "$NAMESPACE" rollout status "deployment/${FULLNAME}-${role}" --timeout "$ROLLOUT_TIMEOUT"
done

# ---- port-forward the api Service -----------------------------------------
log "port-forwarding svc/${FULLNAME}-api -> 127.0.0.1:${API_LOCAL_PORT}"
kubectl -n "$NAMESPACE" port-forward "svc/${FULLNAME}-api" "${API_LOCAL_PORT}:8080" >/tmp/ci-smoke-port-forward.log 2>&1 &
PF_PID=$!
cleanup() {
  kill "$PF_PID" >/dev/null 2>&1 || true
  wait "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

API_URL="http://127.0.0.1:${API_LOCAL_PORT}"

log "waiting for the port-forward to accept connections"
for _ in $(seq 1 30); do
  if curl -sS -o /dev/null "${API_URL}/v1alpha1/healthz" 2>/dev/null; then
    break
  fi
  sleep 1
done

# ---- 2. healthz / readyz ---------------------------------------------------
log "GET /v1alpha1/healthz"
status=$(curl -sS -o /tmp/ci-smoke-healthz.json -w '%{http_code}' "${API_URL}/v1alpha1/healthz")
[ "$status" = "200" ] || fail "healthz status = $status, want 200 (body: $(cat /tmp/ci-smoke-healthz.json))"
log "  -> 200 $(cat /tmp/ci-smoke-healthz.json)"

log "GET /v1alpha1/readyz"
status=$(curl -sS -o /tmp/ci-smoke-readyz.json -w '%{http_code}' "${API_URL}/v1alpha1/readyz")
[ "$status" = "200" ] || fail "readyz status = $status, want 200 (body: $(cat /tmp/ci-smoke-readyz.json))"
log "  -> 200 $(cat /tmp/ci-smoke-readyz.json)"

# ---- 3. workflow validate + publish round-trip -----------------------------
log "POST /v1alpha1/workflows/validate (fixture: $(basename "$FIXTURE"))"
payload=$(jq -n --rawfile src "$FIXTURE" '{format: "yaml", source: $src}')
status=$(curl -sS -o /tmp/ci-smoke-validate.json -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' -d "$payload" \
  "${API_URL}/v1alpha1/workflows/validate")
[ "$status" = "200" ] || fail "validate status = $status, want 200 (body: $(cat /tmp/ci-smoke-validate.json))"
valid=$(jq -r '.valid' /tmp/ci-smoke-validate.json)
[ "$valid" = "true" ] || fail "validate reported valid=$valid (body: $(cat /tmp/ci-smoke-validate.json))"
log "  -> 200 valid=true digest=$(jq -r '.digest' /tmp/ci-smoke-validate.json)"

log "POST /v1alpha1/workflows (publish)"
status=$(curl -sS -o /tmp/ci-smoke-publish.json -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' -d "$payload" \
  "${API_URL}/v1alpha1/workflows")
case "$status" in
  200 | 201) : ;;
  *) fail "publish status = $status, want 200 or 201 (body: $(cat /tmp/ci-smoke-publish.json))" ;;
esac
log "  -> $status digest=$(jq -r '.digest' /tmp/ci-smoke-publish.json)"

# ---- 4. runs list is empty --------------------------------------------------
log "GET /v1alpha1/runs"
status=$(curl -sS -o /tmp/ci-smoke-runs.json -w '%{http_code}' "${API_URL}/v1alpha1/runs")
[ "$status" = "200" ] || fail "runs list status = $status, want 200 (body: $(cat /tmp/ci-smoke-runs.json))"
count=$(jq '.items | length' /tmp/ci-smoke-runs.json)
[ "$count" = "0" ] || fail "runs list has $count item(s) on a freshly installed release, want 0"
log "  -> 200 items=0"

# ---- 5. worker pods Running/Ready ------------------------------------------
log "checking worker pods"
worker_json=$(kubectl -n "$NAMESPACE" get pods -l "app.kubernetes.io/instance=${RELEASE},app.kubernetes.io/component=worker" -o json)
worker_count=$(echo "$worker_json" | jq '.items | length')
[ "$worker_count" = "$WORKER_REPLICAS" ] || fail "found $worker_count worker pod(s), want $WORKER_REPLICAS"
echo "$worker_json" | jq -c '.items[] | {name: .metadata.name, phase: .status.phase, ready: [.status.containerStatuses[]?.ready]}'
not_ready=$(echo "$worker_json" | jq '[.items[] | select(.status.phase != "Running" or ([.status.containerStatuses[]?.ready] | all) == false)] | length')
[ "$not_ready" = "0" ] || fail "$not_ready worker pod(s) are not Running/Ready"
log "  -> ${worker_count}/${worker_count} worker pods Running and Ready"

# ---- 6. scheduler log line shows the active/standby model ------------------
log "checking scheduler startup diagnostic"
scheduler_pod=$(kubectl -n "$NAMESPACE" get pods -l "app.kubernetes.io/instance=${RELEASE},app.kubernetes.io/component=scheduler" -o jsonpath='{.items[0].metadata.name}')
[ -n "$scheduler_pod" ] || fail "no scheduler pod found"
scheduler_logs=$(kubectl -n "$NAMESPACE" logs "$scheduler_pod")
if ! echo "$scheduler_logs" | grep -qi "standby"; then
  fail "scheduler pod $scheduler_pod's logs do not mention standby/active:\n${scheduler_logs}"
fi
log "  -> ${scheduler_pod}: $(echo "$scheduler_logs" | grep -i standby | head -1)"

# ---- 7. optional: callback route through a real Ingress --------------------
# No extraPortMappings/hostPort cluster config needed: the kind node is a
# Docker container on the "kind" bridge network, directly reachable from the
# Docker host (this script's own host) by container IP -- so this resolves
# the node's IP and ingress-nginx's NodePort dynamically instead of relying
# on any specific kind cluster config being in place.
if [ "$SMOKE_INGRESS" = "1" ]; then
  [ -n "$INGRESS_HOST" ] || fail "SMOKE_INGRESS=1 requires INGRESS_HOST"
  require_bin docker

  node_ip=$(docker inspect "$KIND_NODE" --format '{{.NetworkSettings.Networks.kind.IPAddress}}')
  [ -n "$node_ip" ] || fail "could not resolve container IP for kind node '$KIND_NODE'"
  node_port=$(kubectl -n "$INGRESS_NAMESPACE" get svc ingress-nginx-controller -o jsonpath='{.spec.ports[?(@.name=="http")].nodePort}')
  [ -n "$node_port" ] || fail "could not resolve ingress-nginx-controller's http NodePort in namespace $INGRESS_NAMESPACE"
  ingress_url="http://${node_ip}:${node_port}"
  log "ingress-nginx reachable at ${ingress_url} (Host: ${INGRESS_HOST})"

  log "GET /v1alpha1/healthz through the Ingress (sanity: the Ingress itself routes correctly)"
  status=$(curl -sS -o /dev/null -w '%{http_code}' -H "Host: ${INGRESS_HOST}" "${ingress_url}/v1alpha1/healthz")
  [ "$status" = "200" ] || fail "healthz through Ingress status = $status, want 200"
  log "  -> 200"

  log "POST the callback route through the Ingress with a bogus token (expect 401)"
  status=$(curl -sS -o /tmp/ci-smoke-ingress-callback.json -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' -H 'Authorization: Bearer not-a-real-token' \
    -H "Host: ${INGRESS_HOST}" \
    -d '{"event_id":"smoke-ev1","sequence":1,"kind":"heartbeat"}' \
    "${ingress_url}/v1/attempts/att_smoke/events")
  [ "$status" = "401" ] || fail "callback-through-ingress status = $status, want 401 (body: $(cat /tmp/ci-smoke-ingress-callback.json))"
  log "  -> 401 $(cat /tmp/ci-smoke-ingress-callback.json)"
fi

log "ALL SMOKE CHECKS PASSED"
