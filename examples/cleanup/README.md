# cleanup — remove what a merged or closed PR leaves behind

Plan `loop-closure-claude-codex` task t13 (spec claims c6/c15/c37, honesty
conditions h15/h9/h24). One deterministic code node, run through the runner
boundary, triggered by the sweep's `pr.merged` or `pr.closed` fact for a work
item. It deletes the item's landed refs, cancels its parked approval runs
with a machine-readable reason, and writes one record listing everything it
did and everything it declined to do.

Task t17 added the graph's other half: the two **stage comments** that tell
the ticket its loop closed.

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
3. **Cancel the parked runs with a reason, and only while they are still
   parked.** Each of the item's non-terminal runs is read; one whose
   `human-merges-pr` node run is still live is cancelled through
   `POST /v1alpha1/runs/{id}/cancel` with the body
   `{"reason": "pr_merged", "parked_at": "human-merges-pr"}` (or
   `pr_closed`). The control plane validates the reason against a short
   allowlist (`internal/api/cancelreason.go`) and writes it into
   `runs.reason` and the `run.cancelled` event through the
   `cancelRunGuarded` seam. The operator's bodiless cancel is unchanged
   and records no reason.

   `parked_at` is a **precondition, not a fact**, and it is what keeps this
   step honest. This node decides from the run view and acts in a second
   request, and the trigger for the whole graph — a human merging the PR — is
   the same act that advances the approval node. Without the precondition,
   the run that advances between those two requests gets cancelled *along
   with the downstream nodes the approval just made live*, work this node
   never saw and never decided about. The control plane re-checks the node
   inside the cancel transaction, under the same per-run advisory lock the
   human-decision advance itself takes (`internal/engine/humandecision.go`),
   so the two cannot interleave; a run that moved on is a `412` and nothing
   is cancelled. The guard fails closed.

   Either way the run keeps running and is listed as `left_running` with a
   `why`: `not_at_human-merges-pr` when the view already showed it elsewhere,
   `advanced_past_human-merges-pr` when the control plane refused the
   precondition. The spec scopes cancellation to parked runs, and this is how
   that scope survives a race.

   The node and the control plane ship from this repo together. A control
   plane that predates `parked_at` rejects the unknown field with a `400`, so
   pointing this node at a stale deployment is a named `cancel` failure and
   exit 2 — never a silent unconditional cancel. Check what is running with
   `curl -s $NODES_API_URL/v1alpha1/version` before blaming the node.
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

## The stage write-back (task t17, issue #311)

The graph is no longer one node. `route` at the entry decides which of the two
lifecycle facts started the run, and three agent nodes post this loop's last
two stages to the ticket through the jira actor's `post_comment` verb — never
the sweep, which has no Jira write path at all:

```text
  route ──merged──▶ stage-merged ──comment_posted──┐
      │                                            ▼
      └──closed───────────────────────────────▶ cleanup ──failed──▶ cleanup-failed
                                                   │ passed
             ┌─────────────────────────────────────┤
             │ has(issue_key)                      │ closed + Jira key
             ▼                                     ▼
      stage-cleanup                        stage-cleanup-declined
             └──────────────▶ cleaned ◀────────────┘  (and directly, for a gh: item)
```

- `merged` is posted **before** the code node, on a `pr.merged` fact only: the
  pull request landed, and nothing is claimed yet about the loose ends.
- `cleanup` is posted **after** the code node passed, on both facts: the refs,
  parked runs and per-item state are settled and the record says what happened
  to each. A **failed** cleanup posts nothing — a comment saying the loose ends
  were closed when they were not is exactly the false record this graph avoids.

Two nodes post `cleanup` and not one because a binding is a pointer and the
two facts name the work item in different fields: `pr.merged` carries the
correlated Jira key as `issue_key` (a merged PR with no ticket produces no
fact at all), while `pr.closed` carries `work_item`, which may be the
transient `gh:<owner>/<repo>#<n>` form. `has(input.issue_key)` is therefore
exactly "this is the merged fact", and a `gh:`-shaped item has no ticket to
comment on, so that run ends silently at `cleaned` — the cleanup still
happened and the record still says so.

Every `cleanup.passed` edge carries a guard on purpose. The engine picks the
first eligible edge in the **compiler's normalized order** (source, outcome,
target, guard text), not the order the file lists them, and `cleaned` sorts
before both stage nodes — an unguarded `cleaned` edge would win before any
stage guard was evaluated. The six-stage vocabulary and how the sweep reads
these comments back as a watermark are in
`docs/operations/pr-upkeep-lane.md`.

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

`tests/test_stage_write_back_graphs.py` covers the graph shape (which node
posts which stage, the guards, reachability, the schema);
`tests/e2e/stagewriteback_test.go` drives a ticket through intake → merged →
cleanup against the real engine and a fake jira bridge and checks one comment
per stage plus a silent replay. `tests/test_cleanup_node.py` runs `cleanup.py`
against a scratch bare remote
and `tests/fake_api.py`: a reachable ref is deleted, an unreachable ref is
declined and still present, another item's reachable ref is left alone, a
closed PR cancels with `pr_closed`, a remote that refuses the deletion is a
recorded failure rather than a claimed removal, and a `412` from the control
plane leaves the run running with `advanced_past_human-merges-pr` rather than
claiming a cancellation. `internal/api/cancelreason_test.go` covers the API
half: a body reason reaches the event, an unknown reason is a 400 that cancels
nothing, the bodiless operator cancel is unchanged, a matching `parked_at`
cancels, a `parked_at` the run has left is a 412 that cancels nothing and
leaves the node run live, and a terminal run stays a 409 rather than becoming
a 412.
