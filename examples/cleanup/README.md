# cleanup — remove what a merged or closed PR leaves behind

Plan `loop-closure-claude-codex` task t13 (spec claims c6/c15/c37, honesty
conditions h15/h9/h24). One deterministic code node, run through the runner
boundary, triggered by the sweep's `pr.merged` or `pr.closed` fact for a work
item. It deletes the item's landed refs, cancels its parked approval runs
with a machine-readable reason, and writes one record listing everything it
did and everything it declined to do.

Measured before it existed: PR #307 left 8 `review-fix/*` branches and 26
parked `human-merges-pr` runs behind after its merge, all removed by hand.

## What one run does

1. **Attribute refs to the item.** `GET /v1alpha1/runs?work_item=KEY` (task
   t1) gives the item's run ids. A preserve branch is minted from the run id
   (`adapters/*/preserve.py` `mint_branch_name`) and a handover ref lives
   under `refs/culture-nodes/<run-id>/…` (`mint_handover_ref`), so a ref under
   either namespace is the item's when one of those ids is a path component
   of the handover ref or appears in the branch name. Anything else under the
   two namespaces is listed as `unattributed` and left alone.
2. **Delete only what main already has.** `git merge-base --is-ancestor`
   against the default branch decides. A reachable ref is deleted on the
   remote and the remote is re-read to confirm. An unreachable ref is the
   only copy of that work: it is listed as `declined: unreachable` and never
   touched. On a `pr.closed` fact nothing was merged, so every ref is declined
   and the record still names them (h24).
3. **Cancel the parked runs with a reason.** Each of the item's non-terminal
   runs is read; one whose `human-merges-pr` node run is still live is
   cancelled through `POST /v1alpha1/runs/{id}/cancel` with the body
   `{"reason": "pr_merged"}` or `{"reason": "pr_closed"}`. The control plane
   validates the reason against a short allowlist
   (`internal/api/cancelreason.go`) and writes it into `runs.reason` and the
   `run.cancelled` event through the existing `cancelRunWithReason` seam. The
   operator's bodiless cancel is unchanged and records no reason. A live run
   that is NOT parked at the approval node is listed as `left_running`, not
   cancelled — the spec scopes cancellation to parked runs.
4. **Per-item sweep state.** The pr-upkeep sweep keeps none outside the
   control plane: its idempotency is the signal watermark row per
   `source_key` plus the run-input walk, and a watermark is an immutable
   fact. The record says `dropped: []` with that note rather than claiming a
   drop.
5. **One record.** A single JSON line on stdout (the runner stores stdout as
   the attempt's artifact, referenced from the observed evidence record) with
   `deleted`, `declined`, `unattributed`, `cancelled_runs`, `skipped_runs`,
   `left_running`, `sweep_state` and `failures`. Exit 0 when every step
   succeeded (declines and skips are successes); exit 2 when an environment
   step failed — the record still lists the failure by step, and the graph
   routes to `cleanup-failed`.

## Deployment configuration

Everything the graph needs from outside the file is named in the
`Deployment configuration` block at the top of `workflow.yaml`
(`tests/lint` enforces that every registry id and granted environment value
appears there). Two honest notes:

- `GITHUB_TOKEN_WORKER` is the culture-land account's `bridge-push.env`
  credential (task t5). It is read only by a temporary `GIT_ASKPASS` script,
  with the configured credential helpers reset on the push, because a
  configured helper silently outranks `GIT_ASKPASS` on this fleet.
- `POST /v1alpha1/runs/{id}/cancel` is an administrator-role route on an
  Access-fronted listener (`internal/api/principal.go`). Whether the runner
  principal is admitted to it is a deployment policy decision this example
  does not make; `NODES_API_TOKEN` / `NODES_API_COOKIE` are the seams.

## Tests

`tests/test_cleanup_node.py` runs `cleanup.py` against a scratch bare remote
and `tests/fake_api.py`: a reachable ref is deleted, an unreachable ref is
declined and still present, another item's reachable ref is left alone, a
closed PR cancels with `pr_closed`, a remote that refuses the deletion is a
recorded failure rather than a claimed removal. `internal/api/cancelreason_test.go`
covers the API half: a body reason reaches the event, an unknown reason is a
400 that cancels nothing, the bodiless operator cancel is unchanged.
