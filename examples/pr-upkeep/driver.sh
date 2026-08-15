#!/usr/bin/env bash
#
# ============================================================================
#  !!! LIVE-ONLY, BILLABLE, MANUAL DRIVER -- NEVER RUN IN CI !!!
# ============================================================================
#
# External driver for examples/pr-upkeep/workflow.yaml: validates the
# workflow, publishes it (idempotent by digest), and POSTs ONE run to
# /v1alpha1/runs. One run is one upkeep cycle-bundle: the loop edges INSIDE
# the workflow (human-merges-pr.merged -> sweep, human-answers-review.
# answered -> fix, human-answers-review.merged -> sweep, backoff.completed
# -> sweep) carry it from item to item until a terminal edge or a
# spec.limits bound ends it — then a human re-invokes this driver for the
# next cycle. The driver is deliberately external (spec decision: ad-hoc
# scheduling stays outside the engine); it holds no merge credential and
# neither does the run it creates.
#
# issue #71: idle no longer parks on a person (an empty sweep returns to
# `backoff` and re-sweeps in 30m on its own), so this driver's job is
# simpler than it used to be — it only needs re-invoking after a TERMINAL
# run (a dropped item or an explicit end), not after every empty sweep.
#
# issue #74: the fix and review actors are on DIFFERENT machines, so the fix
# lane hands over a portable artifact handle rather than a path. The artifact
# content path is not wired yet (see the workflow's header), so a run today
# is expected to end at `handoff-blocked` with `missing_capability:
# artifact_publish` — a named capability gap, which is the honest answer and
# what this driver's operator should read on the terminal run. It is NOT a
# reason to re-point REVIEW_REPO at a spark path: that is the defect #74 is
# about, and thor's bridge will refuse it with a 403 that names the wrong
# cause.
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
# Deployment prerequisite this driver cannot supply (task t16): the sweep
# node names its script source as a granted environment value, and those are
# resolved by the RUNNER process, not by the run. So
# PR_UPKEEP_SWEEP_SOURCE_URL and PR_UPKEEP_SWEEP_SOURCE_SHA256 must already
# be in the runner's environment (deploy/prod/README.md, "Granted
# environment values"); unset, the sweep is refused by name at the boundary
# and this driver's run ends in `sweep_broken` -> backoff.
#
# Env overrides (all optional):
#   NODES_API_URL          default: http://thor:18080
#   PR_UPKEEP_REPO         default: /home/spark/git/culture-nodes
#                          (must be allowlisted by the fix/review bridges)
#   FIX_INSTRUCTION        override the fix node's instruction
#   REVIEW_INSTRUCTION     override the review node's instruction
#   ASK_INSTRUCTION        override ask-pr-question's instruction (issue #71)
#   NOTIFY_TITLE           override notify-decision-pending's Discord title
#   NOTIFY_DESCRIPTION     override notify-decision-pending's Discord body
#   AWAIT_REPLY_INSTRUCTION override human-answers-review's park instruction
#   MERGE_INSTRUCTION      override the merge task's instruction
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW_FILE="$SCRIPT_DIR/workflow.yaml"

BASE_URL="${NODES_API_URL:-http://thor:18080}"
REPO="${PR_UPKEEP_REPO:-/home/spark/git/culture-nodes}"
# The review actor (codex on thor) has its own allowlisted checkout of the
# repository selected by the deployment grant — a distinct per-host path.
REVIEW_REPO="${PR_UPKEEP_REVIEW_REPO:-/home/thor/git/culture-nodes-agent}"
# The fix instruction now has to state the HANDOFF contract (issue #74), for
# two reasons that both come back to the same fact: the review actor is on
# another machine. First, `fix.completed` requires `handoff.ref`, so the
# session must declare its own result JSON (the bridges'
# `declared_result_override`) rather than let the default envelope through —
# a default envelope carries no handle and would be contract_rejected at this
# node. Second, a session that CANNOT publish an artifact needs the named way
# out spelled for it, or it will improvise: invent a ref, or report completed
# and leave the review lane to fail on another host as somebody else's 403.
FIX_INSTRUCTION="${FIX_INSTRUCTION:-Take the TOP item of the prioritised sweep report bound as sweepReport (its artifact refs carry the full JSON list). Work only that one item: implement the fix on a branch and open or update a PR for it. Never merge anything. The review actor runs on a DIFFERENT machine and cannot see this working directory, so your work has to leave here as a portable handle: publish your change (a patch or bundle) to the artifact store and report the artifact reference it returns. Your FINAL message must be EXACTLY one JSON object and nothing else: {\"outcome\": \"completed\", \"output\": {\"summary\": string, \"pr_number\": integer, \"handoff\": {\"kind\": \"artifact\", \"ref\": \"artifact://<namespace>/<id>\", \"media_type\": string}}}. If you CANNOT publish an artifact from this host — no reachable ingest endpoint, no credential, or nothing exportable — do NOT report completed and do NOT invent a ref. Report exactly {\"outcome\": \"handoff_unavailable\", \"output\": {\"summary\": string, \"missing_capability\": one of \"artifact_publish\", \"workspace_export\", \"handoff_too_large\", \"detail\": string}} and name what is missing in detail. A named missing capability is a useful answer; a fabricated handle is not.}"
REVIEW_INSTRUCTION="${REVIEW_INSTRUCTION:-Read-only independent review of the fix described in fixReport (see the Bound inputs block for handoff, fixReport, fixEvidence, and runEvidence). The work under review is the artifact named by handoff.ref — your own checkout is a working directory on a DIFFERENT machine than the fix lane and does NOT contain the fix, so never review it in place of the handoff. Resolve handoff.ref through the artifact store and review its contents. If you cannot resolve it, FAIL the attempt saying so; do not fall back to your own checkout and do not turn an unresolvable handle into a verdict about the fix. fixEvidence being empty is a KNOWN platform gap (evidence capture for agent nodes), not a finding. Analysis only: change nothing. Your FINAL message must be EXACTLY one JSON object and nothing else: {\"outcome\": \"approve\" or \"changes_required\", \"output\": {\"verdict\": same value as outcome, \"findings\": [{\"severity\": \"info\"|\"minor\"|\"major\"|\"blocking\", \"note\": string}]}} — findings may be empty only with approve; approve requires verdict approve and findings [].}"
MERGE_INSTRUCTION="${MERGE_INSTRUCTION:-The fix PR passed independent review with no pending findings (see reviewVerdict and fixReport in your task payload). Merge the PR yourself on GitHub, then submit outcome merged with a note naming the merge commit. If you decide not to merge, submit outcome dropped with the reason.}"
ASK_INSTRUCTION="${ASK_INSTRUCTION:-The independent review (see reviewVerdict) found blocking or major issues on the PR named in your task payload. Post the review findings as a comment on that PR — this is the question a human needs to answer — then submit outcome posted naming the comment URL.}"
NOTIFY_TITLE="${NOTIFY_TITLE:-pr-upkeep: decision pending}"
NOTIFY_DESCRIPTION="${NOTIFY_DESCRIPTION:-A pr-upkeep review left questions on a PR and is waiting for a reply there. See the PR thread for the substance.}"
AWAIT_REPLY_INSTRUCTION="${AWAIT_REPLY_INSTRUCTION:-A review question is posted on the PR named in your task payload (see questionPosted). This task completes itself automatically once you reply on the PR, merge it, or close it — no manual submission needed unless you want to drop the item outright (submit outcome dropped).}"

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
log "fix checkout=$REPO; review checkout=$REVIEW_REPO (both must match the repository selected by the deployment grant)"

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
	--arg review_repo "$REVIEW_REPO" \
	--arg fix_instruction "$FIX_INSTRUCTION" \
	--arg review_instruction "$REVIEW_INSTRUCTION" \
	--arg ask_instruction "$ASK_INSTRUCTION" \
	--arg notify_title "$NOTIFY_TITLE" \
	--arg notify_description "$NOTIFY_DESCRIPTION" \
	--arg await_reply_instruction "$AWAIT_REPLY_INSTRUCTION" \
	--arg merge_instruction "$MERGE_INSTRUCTION" \
	'{repo: $repo,
	  review_repo: $review_repo,
	  fix_instruction: $fix_instruction,
	  review_instruction: $review_instruction,
	  review_sandbox: "read-only",
	  ask_instruction: $ask_instruction,
	  notify_title: $notify_title,
	  notify_description: $notify_description,
	  await_reply_instruction: $await_reply_instruction,
	  merge_instruction: $merge_instruction}')
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
echo "humans:  merge-pr and answer-review tasks complete themselves when you act on GitHub"
echo "         (merge the PR, or reply/merge/close on a PR with a pending question) — no"
echo "         inbox visit required; manual override stays available via the web /inbox or"
echo "         POST /v1alpha1/human-tasks/{id}/decision if you want to drop an item outright."
echo "remember: this flow cannot merge — human-merges-pr's 'merged' outcome means YOU merged"
echo "          the PR yourself first."
echo "next cycle: re-run this driver after the run reaches a terminal state."
