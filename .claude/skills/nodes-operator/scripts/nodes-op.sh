#!/usr/bin/env bash
# nodes-op.sh — the nodes-operator skill's entry point: drive a running
# culture-nodes control plane to inspect state, author + publish workflows,
# and assign real work to registered actors.
#
# API resolution: $NODES_API_URL, else ~/.culture-nodes/operator.env's
# NODES_API_URL line, else the thor production default. Everything speaks
# the public v1alpha1 HTTP surface — no psql, no ssh, no exceptions.
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
  running                      running runs + current node/attempt detail
  workflows                    published workflows (key, digest)
  runs [N]                     newest N runs (default 10)
  run <id>                     one run: state, node outcomes, attempts
  ledger <id>                  a run's ledger records
  tasks                        pending human tasks
  pending [run-id]             ledger claims still awaiting a human decision
                                (proposed, and no review record names them —
                                NOT an authority filter: confirming a claim
                                appends a review, it never rewrites the claim)
  cancel <id>                  cancel a run (reaps items, propagates actor Cancel)
  validate <file.yaml>         server-side compile check, prints digest
  publish <file.yaml>          validate + publish, prints digest
  create <digest> <input.json> [--category C] create a run (BILLABLE for agent nodes; needs --yes)
  watch <id> [timeout-s]       poll a run to terminal, print outcomes + claims
  grade <run-id> --rating N --notes "..." [--actor ID] [--as ID] [--category C]
                                grade a run against an actor (1-5 rating + rationale).
                                --as defaults to the first registered kind=human actor;
                                --actor defaults to the run's most recent attempt actor.
                                Human --as lands confirmed; agent --as lands proposed.
  assign <actor> "<instruction>" [opts]   one-node workflow -> publish -> run -> watch
      opts: --sandbox read-only|workspace-write   (default read-only)
            --mode plan|default|auto-edit|auto    (qwen actors only; required there)
            --base-ref REF                         (bridge-fetched trusted base)
            --timeout DUR                          (default 15m)
            --retries N                            (default 1 — no auto-retry)
            --outcome NAME                         (default completed)
            --category C                           (optional run category tag)
            --devague-write                        (this package writes .devague/ —
                                                    developer lane only, see below)
            --no-watch                             (create and return the run id)
            --yes                                  (required: this bills a session)
  actors                       registered actor rows, over the API (no ssh)

Actors known to `assign`:
  codex-thor, codex-orin   codex bridges on thor/orin. Cross-machine, separate
                           identity. Go IS installed on both now (~/.local),
                           so build and vet work — but the sandbox denies
                           socket(2) outright, for loopback as well as egress
                           (#119, measured), so NOTHING database-backed and
                           nothing that binds a listener can run there. They
                           can author Go; they cannot gate it. npm and uv are
                           still absent/broken (#96). Route database-backed
                           work elsewhere rather than accepting an unrun test.
  developer, planner,      claude bridges on spark. Full toolchain, so they can
  verifier, intake         actually run what they write — but all four share
                           ONE subscription window with the operator's own
                           session (#48, #97), so they are not four independent
                           capacity pools. Fan out accordingly.

Each actor defaults to its own worktree; `--repo <path>` pins a dispatch
elsewhere. The bridge's own repo_allowlist is the real gate (exact-match), and
for the claude bridges it is the ONLY one — `claude -p` takes no sandbox flag.

.devague/ custody (t13, #199): exactly ONE lane may write devague frames —
developer, in its own worktree — declared in that bridge's config (`custody`
block, adapters/claude-code config.py) and mirrored in this script's custody
table. `--devague-write` on any other actor, or on developer with a --repo
that is not the custody checkout, is refused here before anything is billed;
the bridge refuses the same request again on its own declaration.
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
running)
  tmp=$(mktemp -d)
  trap 'find "$tmp" -depth -delete' EXIT
  api_get "/v1alpha1/runs?state=running&limit=500" > "$tmp/runs.json"
  python3 - "$tmp/runs.json" "$tmp/ids" <<'PYEOF'
import json, sys
d = json.load(open(sys.argv[1]))
runs = d if isinstance(d, list) else d.get("runs", d.get("items", []))
with open(sys.argv[2], "w") as ids:
    for run in runs:
        ids.write(run["id"] + "\n")
print("running runs:", len(runs))
PYEOF
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    api_get "/v1alpha1/runs/$id" | py '
import json, sys
d = json.load(sys.stdin); r = d.get("run", d)
print("%s  name=%s  category=%s" % (r.get("id"), r.get("name") or "-", r.get("category") or "-"))
print("  description:", r.get("description") or "-")
print("  input:", json.dumps(r.get("input"), ensure_ascii=False))
for nr in d.get("node_runs", []):
    attempts = ["%s:%s" % (a.get("actor_id") or "?", a.get("status") or "?") for a in nr.get("attempts", [])]
    print("  %s: %s outcome=%s attempts=%s" % (
        nr.get("node_id"), nr.get("state"), nr.get("outcome"), attempts))'
  done < "$tmp/ids"
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
    print(r.get("authority"), r.get("record_type"), o.get("actor_id"), "--", json.dumps(r.get("data",{}), ensure_ascii=False))'
  ;;
tasks)
  api_get /v1alpha1/human-tasks | py '
import json,sys
d=json.load(sys.stdin); items=d if isinstance(d,list) else d.get("items",d.get("human_tasks",[]))
if not items: print("no pending human tasks")
for t in items: print(t.get("id"), t.get("status"), str(t.get("request",""))[:100])'
  ;;
pending)
  # GET /v1alpha1/pending-decisions: the affirmative half of PRD §10.4's
  # discoverability. Decide what this prints with scripts/decide-claims.py;
  # both read the same server-side rule, so the gate and the queue cannot
  # disagree about what "undecided" means.
  q=""; [ -n "${1:-}" ] && q="?run_id=$1"
  api_get "/v1alpha1/pending-decisions$q" | py '
import json,sys
d=json.load(sys.stdin); items=d.get("items",[])
if not items: print("nothing awaiting a decision"); raise SystemExit(0)
print(f"{d.get("record_count",0)} record(s) awaiting a decision across {len(items)} run(s)")
for g in items:
    print(f"  {g["run_id"]}  (ledger_version {g["ledger_version"]})")
    for r in g.get("records",[]):
        print(f"    {r["id"]}  {r["record_type"]}  from {r.get("origin_actor_id","-")}")'
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
  digest="${1:?usage: create <digest> <input.json> [--category C] [--yes]}"; input="${2:?input json file}"; shift 2 || true
  category=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --yes) ASSUME_YES=1; shift;;
      --category) category="$2"; shift 2;;
      --repo) repo_override="$2"; shift 2;;
      *) echo "nodes-op: unknown create option $1" >&2; exit 1;;
    esac
  done
  need_yes
  body=$(python3 - "$digest" "$input" "$category" <<'PYEOF'
import json, sys
digest, input_path, category = sys.argv[1], sys.argv[2], sys.argv[3]
body = {"workflow_digest": digest, "input": json.load(open(input_path))}
if category:
    body["category"] = category
print(json.dumps(body))
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
grade)
  run_id="${1:?usage: grade <run-id> --rating N --notes \"...\" [--actor ID] [--as ID] [--category C] [--node-run-ref REF] [--attempt-ref REF]}"; shift
  rating=""; notes=""; actor=""; as_actor=""; category=""; node_run_ref=""; attempt_ref=""
  while [ $# -gt 0 ]; do
    case "$1" in
      --rating) rating="$2"; shift 2;;
      --notes) notes="$2"; shift 2;;
      --actor) actor="$2"; shift 2;;
      --as) as_actor="$2"; shift 2;;
      --category) category="$2"; shift 2;;
      --repo) repo_override="$2"; shift 2;;
      --node-run-ref) node_run_ref="$2"; shift 2;;
      --attempt-ref) attempt_ref="$2"; shift 2;;
      *) echo "nodes-op: unknown grade option $1" >&2; exit 1;;
    esac
  done
  [[ "$rating" =~ ^[1-5]$ ]] || { echo "nodes-op: grade requires --rating N with N in 1-5 (got '${rating:-<empty>}')" >&2; exit 1; }
  [ -n "$notes" ] || { echo "nodes-op: grade requires --notes \"rationale text\"" >&2; exit 1; }

  # --as defaults to the first registered kind=human actor -- discoverable
  # cheaply from the actors listing already exposed for the `actors`-less
  # (ssh-free) case: GET /v1alpha1/actors renders each row's kind.
  if [ -z "$as_actor" ]; then
    as_actor=$(api_get /v1alpha1/actors | py '
import json, sys
items = json.load(sys.stdin).get("items", [])
human = [a["id"] for a in items if a.get("kind") == "human"]
print(human[0] if human else "")')
    [ -n "$as_actor" ] || { echo "nodes-op: --as not given and no kind=human actor is registered — pass --as ACTOR_ID" >&2; exit 1; }
  fi

  # --actor defaults to the run's most recently attempted actor -- one extra
  # GET, the same node_runs[].attempts[].actor_id the `run <id>` verb above
  # already prints.
  if [ -z "$actor" ]; then
    actor=$(api_get "/v1alpha1/runs/$run_id" | py '
import json, sys
d = json.load(sys.stdin)
ids = [a.get("actor_id") for nr in d.get("node_runs", []) for a in nr.get("attempts", []) if a.get("actor_id")]
print(ids[-1] if ids else "")')
    [ -n "$actor" ] || { echo "nodes-op: --actor not given and no assigned actor could be discovered on run $run_id — pass --actor ACTOR_ID" >&2; exit 1; }
  fi

  body=$(python3 - "$rating" "$notes" "$actor" "$as_actor" "$category" "$node_run_ref" "$attempt_ref" <<'PYEOF'
import json, sys
rating, notes, actor, as_actor, category, node_run_ref, attempt_ref = sys.argv[1:8]
body = {
    "rating": int(rating),
    "rationale": notes,
    "evaluated_actor_id": actor,
    "grading_actor_id": as_actor,
}
if category:
    body["category"] = category
if node_run_ref:
    body["node_run_ref"] = node_run_ref
if attempt_ref:
    body["attempt_ref"] = attempt_ref
print(json.dumps(body))
PYEOF
)
  api_post "/v1alpha1/runs/$run_id/grades" "$body" | py '
import json, sys
d = json.load(sys.stdin)
origin = d.get("origin", {})
data = d.get("data", {})
print(d.get("id", ""), d.get("authority", ""), origin.get("kind", ""),
      "rating=" + str(data.get("rating", "")), "actor=" + str(data.get("evaluated_actor_id", "")))'
  ;;
assign)
  actor="${1:?usage: assign <codex-thor|codex-orin|developer|planner|verifier|intake|qwen-developer> \"instruction\" [opts]}"; shift
  instruction="${1:?assign needs an instruction}"; shift
  sandbox=read-only; timeout=15m; retries=1; outcome=completed; watch=1; category=""; repo_override=""; handover=false; mode=""; base_ref=""; devague_write=false
  while [ $# -gt 0 ]; do
    case "$1" in
      --sandbox) sandbox="$2"; shift 2;;
      # ACP session mode, required by the qwen bridge and ignored by the
      # others. The qwen gate sets the mode from policy and NEVER falls back
      # to the agent's measured default (h15), so a dispatch that names none
      # is refused -- and on the async path that refusal loses its message and
      # reads as "killed, crashed, or timed out" (#225). Until #225 lands,
      # forgetting this flag costs a confusing round trip, not a clear error.
      --mode) mode="$2"; shift 2;;
      --base-ref) base_ref="$2"; shift 2;;
      --timeout) timeout="$2"; shift 2;;
      --retries) retries="$2"; shift 2;;
      --outcome) outcome="$2"; shift 2;;
      --category) category="$2"; shift 2;;
      --repo) repo_override="$2"; shift 2;;
      # t9 / #90: ask the actor to hand its changes over as a git ref. On
      # codex this also opens `.git` for writing, so the session can commit
      # its own work instead of leaving a working tree for the operator to
      # collect over ssh. Opt-in: a verification package hands nothing over
      # and must stay unable to write .git.
      --handover) handover=true; shift;;
      # t13 / #199: this package will write .devague/ (run devague moves and
      # commit the frame). Only the lane holding custody may; see the
      # custody check below the actor table.
      --devague-write) devague_write=true; shift;;
      --no-watch) watch=0; shift;;
      --yes) ASSUME_YES=1; shift;;
      *) echo "nodes-op: unknown assign option $1" >&2; exit 1;;
    esac
  done
  case "$actor" in
    codex-thor) ref="actor://company/codex-thor@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"; repo=/home/thor/git/culture-nodes-agent;;
    codex-orin) ref="actor://company/codex-orin@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"; repo=/home/orin/git/culture-nodes-agent;;
    # The four spark claude bridges. Each defaults to its OWN lane worktree,
    # never the operator's checkout: `claude -p` takes no sandbox flag and runs
    # with the bridge process's own privileges (its capability surface says so
    # outright -- "confinement: none"), so the repo allowlist is the only
    # boundary there is. developer used to default to /home/spark/git/culture-nodes,
    # which is the checkout the operator is working in; a dispatch and a merge
    # gate would have been writing to the same tree at once (the c42 concurrent
    # -writer corruption mode).
    developer) ref="actor://company/developer@sha256:3333333333333333333333333333333333333333333333333333333333333333"; repo=/home/spark/git/.worktrees.culture-nodes/owe-developer;;
    planner)   ref="actor://company/planner@sha256:4444444444444444444444444444444444444444444444444444444444444444";   repo=/home/spark/git/.worktrees.culture-nodes/owe-planner;;
    verifier)  ref="actor://company/verifier@sha256:5555555555555555555555555555555555555555555555555555555555555555";  repo=/home/spark/git/.worktrees.culture-nodes/owe-verifier;;
    intake)    ref="actor://company/intake@sha256:6666666666666666666666666666666666666666666666666666666666666666";    repo=/home/spark/git/.worktrees.culture-nodes/owe-intake;;
    # The qwen bridge on spark:8092 (adapters/qwen, registered 2026-08-27).
    # Same reasoning as the claude bridges above, and then some: its own
    # capability surface reports `confinement: qwen-code runs its own tools
    # in-process as the bridge user` -- the ACP session modes are an approval
    # policy, not a kernel boundary -- so the exact-match repo allowlist is the
    # only thing standing between a dispatch and this operator's checkout.
    qwen-developer) ref="actor://company/qwen-developer@sha256:7777777777777777777777777777777777777777777777777777777777777777"; repo=/home/spark/git/.worktrees.culture-nodes/qwen-dev;;
    *) echo "nodes-op: unknown actor '$actor' (codex-thor|codex-orin|developer|planner|verifier|intake|qwen-developer)" >&2; exit 1;;
  esac
  # --repo pins a dispatch to one isolated worktree; the bridge's own
  # repo_allowlist is the real gate (exact-match), so an unlisted path is
  # refused by the actor rather than trusted here.
  [ -n "$repo_override" ] && repo="$repo_override"
  # .devague/ CUSTODY TABLE (t13, #199 / #230; frame decision q1). One lane,
  # one checkout. This mirrors the `custody` block in that lane's bridge
  # config (adapters/claude-code config.py, ~/.config/culture-nodes-bridges/
  # developer.json on spark); the bridge re-checks its own declaration at
  # dispatch, so this table cannot widen custody — it only refuses earlier,
  # before a session is billed. Checked BEFORE need_yes: a refusal here is a
  # routing error, and "re-run with --yes" would be the wrong hint for it.
  devague_custody_repo=""
  case "$actor" in
    developer) devague_custody_repo=/home/spark/git/.worktrees.culture-nodes/owe-developer;;
  esac
  if [ "$devague_write" = true ]; then
    if [ -z "$devague_custody_repo" ]; then
      echo "nodes-op: refusing: .devague/ custody is declared on the developer lane only; '$actor' may not write .devague/ — route the package to developer or drop --devague-write (docs/operations/spec-chain-lane.md)" >&2
      exit 1
    fi
    if [ "$repo" != "$devague_custody_repo" ]; then
      echo "nodes-op: refusing: .devague/ custody on the developer lane is bound to $devague_custody_repo; a --devague-write dispatch into '$repo' is not that checkout" >&2
      exit 1
    fi
  fi
  need_yes
  wf=$(mktemp); trap 'rm -f "$wf" "$wf.json"' EXIT
  [[ "$outcome" =~ ^[a-z][a-z0-9_]*$ ]] || { echo "nodes-op: --outcome must match the workflow schema outcomeName pattern ^[a-z][a-z0-9_]*$ (got '$outcome')" >&2; exit 1; }
  sed -e "s|__NAME__|assign-$actor|" -e "s|__ACTOR_REF__|$ref|" \
      -e "s|__TIMEOUT__|$timeout|" -e "s|__MAX_ATTEMPTS__|$retries|" \
      -e "s|__OUTCOME__|$outcome|" \
      "$TEMPLATE" > "$wf"
  # #240: the template binds mode: /run/input/mode unconditionally, but the
  # run input omits `mode` when --mode is not given (see the payload note
  # below), and a binding to an absent member is refused by the worker
  # (internal/worker/bindings.go: no member "mode" at /run/input/mode) --
  # contract_rejected in ~150ms, before any bridge is called. Strip the
  # binding for a mode-less dispatch so the digest carries only bindings the
  # input can satisfy.
  [ -n "$mode" ] || sed -i '/^[[:space:]]*mode: \/run\/input\/mode[[:space:]]*$/d' "$wf"
  [ -n "$base_ref" ] || sed -i '/^[[:space:]]*base_ref: \/run\/input\/base_ref[[:space:]]*$/d' "$wf"
  [ "$devague_write" = true ] || sed -i '/^[[:space:]]*devague_write: \/run\/input\/devague_write[[:space:]]*$/d' "$wf"
  digest=$("$0" publish "$wf")
  [ -n "$digest" ] || { echo "nodes-op: publish returned no digest" >&2; exit 1; }
  python3 - "$instruction" "$sandbox" "$outcome" "$repo" "$handover" "$mode" "$base_ref" "$devague_write" <<'PYEOF' > "$wf.json"
import json, sys
payload = {"instruction": sys.argv[1], "sandbox": sys.argv[2],
           "success_outcome": sys.argv[3], "repo": sys.argv[4],
           "handover": sys.argv[5] == "true"}
# t13: present only when asked, for the same reason `mode` is — an explicit
# false in the run input would read as a custody decision nobody made.
if sys.argv[8] == "true":
    payload["devague_write"] = True
# omitted rather than sent empty: the qwen gate rejects a blank mode with the
# same refusal as a missing one, and an empty string in the run input would
# read as "the operator chose this" in the ledger.
if sys.argv[6]:
    payload["mode"] = sys.argv[6]
if sys.argv[7]:
    payload["base_ref"] = sys.argv[7]
print(json.dumps(payload))
PYEOF
  if [ -n "$category" ]; then
    out=$(NODES_OP_YES=1 "$0" create "$digest" "$wf.json" --category "$category")
  else
    out=$(NODES_OP_YES=1 "$0" create "$digest" "$wf.json")
  fi
  run_id=$(echo "$out" | awk '{print $1}')
  echo "assigned: run=$run_id actor=$actor sandbox=$sandbox${mode:+ mode=$mode} timeout=$timeout${category:+ category=$category}${handover:+ handover=$handover}$([ "$devague_write" = true ] && echo " devague_write=true")"
  # An `if`, not `[ ... ] && ...`: as the arm's last command, a false test
  # in an && list leaves the script's exit status at 1, so every --no-watch
  # dispatch reported failure after succeeding (found by t13's tests).
  if [ "$watch" = "1" ]; then "$0" watch "$run_id"; fi
  ;;
actors)
  # Reads the registry over the public API. This used to shell out to thor's
  # compose psql and was the ONE verb in this skill needing an `ssh thor`
  # alias; stage-1 verification (run 01M03BV3DYNB9N7J1Q1RJ55HNB) established
  # that GET /v1alpha1/actors already returns actor_key, revision AND
  # endpoint_ref, so the ssh path was answering a question the API answers.
  # Registration itself still goes through register-actor.sh.
  api_get /v1alpha1/actors | py 'import json,sys
rows = json.load(sys.stdin).get("items", [])
for r in sorted(rows, key=lambda r: (r.get("actor_key",""), r.get("revision",0))):
    print("|".join([str(r.get("actor_key","")), str(r.get("revision","")), str(r.get("endpoint_ref") or "")]))'
  ;;
*)
  usage
  ;;
esac
