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
| `workspace` | the deployment precondition, measured before the first git call: `NODES_WORKSPACE` (else the job's cwd) is a git checkout and has an `origin` | `workspace_not_a_checkout` / `origin_not_configured` record naming the path, exit 2 |
| `fetch` | fetch `handover_ref` from `handover_remote`; refuse a ref outside `refs/culture-nodes/<run_id>/` | exit 4 |
| `lease` | per-target-branch lock directory under the land checkout's `.git` | `waiting`, exit 5 |
| `rebase` | skip if already on the branch (ancestor or `git cherry` equivalent); route `.github/` changes; rebase in a scratch worktree | conflict → derived routing record, exit 3, no push |
| `gate` | `land_gate.py`: the pre-push chain in the rebased worktree — target pytest (`LAND_GATE_TESTS`, default `uv run pytest -n auto -q`), `go test ./tests/lint/...`, `scripts/lint-all.sh <job>` (`LAND_GATE_JOB`, default `root`, with `LINT_ALL_SKIP=triage` — `LAND_LINT_ALL_SKIP`), the 1000-line file-length guard — then **one** version bump for the landing (`bump.py` fed a JSON changelog on stdin naming the findings; one commit on top of the handover commits, never a rewrite); toolchains `go`, `uv`, `node`, `markdownlint-cli2` are checked first | missing toolchain → `toolchain_missing` record + `refused`, exit 2, nothing ran; an `LAND_GATE_JOB` the script does not have → `lint_job_unknown` the same way; red step → routing record `gate_failed` naming the step and its tail, exit 3, no push; lint-all exit **2** → `measurement_incomplete`, but only when the script NAMED the steps it could not run (an exit 2 that names nothing linted nothing, and is red) |
| `push` | `git push` `<sha>:refs/heads/<target>`, helpers reset, `GIT_ASKPASS` from `bridge-push.env`; a rejection is classified — one re-fetch-and-rebase on a STALE one (the branch moved: non-fast-forward, fetch first, or the `cannot lock ref` compare-and-swap form) | second stale rejection → routing record, exit 3; a POLICY rejection (`[remote rejected]` from branch protection or a pre-receive hook) → `refused` naming the remote's own message, exit 4, no second round |
| `reply` | `land_reply.py`: reads the producing run's `input.findings` from the control plane; one signed reply per landed finding on its review thread (naming the landed sha and the finding id); findings with no thread share ONE PR comment; skips a reply already posted for this sha | `GITHUB_TOKEN_LAND_PR` missing → `refused` record naming it, exit 2, nothing posted |
| `resolve` | `resolveReviewThread` per landed finding's thread; skips a thread already resolved; a finding with no thread is a recorded skip | GitHub error → exit 2 |
| `checkout_lease` | per-checkout lock directory under the producing checkout's `.git` **and** `land_probe.py`'s count of the control plane's live attempts for that actor's **key** (every registration revision, over the whole paged node-run listing) | busy → `waiting`, exit 5; a probe that could not measure (a failed read, or a listing that did not end within its page bound) records `attempts_probe: unmeasured` and **also** waits — the reset does not run on an unmeasured count |
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
reasons `rebase_conflict`, `stale_after_retry`, `gate_failed` or the inherited
`out_of_workflow_scope`.

## The checkout the node runs in

The runner service registered as `runner://headspace/land` must be deployed so
its jobs start **in culture-land's checkout** on that host
(`/home/culture-land/git/culture-nodes-land`, the path `cutover.sh`'s
`LAND_HANDOVER_REMOTE` names), and that path must reach the operation as
`NODES_WORKSPACE`. The node cannot declare it: `operation.workspaceRef` is a
JSON Pointer into the run's own surfaces
(`internal/compiler/contract.go`, `checkWorkspaceRef`) and nothing upstream of
`land` produces the checkout as an artifact — the same choice
`examples/development-loop` records for its gate. Staging a copy would not do
either: a container-backed runner destroys its workspace on the way out
(`internal/runners/headspace/doc.go`), while the branch lease is a lock
directory under this `.git` that has to **outlive the job** for the next
landing to serialise on it, `origin` is the remote the push credential is
scoped to, and the gate runs `go`/`uv`/`node` off the land account's PATH.

So it is a deployment fact, and what the node can do is measure it. The
`workspace` step runs before the fetch and refuses by name —
`workspace_not_a_checkout` or `origin_not_configured`, exit 2, naming the path
— instead of failing three git calls deep with `fatal: not a git repository`.
It lives in `land_gate.py` (`check_workspace`), which owns the other host
precondition too and refuses it the same way (`toolchain_missing`); `land.py`
reaches it before the fetch rather than at the gate.

## The gate and the single bump (task t7)

`land_gate.py` is fetched beside `land.py` (`LAND_GATE_SOURCE_URL` /
`LAND_GATE_SOURCE_SHA256`) and runs the operator's pre-push chain in the
scratch worktree after the rebase. A landing of N fixes carries **exactly one**
`pyproject.toml` bump and one `CHANGELOG.md` entry listing the N findings
(`LAND_FINDINGS_JSON`, else the landed commits' subjects); a handover that
already bumps the version is landed as it is (`bump: {skipped:
handover_already_bumps}`), and a re-run of a landed ref skips the gate with
the rebase. A red step routes to a human (`gate_failed`) and pushes nothing; a
missing toolchain refuses by name before the first step (`toolchain_missing`),
which is how "Go is not on thor" reads in the ledger. An `LAND_GATE_JOB` that
`scripts/lint-all.sh` does not have is refused there too (`lint_job_unknown`):
the script answers a typo with the same exit 2 it uses for a step it could not
measure, so left to the chain an unlinted tree would read as a merely
incomplete measurement and land. The deploy reports the
same fact per binary for `culture-land`
(`deploy/prod/lanes/land-toolchain.sh`, called at the end of `deploy.sh thor`
and `deploy.sh orin`): the check itself fails naming the missing binaries, and
the call site guards it, because installing one is a counted hand-turn a
deploy cannot type.

## The checkout probe (`land_probe.py`)

Fetched beside `land.py` (`LAND_PROBE_SOURCE_URL` / `LAND_PROBE_SOURCE_SHA256`),
it answers the one question the checkout's own lock file cannot: does the
engine have a live attempt on the actor whose checkout is about to be
`reset --hard`? Three reads — the actor row behind the input's id, the actor
listing (every registration revision of that **key**, because re-registering
mints a new row id while older attempts keep the old one), and the node-run
listing walked through `next_cursor` to its end. Anything it could not
complete is `unmeasured`, never `0`: a lander that read one page, or none,
and reported "nothing is running" would be reporting a measurement it never
made. `LAND_PRODUCING_ACTOR_KEY` declares the key and saves a read;
`NODES_API_URL` unset is `not_configured` (the checkout's lock is then the
only lease).

## The lease model

Both leases are **lock directories** — `mkdir` is atomic everywhere git runs,
locally and over ssh — holding a `holder.json` (land run, pid, host). The
**branch** lease lives in the land checkout's `.git`, the one place every
lander of a deployment shares, so two landers serialise without a control-plane
round trip. The **checkout** lease lives in the producing checkout's `.git`,
reached through the same transport the fetch used (a local path, or
`ssh://user@host/path`), and is paired with the fact a file cannot know —
whether the engine has a LIVE attempt on that actor's key (`land_probe.py`
carries that half, above, including why a probe that could not MEASURE waits
rather than resets). A stale local lock (same host, pid dead) is reclaimed and
the record says so; a lock this node cannot judge is honoured. Nothing is
written to the control plane: an `agent` actor's bearer cannot write a lease
row. Waiting is non-blocking (`LAND_LEASE_WAIT_SECONDS=0`): the graph parks and
re-enters.

## What it never does

No `--force`, no force refspec, no merge API call, no PR merge, no read of the
operator's Access cookie — all grep-asserted in `tests/test_land_node.py`,
which drives the node against scratch bare remotes with no network.

`human-merges-pr` is the only merge path (c15/c38), and `land.py` itself opens
no URL — the only HTTP in the node is the siblings'. The push token comes from
the environment or `~/.culture-nodes/bridge-push.env` (`LAND_PUSH_ENV_FILE`),
never from the input and never in argv; diagnostics are redacted. This run's
identity comes from the runner boundary (`NODES_RUN_ID` / `NODES_NODE_RUN_ID` /
`NODES_ATTEMPT_ID`), the checkout from `NODES_WORKSPACE` (default cwd, and
measured before the fetch), and the routing record's producer from
`LAND_ACTOR_ID` (default `company/land`).

The workflow header documents the deployment grants (`LAND_SOURCE_URL`,
`LAND_SOURCE_SHA256`, the same pair for each of `land_gate.py`,
`land_probe.py` and `land_reply.py`, `GITHUB_TOKEN_WORKER`, `NODES_API_URL`)
and why the graph routes on exit codes through a decision node.
