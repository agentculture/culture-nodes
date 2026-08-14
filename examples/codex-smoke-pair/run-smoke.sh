#!/usr/bin/env bash
#
# ============================================================================
#  !!! LIVE-ONLY, BILLABLE, MANUAL VERIFICATION SCRIPT -- NEVER RUN IN CI !!!
# ============================================================================
#
# This script dispatches TWO REAL, BILLABLE `codex exec` sessions -- one
# against the company/codex-thor actor, one against company/codex-orin --
# through a running production (or equivalent) culture-nodes deployment. It
# is the issue #14 / task t8 acceptance check: prove both registered codex
# actors can complete a real node through the engine's normal dispatch path
# and each write their own `proposed` ledger claim.
#
# Unlike adapters/colleague's COLLEAGUE_ENGINE=mock, codex has no offline
# deterministic engine (adapters/codex/README.md, "Trust model"), so there is
# no way to exercise this workflow without spending real ChatGPT/API quota.
# Both nodes ask codex only to report the repo HEAD commit and confirm it
# can read README.md (`sandbox: read-only` in each node's input -- see
# adapters/codex/README.md's "Invocation input fields" table), so the run
# cannot mutate either agent checkout, but it IS a real dispatched session on
# each host and IS billed as one.
#
# This script must never be referenced from any file under .github/workflows/
# and must never run unattended. Set CONFIRM_BILLABLE=yes to acknowledge that
# before it will do anything.
#
# Usage:
#   CONFIRM_BILLABLE=yes ./run-smoke.sh
#
# Env overrides (all optional):
#   NODES_API_URL           default: http://thor:18080
#   FIRST_REPO              default: /home/thor/git/culture-nodes-agent
#   SECOND_REPO             default: /home/orin/git/culture-nodes-agent
#   SMOKE_INSTRUCTION       default: read-only HEAD/README check (see below)
#   SMOKE_SANDBOX           default: read-only
#   SMOKE_SUCCESS_OUTCOME   default: completed
#   POLL_INTERVAL_SECONDS   default: 5
#   POLL_TIMEOUT_SECONDS    default: 2700 (45m -- both nodes' own policy.timeout
#                            is 15m each, sequential, plus engine/queue overhead)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW_FILE="$SCRIPT_DIR/smoke.workflow.yaml"

BASE_URL="${NODES_API_URL:-http://thor:18080}"
FIRST_REPO="${FIRST_REPO:-/home/thor/git/culture-nodes-agent}"
SECOND_REPO="${SECOND_REPO:-/home/orin/git/culture-nodes-agent}"
SMOKE_INSTRUCTION="${SMOKE_INSTRUCTION:-Report the repo HEAD commit (git rev-parse HEAD) and confirm you can read README.md. Make no changes.}"
SMOKE_SANDBOX="${SMOKE_SANDBOX:-read-only}"
SMOKE_SUCCESS_OUTCOME="${SMOKE_SUCCESS_OUTCOME:-completed}"
POLL_INTERVAL_SECONDS="${POLL_INTERVAL_SECONDS:-5}"
POLL_TIMEOUT_SECONDS="${POLL_TIMEOUT_SECONDS:-2700}"

log() { printf '\n>>> %s\n' "$*"; }
fail() { printf '\n!!! %s\n' "$*" >&2; exit 1; }

# --- billable confirmation gate -------------------------------------------
if [ "${CONFIRM_BILLABLE:-}" != "yes" ]; then
	fail "refusing to run: this dispatches real, billable codex exec sessions on thor and orin. Re-run as: CONFIRM_BILLABLE=yes $0"
fi

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"
[ -f "$WORKFLOW_FILE" ] || fail "workflow file not found: $WORKFLOW_FILE"

log "target API: $BASE_URL"
log "first_repo=$FIRST_REPO second_repo=$SECOND_REPO sandbox=$SMOKE_SANDBOX success_outcome=$SMOKE_SUCCESS_OUTCOME"

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

# --- publish ------------------------------------------------------------
log "POST /v1alpha1/workflows (publish)"
publish_resp=$(curl -sS -w '\n%{http_code}' -H 'content-type: application/json' \
	-d "$validate_body" "${BASE_URL}/v1alpha1/workflows")
publish_status=$(tail -n1 <<<"$publish_resp")
publish_payload=$(sed '$d' <<<"$publish_resp")
[ "$publish_status" = "200" ] || [ "$publish_status" = "201" ] || fail "publish returned HTTP $publish_status: $publish_payload"
workflow_digest=$(jq -r '.digest' <<<"$publish_payload")
[ -n "$workflow_digest" ] && [ "$workflow_digest" != "null" ] || fail "publish response had no digest: $publish_payload"
echo "published: workflow_key=$(jq -r '.workflow_key' <<<"$publish_payload") digest=$workflow_digest"

# --- create run ---------------------------------------------------------
log "POST /v1alpha1/runs (create) -- THIS DISPATCHES REAL BILLABLE CODEX SESSIONS"
run_input=$(jq -n \
	--arg instruction "$SMOKE_INSTRUCTION" \
	--arg sandbox "$SMOKE_SANDBOX" \
	--arg success_outcome "$SMOKE_SUCCESS_OUTCOME" \
	--arg first_repo "$FIRST_REPO" \
	--arg second_repo "$SECOND_REPO" \
	'{instruction: $instruction, sandbox: $sandbox, success_outcome: $success_outcome, first_repo: $first_repo, second_repo: $second_repo}')
run_body=$(jq -n --arg digest "$workflow_digest" --argjson input "$run_input" '{workflow_digest: $digest, input: $input}')
run_resp=$(curl -sS -w '\n%{http_code}' -H 'content-type: application/json' \
	-d "$run_body" "${BASE_URL}/v1alpha1/runs")
run_status=$(tail -n1 <<<"$run_resp")
run_payload=$(sed '$d' <<<"$run_resp")
[ "$run_status" = "201" ] || fail "create run returned HTTP $run_status: $run_payload"
run_id=$(jq -r '.id' <<<"$run_payload")
[ -n "$run_id" ] && [ "$run_id" != "null" ] || fail "create run response had no id: $run_payload"
echo "run created: id=$run_id state=$(jq -r '.state' <<<"$run_payload")"

# --- poll to a terminal state --------------------------------------------
log "polling GET /v1alpha1/runs/${run_id} until terminal (timeout ${POLL_TIMEOUT_SECONDS}s)"
elapsed=0
final_state=""
view_payload=""
while [ "$elapsed" -lt "$POLL_TIMEOUT_SECONDS" ]; do
	view_resp=$(curl -sS -w '\n%{http_code}' "${BASE_URL}/v1alpha1/runs/${run_id}")
	view_status=$(tail -n1 <<<"$view_resp")
	view_payload=$(sed '$d' <<<"$view_resp")
	[ "$view_status" = "200" ] || fail "get run returned HTTP $view_status: $view_payload"
	final_state=$(jq -r '.run.state' <<<"$view_payload")
	case "$final_state" in
	completed | failed | cancelled)
		break
		;;
	*)
		echo "  ... state=$final_state (elapsed ${elapsed}s)"
		sleep "$POLL_INTERVAL_SECONDS"
		elapsed=$((elapsed + POLL_INTERVAL_SECONDS))
		;;
	esac
done
[ -n "$final_state" ] || fail "never got a run view"
case "$final_state" in
completed | failed | cancelled) : ;;
*) fail "run ${run_id} did not reach a terminal state within ${POLL_TIMEOUT_SECONDS}s (last state: $final_state) -- check ${BASE_URL}/v1alpha1/runs/${run_id} manually" ;;
esac
echo "run ${run_id} reached terminal state: $final_state"

# --- report the two node outcomes ----------------------------------------
log "node outcomes"
jq -c '.node_runs[] | {node_id, state, outcome, attempts: (.attempts | map({actor_id, status}))}' <<<"$view_payload"

first_outcome=$(jq -r '.node_runs[] | select(.node_id == "codex-first") | .outcome // "(none)"' <<<"$view_payload")
second_outcome=$(jq -r '.node_runs[] | select(.node_id == "codex-second") | .outcome // "(none)"' <<<"$view_payload")
echo "codex-first outcome: $first_outcome"
echo "codex-second outcome: $second_outcome"

# --- report the two proposed ledger claims --------------------------------
log "GET /v1alpha1/runs/${run_id}/ledger -- proposed claims"
ledger_resp=$(curl -sS -w '\n%{http_code}' "${BASE_URL}/v1alpha1/runs/${run_id}/ledger")
ledger_status=$(tail -n1 <<<"$ledger_resp")
ledger_payload=$(sed '$d' <<<"$ledger_resp")
[ "$ledger_status" = "200" ] || fail "get ledger returned HTTP $ledger_status: $ledger_payload"

claim_count=$(jq '[.items[] | select(.record_type == "claim" and .authority == "proposed")] | length' <<<"$ledger_payload")
echo "proposed claim records: $claim_count"
jq -c '.items[] | select(.record_type == "claim" and .authority == "proposed") | {node_run_id, authority, origin, data}' <<<"$ledger_payload"

log "SMOKE COMPLETE"
echo "run_id=$run_id final_state=$final_state workflow_digest=$workflow_digest"
echo "codex-first outcome=$first_outcome codex-second outcome=$second_outcome proposed_claims=$claim_count"

if [ "$final_state" != "completed" ]; then
	fail "run did not complete cleanly (final_state=$final_state) -- see node outcomes and ledger output above for what actually happened"
fi
if [ "$claim_count" -lt 2 ]; then
	fail "expected 2 proposed claim records (one per actor), got $claim_count -- see ledger output above"
fi
