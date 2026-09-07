# land — one handover ref onto its PR branch, as a code node

The operator's cherry-pick, as a graph (loop-closure task t6; issues #315
and #309 step 2). After an actor run hands over a ref under
`refs/culture-nodes/<run-id>/`, the `land` code node — `land.py`, stdlib-only,
executed by the nodes-runner under the `culture-land` engine account —
**fetches** it from the producing actor's `handover_remote`, **rebases** it
onto the PR branch tip, **pushes** it without `--force`, and **resets** the
producing actor's checkout to the new tip. It writes one JSON record per step
reached, so a partial landing is visible in the ledger and a re-run continues
instead of pushing twice.

Start it with one event (the pr-upkeep sweep or a completion hook will emit it;
today it is hand-emitted):

```bash
curl -s -X POST "$NODES_API_URL/v1alpha1/events" \
  -H "Authorization: Bearer $NODES_EVENT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
        "name": "land.handover.ready",
        "subject": "<work-item>/<run-id>",
        "payload": {
          "handover_ref": "refs/culture-nodes/<run-id>/<tail>",
          "handover_remote": "<the producing actor'"'"'s metadata.handover_remote>",
          "target_branch": "<the PR branch>",
          "work_item": "<work-item>",
          "actor_id": "<the producing actor'"'"'s id>",
          "run_id": "<run-id>"
        }
      }'
```

## Steps and records

| step | what it does | on trouble |
|---|---|---|
| `fetch` | fetch `handover_ref` from `handover_remote`; refuse a ref outside `refs/culture-nodes/<run_id>/` | exit 4 |
| `lease` | per-target-branch lock directory under the land checkout's `.git` | `waiting`, exit 5 |
| `rebase` | skip if already on the branch (ancestor or `git cherry` equivalent); route `.github/` changes; rebase in a scratch worktree | conflict → derived routing record, exit 3, no push |
| `gate` | hook point — task t7 (gate chain + single version bump) | `not_implemented` record |
| `push` | `git push` `<sha>:refs/heads/<target>`, helpers reset, `GIT_ASKPASS` from `bridge-push.env`; one re-fetch-and-rebase on a non-fast-forward rejection | second rejection → routing record, exit 3 |
| `reply` | `land_reply.py`: reads the producing run's `input.findings` from the control plane; one signed reply per landed finding on its review thread (naming the landed sha and the finding id); findings with no thread share ONE PR comment; skips a reply already posted for this sha | `GITHUB_TOKEN_LAND_PR` missing → `refused` record naming it, exit 2, nothing posted |
| `resolve` | `resolveReviewThread` per landed finding's thread; skips a thread already resolved; a finding with no thread is a recorded skip | GitHub error → exit 2 |
| `checkout_lease` | per-checkout lock directory under the producing checkout's `.git` **and** the control plane's live attempts for that actor | `waiting`, exit 5; the reset does not run |
| `reset` | `checkout -B <target> <tip>` + `reset --hard` in the producing checkout | exit 2 |

The reply and resolve steps live in the sibling `land_reply.py` (task t8), fetched
and digest-verified beside `land.py` by the bootstrap (`LAND_REPLY_SOURCE_URL` /
`LAND_REPLY_SOURCE_SHA256`). They sign as `- culture-nodes (land node)` — not the
cicd scripts' `(Claude)` signature — and use `GITHUB_TOKEN_LAND_PR` from
`~/.culture-nodes/land-pr.env` (pull-requests:write), never the push token. The
client has no merge endpoint (`tests/test_land_reply.py` greps for it): its GitHub
operations are read threads, reply, resolve, read comments, comment.

The routing record uses `internal/repair`'s shape (a `derived` `decision`
selecting `human`, `dispatched: false`) with `router: land_routing` and the
reasons `rebase_conflict`, `stale_after_retry` or the inherited
`out_of_workflow_scope`.

## What it never does

No `--force`, no force refspec, no merge API call, no PR merge, no read of the
operator's Access cookie — all grep-asserted in `tests/test_land_node.py`,
which drives the node against scratch bare remotes with no network.

The workflow header documents the deployment grants (`LAND_SOURCE_URL`,
`LAND_SOURCE_SHA256`, `GITHUB_TOKEN_WORKER`, `NODES_API_URL`) and why the
graph routes on exit codes through a decision node.
