#!/usr/bin/env bash
# nodes-op.sh — the nodes-operator skill's entry point: drive a running
# culture-nodes control plane to inspect state, author + publish workflows,
# and assign real work to registered actors.
#
# API resolution: $NODES_API_URL, else ~/.culture-nodes/operator.env's
# NODES_API_URL line, else the thor production default. Everything speaks
# the public v1alpha1 HTTP surface — no psql, no ssh, except the `actors`
# verb which documents its ssh dependency inline.
#
# Billable guard: `assign` and `create` dispatch real agent sessions.
# They refuse without --yes (or NODES_OP_YES=1) so a casual invocation
# can never spend quota silently.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="$SCRIPT_DIR/../templates/assign.workflow.yaml"

if [ -z "${NODES_API_URL:-}" ] && [ -f "$HOME/.culture-nodes/operator.env" ]; then
  NODES_API_URL=$(grep -E '^NODES_API_URL=' "$HOME/.culture-nodes/operator.env" | tail -1 | cut -d= -f2-)
fi
API="${NODES_API_URL:-http://192.168.1.146:18080}"

usage() {
  cat >&2 <<'EOF'
usage: nodes-op.sh <verb> [args]

  status                       healthz + run counts
  workflows                    published workflows (key, digest)
  runs [N]                     newest N runs (default 10)
  run <id>                     one run: state, node outcomes, attempts
  ledger <id>                  a run's ledger records
  tasks                        pending human tasks
  cancel <id>                  cancel a run (reaps items, propagates actor Cancel)
  validate <file.yaml>         server-side compile check, prints digest
  publish <file.yaml>          validate + publish, prints digest
  create <digest> <input.json> create a run (BILLABLE for agent nodes; needs --yes)
  watch <id> [timeout-s]       poll a run to terminal, print outcomes + claims
  assign <actor> "<instruction>" [opts]   one-node workflow -> publish -> run -> watch
      opts: --sandbox read-only|workspace-write   (default read-only)
            --timeout DUR                          (default 15m)
            --retries N                            (default 1 — no auto-retry)
            --outcome NAME                         (default completed)
            --no-watch                             (create and return the run id)
            --yes                                  (required: this bills a session)
  actors                       registered actors (requires `ssh thor`)

Actors known to `assign`: codex-thor, codex-orin (repo allowlists are per-host).
EOF
  exit 1
}

py() { python3 -c "$1"; }
api_get() { curl -fsS -m 20 "$API$1"; }
api_post() { curl -fsS -m 60 -H 'content-type: application/json' -d "$2" "$API$1"; }

need_yes() {
  if [ "${NODES_OP_YES:-0}" != "1" ] && [ "${ASSUME_YES:-0}" != "1" ]; then
    echo "nodes-op: refusing: this dispatches a real, billable agent session — re-run with --yes (or NODES_OP_YES=1)" >&2
    exit 1
  fi
}

verb="${1:-}"; [ -n "$verb" ] || usage; shift || true

case "$verb" in
status)
  api_get /v1alpha1/healthz; echo
  api_get "/v1alpha1/runs?limit=200" | py '
import json,sys
runs=json.load(sys.stdin); runs=runs if isinstance(runs,list) else runs.get("runs",runs.get("items",[]))
from collections import Counter
c=Counter(r.get("state","?") for r in runs)
print("runs (newest 200):", dict(c))'
  ;;
workflows)
  api_get /v1alpha1/workflows | py '
import json,sys
d=json.load(sys.stdin); items=d if isinstance(d,list) else d.get("workflows",d.get("items",[]))
for w in items: print(w.get("workflow_key",w.get("key","?")), w.get("digest",""))'
  ;;
runs)
  n="${1:-10}"
  [[ "$n" =~ ^[1-9][0-9]{0,3}$ ]] || { echo "nodes-op: runs takes a positive integer (got '$n')" >&2; exit 1; }
  api_get "/v1alpha1/runs?limit=$n" | py "
import json,sys
runs=json.load(sys.stdin); runs=runs if isinstance(runs,list) else runs.get('runs',runs.get('items',[]))
for r in runs[:$n]: print(r['id'], r.get('state','?'), r.get('created_at',''))"
  ;;
run)
  id="${1:?usage: run <id>}"
  tmp=$(mktemp); api_get "/v1alpha1/runs/$id" > "$tmp"
  python3 - "$tmp" <<'PYEOF'
import json, sys
d = json.load(open(sys.argv[1]))
r = d.get("run", d)
print("state:", r.get("state"))
for nr in d.get("node_runs", []):
    atts = [(a.get("status"), a.get("actor_id")) for a in nr.get("attempts", [])]
    print("  %s: %s outcome=%s attempts=%s" % (
        nr.get("node_id"), nr.get("state"), nr.get("outcome"), atts))
PYEOF
  rm -f "$tmp"
  ;;
ledger)
  id="${1:?usage: ledger <id>}"
  api_get "/v1alpha1/runs/$id/ledger" | py '
import json,sys
d=json.load(sys.stdin)
for r in d.get("items",[]):
    o=r.get("origin",{})
    print(r.get("authority"), r.get("record_type"), o.get("actor_id"), "--", json.dumps(r.get("data",{}))[:160])'
  ;;
tasks)
  api_get /v1alpha1/human-tasks | py '
import json,sys
d=json.load(sys.stdin); items=d if isinstance(d,list) else d.get("items",d.get("human_tasks",[]))
if not items: print("no pending human tasks")
for t in items: print(t.get("id"), t.get("status"), str(t.get("request",""))[:100])'
  ;;
cancel)
  id="${1:?usage: cancel <id>}"
  api_post "/v1alpha1/runs/$id/cancel" '{}' | py '
import json,sys; print("cancel ->", json.load(sys.stdin).get("state"))'
  ;;
validate)
  f="${1:?usage: validate <file.yaml>}"
  body=$(python3 - "$f" <<'PYEOF'
import json, sys
print(json.dumps({"format": "yaml", "source": open(sys.argv[1]).read()}))
PYEOF
)
  api_post /v1alpha1/workflows/validate "$body" | py '
import json,sys; d=json.load(sys.stdin)
print("valid:", d.get("valid"), "digest:", d.get("digest"))
for diag in d.get("diagnostics",[]) or []: print("  -", diag)'
  ;;
publish)
  f="${1:?usage: publish <file.yaml>}"
  body=$(python3 - "$f" <<'PYEOF'
import json, sys
print(json.dumps({"format": "yaml", "source": open(sys.argv[1]).read()}))
PYEOF
)
  api_post /v1alpha1/workflows "$body" | py '
import json,sys; d=json.load(sys.stdin)
print(d.get("digest",""))'
  ;;
create)
  digest="${1:?usage: create <digest> <input.json> [--yes]}"; input="${2:?input json file}"
  [ "${3:-}" = "--yes" ] && ASSUME_YES=1
  need_yes
  body=$(python3 - "$digest" "$input" <<'PYEOF'
import json, sys
print(json.dumps({"workflow_digest": sys.argv[1], "input": json.load(open(sys.argv[2]))}))
PYEOF
)
  api_post /v1alpha1/runs "$body" | py 'import json,sys; d=json.load(sys.stdin); print(d["id"], d.get("state"))'
  ;;
watch)
  id="${1:?usage: watch <id> [timeout-s]}"; limit="${2:-1800}"; t=0
  while [ "$t" -lt "$limit" ]; do
    state=$(api_get "/v1alpha1/runs/$id" | py 'import json,sys; print(json.load(sys.stdin).get("run",{}).get("state",""))')
    case "$state" in completed|failed|cancelled) break;; esac
    sleep 5; t=$((t+5))
  done
  echo "state: $state"
  "$0" run "$id"
  "$0" ledger "$id"
  ;;
assign)
  actor="${1:?usage: assign <codex-thor|codex-orin> \"instruction\" [opts]}"; shift
  instruction="${1:?assign needs an instruction}"; shift
  sandbox=read-only; timeout=15m; retries=1; outcome=completed; watch=1
  while [ $# -gt 0 ]; do
    case "$1" in
      --sandbox) sandbox="$2"; shift 2;;
      --timeout) timeout="$2"; shift 2;;
      --retries) retries="$2"; shift 2;;
      --outcome) outcome="$2"; shift 2;;
      --no-watch) watch=0; shift;;
      --yes) ASSUME_YES=1; shift;;
      *) echo "nodes-op: unknown assign option $1" >&2; exit 1;;
    esac
  done
  case "$actor" in
    codex-thor) ref="actor://company/codex-thor@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"; repo=/home/thor/git/culture-nodes-agent;;
    codex-orin) ref="actor://company/codex-orin@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"; repo=/home/orin/git/culture-nodes-agent;;
    *) echo "nodes-op: unknown actor '$actor' (codex-thor|codex-orin)" >&2; exit 1;;
  esac
  need_yes
  wf=$(mktemp); trap 'rm -f "$wf" "$wf.json"' EXIT
  [[ "$outcome" =~ ^[a-z][a-z0-9_]*$ ]] || { echo "nodes-op: --outcome must match the workflow schema outcomeName pattern ^[a-z][a-z0-9_]*$ (got '$outcome')" >&2; exit 1; }
  sed -e "s|__NAME__|assign-$actor|" -e "s|__ACTOR_REF__|$ref|" \
      -e "s|__TIMEOUT__|$timeout|" -e "s|__MAX_ATTEMPTS__|$retries|" \
      -e "s|__OUTCOME__|$outcome|" \
      "$TEMPLATE" > "$wf"
  digest=$("$0" publish "$wf")
  [ -n "$digest" ] || { echo "nodes-op: publish returned no digest" >&2; exit 1; }
  python3 - "$instruction" "$sandbox" "$outcome" "$repo" <<'PYEOF' > "$wf.json"
import json, sys
print(json.dumps({"instruction": sys.argv[1], "sandbox": sys.argv[2],
                  "success_outcome": sys.argv[3], "repo": sys.argv[4]}))
PYEOF
  out=$(NODES_OP_YES=1 "$0" create "$digest" "$wf.json")
  run_id=$(echo "$out" | awk '{print $1}')
  echo "assigned: run=$run_id actor=$actor sandbox=$sandbox timeout=$timeout"
  [ "$watch" = "1" ] && "$0" watch "$run_id"
  ;;
actors)
  # Reads the registry through thor's compose psql — the one verb that
  # needs the `ssh thor` alias (registration itself stays register-actor.sh).
  ssh thor 'cd culture-nodes-prod/deploy/prod && docker compose --env-file ~/.culture-nodes/prod.env -f compose.thor.yml exec -T postgres psql -U nodes -d nodes -Atc "SELECT actor_key, revision, endpoint_ref FROM actors ORDER BY actor_key, revision"'
  ;;
*)
  usage
  ;;
esac
