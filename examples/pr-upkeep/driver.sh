#!/usr/bin/env bash
#
# ============================================================================
#  !!! LIVE-ONLY, BILLABLE, MANUAL DRIVER -- NEVER RUN IN CI !!!
# ============================================================================
#
# External driver for examples/pr-upkeep/workflow.yaml: validates the
# workflow, publishes it (idempotent by digest), and POSTs ONE run to
# /v1alpha1/runs. One run is one upkeep cycle-bundle: the loop edges INSIDE
# the workflow (approved -> sweep, changes_required -> fix, resume -> sweep)
# carry it from item to item until a terminal edge or a spec.limits bound
# ends it — then a human re-invokes this driver for the next cycle. The
# driver is deliberately external (spec decision: ad-hoc scheduling stays
# outside the engine); it holds no merge credential and neither does the
# run it creates.
#
# BILLABLE: the run's fix node dispatches a real claude-code session and its
# review node a real codex session per loop iteration. Like
# examples/codex-smoke-pair/run-smoke.sh, this script refuses to do anything
# without CONFIRM_BILLABLE=yes, and it must never be referenced from any
# file under .github/workflows/ or run unattended.
#
# The run parks on PEOPLE by design (the approval gate, the between-items
# human node), potentially for days — so unlike run-smoke.sh this driver
# does NOT poll to a terminal state. It creates the run, prints where it
# stands, and leaves the humans to the inbox (web /inbox or
# POST /v1alpha1/human-tasks/{id}/decision).
#
# Usage:
#   CONFIRM_BILLABLE=yes ./driver.sh
#
# Env overrides (all optional):
#   NODES_API_URL       default: http://thor:18080
#   PR_UPKEEP_REPO      default: /home/spark/git/culture-nodes
#                       (must be allowlisted by the fix/review bridges)
#   FIX_INSTRUCTION     override the fix node's instruction
#   REVIEW_INSTRUCTION  override the review node's instruction
#   PARK_INSTRUCTION    override the between-items human node's instruction
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW_FILE="$SCRIPT_DIR/workflow.yaml"

BASE_URL="${NODES_API_URL:-http://thor:18080}"
REPO="${PR_UPKEEP_REPO:-/home/spark/git/culture-nodes}"
FIX_INSTRUCTION="${FIX_INSTRUCTION:-Take the TOP item of the prioritised sweep report bound as sweepReport (its artifact refs carry the full JSON list). Work only that one item: implement the fix on a branch and open or update a PR for it. Never merge anything. Summarise what you changed and name the PR.}"
REVIEW_INSTRUCTION="${REVIEW_INSTRUCTION:-Read-only independent review of the fix described in fixReport, against the fix node evidence records (fixEvidence) and the run evidence trail (runEvidence). Verdict approve or changes_required with findings. Analysis only: change nothing.}"
PARK_INSTRUCTION="${PARK_INSTRUCTION:-The sweep found no unresolved SonarCloud issues and no open Qodo findings. Decide: resume (sweep again now) or done (end this run).}"

log() { printf '\n>>> %s\n' "$*"; }
fail() { printf '\n!!! %s\n' "$*" >&2; exit 1; }

# --- billable confirmation gate -------------------------------------------
if [ "${CONFIRM_BILLABLE:-}" != "yes" ]; then
	fail "refusing to run: each loop iteration of this run dispatches a real, billable claude-code fix session and a real codex review session. Re-run as: CONFIRM_BILLABLE=yes $0"
fi

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"
[ -f "$WORKFLOW_FILE" ] || fail "workflow file not found: $WORKFLOW_FILE"

log "target API: $BASE_URL"
log "repo=$REPO (single-repo flow: culture-nodes only, claim c26)"

# --- validate ---------------------------------------------------------------
log "POST /v1alpha1/workflows/validate"
source_json=$(jq -Rs '.' "$WORKFLOW_FILE")
validate_body=$(jq -n --argjson source "$source_json" '{format: "yaml", source: $source}')
validate_resp=$(curl -sS -w '\n%{http_code}' -H 'content-type: application/json' \
	-d "$validate_body" "${BASE_URL}/v1alpha1/workflows/validate")
validate_status=$(tail -n1 <<<"$validate_resp")
validate_payload=$(sed '$d' <<<"$validate_resp")
[ "$validate_status" = "200" ] || fail "validate returned HTTP $validate_status: $validate_payload"
valid=$(jq -r '.valid' <<<"$validate_payload")
[ "$valid" = "true" ] || fail "workflow does not validate: $validate_payload"
echo "valid: true, digest: $(jq -r '.digest' <<<"$validate_payload")"

# --- publish (idempotent by digest) ----------------------------------------
log "POST /v1alpha1/workflows (publish)"
publish_resp=$(curl -sS -w '\n%{http_code}' -H 'content-type: application/json' \
	-d "$validate_body" "${BASE_URL}/v1alpha1/workflows")
publish_status=$(tail -n1 <<<"$publish_resp")
publish_payload=$(sed '$d' <<<"$publish_resp")
[ "$publish_status" = "200" ] || [ "$publish_status" = "201" ] || fail "publish returned HTTP $publish_status: $publish_payload"
workflow_digest=$(jq -r '.digest' <<<"$publish_payload")
[ -n "$workflow_digest" ] && [ "$workflow_digest" != "null" ] || fail "publish response had no digest: $publish_payload"
echo "published: workflow_key=$(jq -r '.workflow_key' <<<"$publish_payload") digest=$workflow_digest"

# --- create the cycle's run ------------------------------------------------
log "POST /v1alpha1/runs (create) -- ONE upkeep cycle-bundle, BILLABLE per iteration"
run_input=$(jq -n \
	--arg repo "$REPO" \
	--arg fix_instruction "$FIX_INSTRUCTION" \
	--arg review_instruction "$REVIEW_INSTRUCTION" \
	--arg park_instruction "$PARK_INSTRUCTION" \
	'{repo: $repo,
	  fix_instruction: $fix_instruction,
	  review_instruction: $review_instruction,
	  review_sandbox: "read-only",
	  park_instruction: $park_instruction}')
run_body=$(jq -n --arg digest "$workflow_digest" --argjson input "$run_input" '{workflow_digest: $digest, input: $input}')
run_resp=$(curl -sS -w '\n%{http_code}' -H 'content-type: application/json' \
	-d "$run_body" "${BASE_URL}/v1alpha1/runs")
run_status=$(tail -n1 <<<"$run_resp")
run_payload=$(sed '$d' <<<"$run_resp")
[ "$run_status" = "201" ] || fail "create run returned HTTP $run_status: $run_payload"
run_id=$(jq -r '.id' <<<"$run_payload")
[ -n "$run_id" ] && [ "$run_id" != "null" ] || fail "create run response had no id: $run_payload"
echo "run created: id=$run_id state=$(jq -r '.state' <<<"$run_payload")"

# --- where it stands, then hand off to the humans --------------------------
log "GET /v1alpha1/runs/${run_id} (snapshot, not a poll)"
view_resp=$(curl -sS -w '\n%{http_code}' "${BASE_URL}/v1alpha1/runs/${run_id}")
view_status=$(tail -n1 <<<"$view_resp")
view_payload=$(sed '$d' <<<"$view_resp")
[ "$view_status" = "200" ] || fail "get run returned HTTP $view_status: $view_payload"
jq -c '{state: .run.state, nodes: [.node_runs[]? | {node_id, state, outcome}]}' <<<"$view_payload"

log "CYCLE STARTED"
echo "run_id=$run_id workflow_digest=$workflow_digest"
echo "watch:   ${BASE_URL}/v1alpha1/runs/${run_id}"
echo "ledger:  ${BASE_URL}/v1alpha1/runs/${run_id}/ledger"
echo "humans:  decide gate + park tasks via the web /inbox (or POST /v1alpha1/human-tasks/{id}/decision)"
echo "remember: 'approved' means YOU merged the PR yourself first — this flow cannot merge."
echo "next cycle: re-run this driver after the run reaches a terminal state."
