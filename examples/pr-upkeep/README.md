# pr-upkeep — the loop that keeps this repo's own quality debt worked

Plan task t21 (spec claims c15/c26, honesty condition h21): culture-nodes
uses itself to sweep its **own** unresolved SonarCloud issues and open Qodo
PR findings, and works the resulting list one item at a time through a
human-gated fix/review cycle. Every loop iteration passes a person; the
flow can propose, fix, and review — it can never merge.

## The graph

```text
            ┌──────────────────────────────────────────────┐
            │              approved (human merged)          │
            ▼                                              │
  sweep ──passed/failed──▶ triage ──items──▶ fix ──▶ review ──▶ human-approves-pr
    ▲                        │                ▲                     │
    │                        │empty           └──changes_required───┘
    │                        ▼                                      │
    │              human-prepares-next-item             rejected ──▶ finish
    │                 │resume        │done                          ▲
    └─────────────────┘              └──────────────────────────────┘
    ▲
    └── backoff (wait 30m) ◀──sweep_broken── triage
```

- **sweep** (code node, declared intent: `network: egress-allowlist` per
  issue #50) runs [`sweep.py`](sweep.py) through the runner boundary:
  SonarCloud's issues API (unresolved issues for this repo's component key)
  plus the Qodo `Code Review` comments on this repo's open PRs, parsed into
  one prioritised work-item list. Its one routable domain fact is the **exit
  code** — a code node's persisted output is runner metadata, not stdout —
  so `0` means work found, `10` (`EXIT_EMPTY`) means a clean empty sweep,
  and anything else means the sweep itself broke.
- **triage** (decision node) reads `/nodes/sweep/output` and splits on
  `exit_code`: items → fix, clean-empty → the human park, broken → a 30m
  durable `backoff` wait, then re-sweep.
- **fix** (agent node, `company/developer` — the spark claude-code bridge)
  takes the top item, fixes it on a branch, and opens/updates a PR. Its
  bindings carry the sweep report and the sweep node's own observed
  evidence (`/nodes/sweep/evidence`), plus the ledger's decision history so
  a `changes_required` verdict from the gate reaches the next pass.
- **review** (agent node, `company/codex-thor` — a different model family
  and bridge than fix, on the independent-review pattern) is **read-only**
  (issue #18: codex sessions are analysis-only; the run-input contract pins
  `review_sandbox` to the literal `read-only`, so a writable review run is
  unpublishable). It binds the fix node's self-reported output, the fix
  node's own evidence records (`/nodes/fix/evidence` — the node-run-scoped
  surface task t7 made resolvable) and the run-wide evidence projection
  (`/ledger/projections/evidence`, task t6). Both verdicts route to the
  human gate: no agent alone re-triggers the billable fix actor.
- **human-approves-pr** (approval node, 72h deadline) declares
  `changes_required` as an explicit contract outcome next to the implied
  approved/rejected/expired, so the person can send work back to fix as a
  first-class domain outcome. `approved` loops back to sweep for the next
  item; `rejected` drops the item and ends the run; `expired` is left
  unrouted on the delivery-loop precedent.
- **human-prepares-next-item** (agent node on a `kind=human` actor,
  `company/human-ops`, via the [human-inbox bridge](../../adapters/human-inbox/))
  is the between-items park after an empty sweep: the run waits durably —
  no lease, no worker — until the person answers `resume` (sweep again) or
  `done` (end the run) through the inbox.

Acceptance blocks on sweep (`process_exit == 0`) and fix
(`workspace_diff complete`) use `enforce: observe` for this pass of issue
37: every verdict lands as a derived record; routing is untouched.

## The human-merges rule

**This flow holds zero merge credentials in the control plane.** The sweep
is read-only against two public APIs; the fix actor can push branches and
open PRs but never merge; the review actor is read-only outright; the control
plane process holds no GitHub credential at all (spec claim c13, issue #54).

**How the workflow's `approved` outcome works now:** The human merges the PR
in their own session, and a separate **merge tracker** (adapters/human-inbox,
outside the deployment) observes the merge event through the GitHub API. When
the tracker observes the PR transitioning to `merged: true`, it automatically
submits the task through the bridge's existing submit surface with an
**observed-submission claim** that names the merge commit. The engine records
this as a `data.kind: "observed-submission"` ledger entry carrying the
`collection_method: "github_pr_merged"` and the merge commit SHA — honest
attribution without claiming runner-observed authority.

**The credential boundary:** The GitHub credential (`GITHUB_TOKEN`) lives
only in the tracker process beside the human-inbox bridge, outside the
control-plane deployment. The tracker is stdlib-only and polls GitHub only
for the declared observable (the PR number). The control plane continues to
make zero GitHub API calls and holds zero GitHub credentials.

**Override lanes stay intact:** Tasks with no declared `observe` block
behave exactly as today — purely manual submit through the inbox. A person
can still submit a task manually at any time, even if an observable is
declared; the manual path is the override for dropped work and merges without
a PR. A PR closed without merging does NOT auto-complete — only the merged
state is unambiguous enough to trigger automatic submission.

## The single-repo boundary (claim c26)

This flow works on culture-nodes and nothing else. The repo is hard-coded,
not configured per run:

- `sweep.py` pins `SONAR_COMPONENT_KEY` and `GITHUB_REPO` to this repo —
  the single configuration mention of the SonarCloud component key:

  ```bash
  grep -rn "agentculture_culture-nodes" examples/pr-upkeep \
      --include='*.py' --include='*.yaml' --include='*.sh'
  # exactly one hit: sweep.py's SONAR_COMPONENT_KEY
  ```

  (The recorded fixtures under `fixtures/` also contain the key — as data
  inside recorded API payloads, which is what recorded payloads look like.)

- the `repo` run input is the local checkout path the fix/review bridges
  allowlist; a run naming a non-allowlisted repo is refused by the bridges
  themselves.

## The extractor and its fixtures

[`sweep.py`](sweep.py) is stdlib-only Python 3.12 (the reference bridges'
no-PyPI-graph constraint). Its parsing is deterministic and unit-tested in
[`tests/test_pr_upkeep_sweep.py`](../../tests/test_pr_upkeep_sweep.py)
against **recorded** fixtures:

- `fixtures/sonarcloud-issues.json` — the live issues-search response for
  this repo, recorded 2026-08-13: the four standing unresolved issues
  (1 BLOCKER `python:S3516`, 2 CRITICAL `go:S3776`, 1 MINOR `godre:S8193`)
  the t22 live run targets.
- `fixtures/qodo-pr35-code-review.body.txt` and
  `fixtures/qodo-pr42-code-review.body.txt` — the verbatim
  `Code Review by Qodo` comment bodies from PR #35 and PR #42 (stored as
  `.txt` so the repo-wide markdownlint sweep does not lint recorded HTTP
  payloads as repo documentation). Both recorded reviews carry only
  resolved/dismissed findings, so the tests exercise the open-finding path
  by stripping the `✓ Resolved` / `✗ Dismissed` / `<s>` markers from the
  same recorded bodies — which is exactly the shape of an open finding.

Run them with the normal suite:

```bash
uv run pytest tests/test_pr_upkeep_sweep.py -v
```

## Validate and run

```bash
# compile locally until clean (0 errors, 0 warnings):
go run ./cmd/nodes validate examples/pr-upkeep/workflow.yaml

# drive one cycle against a live deployment — BILLABLE, human-guarded:
CONFIRM_BILLABLE=yes examples/pr-upkeep/driver.sh
```

[`driver.sh`](driver.sh) is the external driver (scheduling stays outside
the engine by design): it validates, publishes idempotently by digest, and
POSTs **one** run to `/v1alpha1/runs` — one upkeep cycle-bundle, looping
inside the workflow under `spec.limits` (`maxTransitions: 64`,
`maxVisitsPerNode: 6`, `maxDuration: 168h`) until a terminal edge or bound
ends it. It refuses to run without `CONFIRM_BILLABLE=yes`, exactly like
`examples/codex-smoke-pair/run-smoke.sh`, because every loop iteration
dispatches a real claude-code fix session and a real codex review session.
It deliberately does **not** poll to a terminal state: the run parks on
people (gate + between-items park) for hours or days; decisions happen in
the web `/inbox` or via `POST /v1alpha1/human-tasks/{id}/decision`. When a
run ends, re-invoke the driver for the next cycle.

## Operational notes

- The sweep operation expects the extractor at `/opt/pr-upkeep/sweep.py`
  inside the pinned `python:3.12-slim` image — bake it in or bind-mount it
  when registering the runner. **Egress: DECLARED intent is `network:
  egress-allowlist` (sonarcloud.io + api.github.com only).** Headspace-cli
  0.11.0 supports only disabled/enabled network posture, so the boundary
  honestly rejects the allowlist as `rejected_input` (issue #50). The
  workflow runs with `network: full` until headspace ships allowlist support;
  restore `network: egress-allowlist` and add a runner conformance fixture
  then. No proxy workaround lands in the control plane (spec claim c29).
- `company/human-ops` is the human-inbox bridge README's registration
  example (`kind=human`, same append-only actor revisions as every agent).
- The `uses:` digests are the same referenced identities the sibling
  examples pin (`company/developer`, `company/codex-thor`,
  `runner://headspace/docker`) — shared placeholders for the same actors,
  not fresh ones.
