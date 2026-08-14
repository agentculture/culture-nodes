# pr-upkeep — the loop that keeps this repo's own quality debt worked

Plan task t21 (spec claims c15/c26, honesty condition h21): culture-nodes
uses itself to sweep its **own** unresolved SonarCloud issues, open Qodo
PR findings and failed CI check runs, and works the resulting list one item
at a time through a human-gated fix/review cycle. Every loop iteration
passes a person; the flow can propose, fix, and review — it can never
merge.

## Deployment configuration

Everything this example needs from the world outside its graph, and where
each value comes from. Loading it into a deployment that is not this one
means supplying these — it never means editing `workflow.yaml`.

| Value | Where it comes from |
| --- | --- |
| `repo`, `review_repo` | **Run input.** Two per-host working directories: `repo` is where the fix actor works, `review_repo` is the review actor's own allowlisted checkout on its own machine. Neither is the work under review — that crosses as `handoff` (see "The cross-machine handoff"). |
| `fix_instruction`, `review_instruction`, `ask_instruction`, `await_reply_instruction`, `merge_instruction`, `notify_title`, `notify_description`, `review_sandbox` | **Run input.** The words each actor is given, authored per run; [`driver.sh`](driver.sh) carries this deployment's defaults and every one is overridable by an environment variable it documents. |
| `actor://company/developer` | **Actor registry.** The fix lane, and the only identity here holding a GitHub write credential. |
| `actor://company/codex-thor` | **Actor registry.** The review lane — deliberately a different backend *and* host from the fix lane. The `thor` in the id is a registry key naming a role, not a hostname you must own: `internal/worker/registry.go` resolves the identity against your actors table (with the `@sha256` revision suffix stripped), so you register the same id against your own endpoint. |
| `actor://company/human-ops` | **Actor registry.** The `kind=human` inbox bridge. |
| `actor://company/notify-discord` | **Actor registry.** The notify adapter (issue #68). |
| `runner://headspace/docker` | **Runner registry.** The code-node runner boundary the sweep dispatches through. |
| `PR_UPKEEP_SWEEP_SOURCE_URL` | **Granted environment value** on the sweep operation. Where `sweep.py` is fetched from at dispatch time. |
| `PR_UPKEEP_SWEEP_SOURCE_SHA256` | **Granted environment value.** The sha256 those fetched bytes must have; the bootstrap refuses to execute anything else. |
| `PR_UPKEEP_MAX_PRS_PER_SWEEP`, `PR_UPKEEP_REQUIRED_CHECKS`, `GITHUB_TOKEN` | **Process environment of the sweep**, all optional except the token's effect on rate limits. Documented at their constants in `sweep.py`. |
| `SONAR_COMPONENT_KEY`, `GITHUB_REPO` | **Pinned in `sweep.py`, on purpose** — see below. This is *the one value a new operator changes*, and they change it in their own copy of the script. |

The granted environment values are the ones a reader cannot trace from the
document alone: they resolve in the worker process that dispatches the
operation, and the runner boundary refuses the operation **by name** when
one is unset (`internal/runners/headspace/bridge.go`'s `resolveEnv`). That
is why they are named in the workflow's own header as well as here.

Until task t16 the sweep's script came from a `raw.githubusercontent` URL
pinned to one org, one commit and one path. A third party who loaded this
example got a graph that silently fetched and executed *our* bytes — a
supply-chain property, not a portability wart. Fork `sweep.py`, publish your
copy, and grant its URL and digest:

```bash
sha256sum examples/pr-upkeep/sweep.py   # the value for PR_UPKEEP_SWEEP_SOURCE_SHA256
```

Both values are resolved by the **runner** process, from its own
environment — see [`deploy/prod/README.md`](../../deploy/prod/README.md)'s
"Granted environment values" for where they live on this deployment and how
`deploy.sh` re-grants them.

A digest mismatch, or either value unset, exits nonzero — which `triage`
reads as `sweep_broken` and routes to the backoff wait, like any other
broken sweep. The `0` / `10` / other exit-code contract is untouched.

## The graph

Issue #71 removed the between-items human park: idle is no longer a human
task, and a genuine judgement (a review that came back `changes_required`)
gets a real decision instead of a second inbox. See "Idle vs blocked"
below for why.

```text
                    ┌────────────────────────────────────────────┐
                    │         merged (human merged the PR)         │
                    ▼                                              │
  sweep ──passed/failed──▶ triage ──items──▶ fix ──▶ review ──approve──▶ human-merges-pr
    ▲                        │                ▲                              │dropped
    │                        │empty           │answered                      ▼
    │                        ▼                │                           finish
    │                     backoff       human-answers-review ◀─sent─ notify-decision-pending
    │                        │           │merged      │dropped              ▲
    │                        │           ▼            ▼                    │posted
    └────────────────────────┘        sweep         finish        ask-pr-question
    ▲                                                                       ▲
    └── backoff (wait 30m) ◀──sweep_broken── triage         review.changes_required ──┘

  fix ──handoff_unavailable──▶ handoff-blocked   (issue #74: the fix host has
                                                  no portable handle to hand
                                                  the review host; the run
                                                  ends naming the capability)
```

- **sweep** (code node, declared intent: `network: egress-allowlist` per
  issue #50) runs [`sweep.py`](sweep.py) through the runner boundary:
  SonarCloud's issues API (unresolved issues for this repo's component key),
  the Qodo `Code Review` comments on this repo's open PRs, and the **failed
  CI check runs** on those PRs' head commits (issue #61), parsed into one
  prioritised work-item list. Its one routable domain fact is the **exit
  code** — a code node's persisted output is runner metadata, not stdout —
  so `0` means work found, `10` (`EXIT_EMPTY`) means a clean empty sweep,
  and anything else means the sweep itself broke.
- **triage** (decision node) reads `/nodes/sweep/output` and splits on
  `exit_code`: items → fix, clean-empty → `backoff` (issue #71: idle just
  re-sweeps, no human involved), broken → the SAME `backoff` wait, then
  re-sweep.
- **fix** (agent node, `company/developer` — the spark claude-code bridge)
  takes the top item, fixes it on a branch, and opens/updates a PR. Its
  bindings carry the sweep report and the sweep node's own observed
  evidence (`/nodes/sweep/evidence`), plus the ledger's decision history so
  a `changes_required` verdict reaches the next pass. It must also publish
  its work as a **portable handle** — see [the cross-machine
  handoff](#the-cross-machine-handoff-issue-74) — and has a named way to
  say it cannot: the `handoff_unavailable` outcome.
- **review** (agent node, `company/codex-thor` — a different model family
  and bridge than fix, on the independent-review pattern) is **read-only**
  (issue #18: codex sessions are analysis-only; the run-input contract pins
  `review_sandbox` to the literal `read-only`, so a writable review run is
  unpublishable). It reads the work under review through the fix lane's
  `handoff` handle, never through a path; its own `review_repo` is only the
  working directory thor's bridge allowlists. It also binds the fix node's
  self-reported output, the fix node's own evidence records
  (`/nodes/fix/evidence` — the node-run-scoped surface task t7 made
  resolvable) and the run-wide evidence projection
  (`/ledger/projections/evidence`, task t6). Both verdicts reach a human:
  `approve` becomes an active merge assignment, `changes_required` becomes
  a genuine decision — no agent alone re-triggers the billable fix actor.
- **ask-pr-question** (agent node, reusing the FIX actor
  `company/developer` — the only actor in this flow with an established
  GitHub write credential) posts the review's findings as a comment on the
  PR fix opened, when review comes back `changes_required`. The question
  goes where the review conversation already lives.
- **notify-decision-pending** (agent node, `company/notify-discord` —
  [adapters/notify](../../adapters/notify/), issue #68) announces to
  Discord that a decision is pending. The message is deliberately static:
  the notification says a decision is waiting, the PR holds the substance.
- **human-answers-review** (agent node on the `kind=human` actor
  `company/human-ops`, via the [human-inbox bridge](../../adapters/human-inbox/))
  parks on `observe: {kind: github_pr_reply}` — the tracker's decision
  observable (issue #71), read alongside `github_pr_merged` below. Three
  outcomes, two of them auto-observed: `answered` (a qualifying reply
  landed — back to `fix` for another pass), `merged` (the human merged the
  PR instead of replying — the strongest possible answer, back to `sweep`
  like `human-merges-pr.merged`), and `dropped` (the PR closed unmerged
  while the question sat unread, auto-observed so a question on a dead PR
  never waits forever, or a manual override — ends the run).
- **human-merges-pr** (agent node, `company/human-ops`) is the active merge
  assignment for a clean `approve` verdict: `merged` (auto-observed via
  `github_pr_merged`) loops back to `sweep`; `dropped` (manual only) ends
  the run.

Acceptance blocks on sweep (`process_exit == 0`) and fix
(`workspace_diff complete`) use `enforce: observe` for this pass of issue
37: every verdict lands as a derived record; routing is untouched.

## Idle vs blocked (issue #71)

`human-prepares-next-item` used to serve two different states through one
generic "resume/done" park: **idle** ("nothing to do right now") and
**blocked** ("I need a person to judge something"). Conflating them meant
an idle loop and a blocked loop looked identical, and neither could be
woken by a newly arrived PR — issue #72 (the tracker deployed on a
different host than the actor it watches) was the prerequisite that made
this observable in production. The fix removes the node outright and
treats the two states as genuinely different:

- **Idle** stops being a human task. `sweep` is now both the loop's entry
  AND its resting place: an empty sweep returns to `backoff` (the SAME
  30-minute durable wait a broken sweep already used) and re-sweeps.
  Nobody is "assigned" an idle repository.
- **Blocked** gets a real decision, built from three ordinary steps rather
  than a generic park: `ask-pr-question` posts the question to the PR,
  `notify-decision-pending` tells Discord a decision is pending, and
  `human-answers-review` waits for the PR reply as an observable — exactly
  the shape `human-merges-pr` already used for the merge. Every human
  interaction in this graph is now a real-world artifact the tracker
  observes (a merge, a reply, or a PR closing), not a task parked in a
  second inbox.

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
a PR. A PR closed without merging does NOT auto-complete `human-merges-pr` —
only the merged state is unambiguous enough to trigger automatic submission
there.

## The decision observable (issue #71)

`human-answers-review` parks on a SECOND observation kind the same tracker
now understands, `github_pr_reply` — read
[adapters/human-inbox's README](../../adapters/human-inbox/README.md) for
the full contract. Three points worth restating here, in this flow's own
terms:

- **Which reply counts.** The rule is deliberately structural, not
  content-based: a comment counts when it is posted strictly AFTER
  `ask-pr-question`'s own comment (GitHub's own `since` filter on the
  comments API, scoped to the task's own park time) by an author who is
  not one of the flow's own automated identities (the Qodo review bot by
  default; extend via `HUMAN_INBOX_TRACKER_REPLY_IGNORED_LOGINS`). No
  magic marker is required — the question was JUST posted on this PR, so
  the next human comment on the thread IS the answer in context. This is
  what keeps the flow from resuming on an unrelated "thanks": an unrelated
  aside would have to be posted by a real person, strictly after the
  question, on this specific PR — in practice only the person answering
  does that.
- **The PR's terminal states are observables too.** A question posted to a
  PR that then gets merged or closed does not wait forever:
  `human-answers-review` checks the PR's own state on every cycle before
  checking for a reply, so `merged: true` completes the task as `merged`
  (loops to `sweep`, the strongest possible answer) and `state: closed`
  (unmerged) completes it as `dropped` (ends the run) — neither needs a
  human to notice and manually intervene.
- **The rate budget.** `github_pr_reply` shares the SAME per-cycle GitHub
  request budget as `github_pr_merged` — it does not raise
  `github_request_budget` or shorten `poll_seconds`. In the anonymous
  lane's worst case (no `GITHUB_TOKEN`, default 50% utilization) the
  tracker plans one GitHub request per 120-second cycle, 30 requests/hour
  against the 60/hour ceiling, REGARDLESS of how many PRs are being
  watched — adding reply-kind groups only adds more entrants to the same
  round-robin queue. A reply-kind group's full check costs up to TWO of
  those per-cycle requests when the PR is still open (one for terminal
  state, one for new comments) versus one for a merge-kind group, so at
  budget=1 an open reply-kind group needs at least two cycles (up to
  ~240s) to complete one full check — slower detection than a merge check,
  an explicit and accepted trade-off for staying inside the same ceiling,
  never a silent overrun. Reply-kind groups are checked BEFORE merge-kind
  groups each cycle (a human is actively blocked on a reply-kind group;
  a merge-kind group's human can act at their own pace), which reprioritises
  the same fixed budget without growing it.

## The single-repo boundary (claim c26)

This flow works on culture-nodes and nothing else. The repo is hard-coded,
not configured per run — and that is the point rather than an oversight. It
is a **blast-radius boundary**: `fetch_open_pulls` walks *every* open PR on
the repo and reads each one's comments and check runs, so a repo taken from
run input would aim that enumeration wherever a caller pointed it, on a
credential the script did not choose. Pinned in code, re-pointing it is an
edit someone makes deliberately in a fork.

So it stays pinned, and it is **the one value a new operator changes** to
run this example against their own repo: edit the two constants in your
copy of `sweep.py` — the copy `PR_UPKEEP_SWEEP_SOURCE_URL` points at — and
nothing else. Both halves are stated at the constants themselves, and
`tests/test_pr_upkeep_sweep.py`'s `TestTheSweptRepoIsPinnedAndSaysSo`
asserts that the pin is real (plain literals, no environment value able to
re-point it) *and* that this note still exists.

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

## The cross-machine handoff (issue #74)

`fix` and `review` are deliberately different actors — that is the whole
independent-review pattern — and different actor increasingly means
**different machine**: `company/developer` is the spark claude-code bridge,
`company/codex-thor` is the codex bridge on thor. A filesystem path does not
survive that boundary, and this graph used to hand one across it anyway.

What it looked like, in run `01KZZSGSWH11J7R7P4V2HPTZZQ`:

```text
sweep   completed  passed
triage  completed  items
fix     completed  completed     <- real session on spark, committed b01608c
review  failed     auth_or_policy (HTTP 403): actor answered Forbidden
```

The bridge was right. It was handed `/home/spark/git/.worktrees.culture-nodes/upkeep-fix`,
which is outside thor's allowlist and does not exist on thor at all. The
problem is that the error names **authorization** when the cause is
**topology** — which points a reader at credentials and away from the actual
defect.

Three things changed, and the invariants are locked by
[`tests/lint/crosshosthandoff_test.go`](../../tests/lint/crosshosthandoff_test.go):

1. **`fix.completed` requires a handle.** Its contract requires
   `handoff: {kind: artifact, ref: "artifact://<namespace>/<id>"}`, and the
   ref is pattern-constrained so it cannot quietly become a path again. An
   artifact reference "never carries or implies a filesystem path"
   ([`internal/artifacts/doc.go`](../../internal/artifacts/doc.go)) — it
   resolves through the store from any host. Because the engine validates a
   completion against the outcome schema
   ([`internal/engine/complete.go`](../../internal/engine/complete.go)'s
   `checkOutput`), a fix that produced no handle **cannot report
   `completed`**. This is enforced, not advised.
2. **A fix host that cannot produce one says so, by name.** The
   `handoff_unavailable` domain outcome requires a `missing_capability` from
   a closed set — `artifact_publish`, `workspace_export`,
   `handoff_too_large` — so the answer is a name, not a sentence to
   interpret.
3. **That outcome never reaches `review`.** It routes to the terminal
   `handoff-blocked` node, which carries the fix node's output (where
   `missing_capability` lives) as the run's output. `finish` would have
   buried it under the sweep report.

**Why not a git ref**, which is [issue #74](https://github.com/agentculture/culture-nodes/issues/74)'s
own recommendation and would reuse task t25's preserve-branch machinery: a
probe of the spark bridge host settled it. `origin` is HTTPS not SSH, no SSH
key is authorised for GitHub, there is no `credential.helper`, and the
running bridge process carries neither `GH_TOKEN` nor `GITHUB_TOKEN`. The
host that must produce the handle cannot push. `handoff.kind` is an enum
with one member today so that adding `git_ref` later — once a bridge host
holds a credential — is visibly additive.

**What is not wired yet.** [`internal/artifacts`](../../internal/artifacts/)
is a complete library (Store, Router, Postgres and S3 drivers, migrations
0004/0006) with **zero production callers**. There is no artifact ingest or
fetch endpoint on the control plane's HTTP surface, no bridge publishes
bytes, and `InvocationResult.artifact_refs` is accepted on the wire and then
dropped. So today every fix host lands on `handoff_unavailable` with
`missing_capability: artifact_publish`. That is the true state of the
system, and a far better answer than reviewing thor's own checkout and
calling it a review of spark's work. Remaining for the content path: an
ingest/fetch endpoint, bridge-side publish and resolve across all backends
(the all-backends rule), and persisting actor-reported artifact refs so
`/nodes/<id>/artifacts` can become a bindable surface — it is refused today
by both [`internal/compiler/contract.go`](../../internal/compiler/contract.go)
and [`internal/worker/bindings.go`](../../internal/worker/bindings.go), and
those two verdicts must change together.

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
- `fixtures/github-check-runs-pr60.json` — the live check-runs response for
  PR #60's head commit `67672519`, recorded 2026-08-14. This is the exact
  payload issue #61 was filed on: a red `lint` job the two-source sweep
  could not see, beside a **passing** `SonarCloud Code Analysis` check.
- `fixtures/github-check-runs-pr60-sonar-gate-failed.json` — the same
  recording with the SonarCloud check's quality gate flipped to failed, so
  one payload carries a failed Sonar-named check **and** a failed non-Sonar
  check and the skip below is provable. It is derived rather than recorded
  because culture-nodes has never had a red SonarCloud check run in its
  recorded history (checked across the last 60 commits on main on
  2026-08-14: two failing check runs, both `github-actions`). Exactly three
  fields differ from the verbatim recording — `conclusion`, `output.title`,
  `output.summary` on that one check run — and a test asserts that rather
  than leaving the provenance as prose.

### The check-runs source (issue #61)

A red CI check is neither a SonarCloud issue nor a Qodo review body, so the
two-source sweep reported *nothing* for PR #60 while its `lint` job sat
red. The third source reads
`GET /repos/{repo}/commits/{head_sha}/check-runs` for each swept PR — the
same `MAX_PRS_PER_SWEEP`-capped set the other two per-PR queries use, one
more request per PR, no new host and no new credential. Three decisions
worth stating here:

- **Sonar-named checks are skipped** (matched on the check name *and* the
  `sonarqubecloud` app slug, so a renamed check is still caught). A red
  quality gate is not separate work from the issues that made it red, and
  those already arrive through the Sonar feed carrying a rule, a file and a
  line — where the check run carries only "the gate failed".
- **Severity maps onto the existing ladder**, not a fourth vocabulary: a
  failed required check takes `CRITICAL` (rank 1, the HIGH/CRITICAL band),
  a failed optional one takes `MEDIUM` (rank 2).
- **Required-ness is declared, not read.** The check-runs API does not
  report it, and `GET /repos/{repo}/branches/main/protection` answers 404
  *Branch not protected* for this repo (checked 2026-08-14), so
  `REQUIRED_CHECKS` names the three merge gates CLAUDE.md documents
  (`test`, `lint`, `version-check`). Override with
  `PR_UPKEEP_REQUIRED_CHECKS`; when branch protection does land, that
  declared list should be replaced by the protection API's own answer.

Only *completed* check runs with a `failure`, `timed_out` or
`action_required` conclusion become work. `cancelled` deliberately does
not: a cancelled run is a superseded or interrupted job, not a finding.

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
people (the merge assignment, or the decision) for hours or days;
manual overrides happen in the web `/inbox` or via
`POST /v1alpha1/human-tasks/{id}/decision`. When a run ends, re-invoke the
driver for the next cycle — issue #71 means that is rarer now, since an
empty sweep re-sweeps on its own instead of ending the run.

### How the observable is declared (issue #73, closed)

Both `human-merges-pr` and `human-answers-review` declare what they are
waiting for **in the graph text**, as a typed literal binding:

```yaml
input:
  bindings:
    instruction: /run/input/merge_instruction
    pr: /nodes/fix/output/pr_number
    observe:
      literal:
        kind: github_pr_merged
```

The split is the point. A binding value is either a JSON Pointer (a read
from run, node, or ledger data) or a `literal:` (a constant the author
wrote), and the two are never confused because a bare string is always a
pointer. The observation **kind** is a declaration and never changes, so it
is a literal; **which PR** is per-cycle data produced by the `fix` node, so
it is a pointer. A pointer cannot be smuggled inside a literal — that would
be the template language PRD §11.2 forbids — so the tracker reads `pr` from
the `observe` block when it is there and from the task's own input
otherwise, the same fallback it has always applied to `repo`.

This is the shape the convention was always documented as having. Until
issue #73 landed the compiler refused it, and the workflow shipped the
observable as a whole object riding run input instead — which compiled, but
meant an author read the graph and could not see what the node watched. Two
guards keep the documented shape and the compilable shape the same from now
on: `scripts/validate-examples.sh` and `tests/lint/examplescompile_test.go`
(see [docs/invariants.md](../../docs/invariants.md), invariant 3).

## Operational notes

- The sweep operation **fetches** the extractor at dispatch time from the
  URL its deployment grants and verifies its sha256 before executing it (see
  "Deployment configuration"); the pinned `python:3.12-slim` image needs
  nothing baked in, since the script is stdlib-only. **Egress: DECLARED
  intent is `network:
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
  `company/notify-discord` — the last from `examples/notify-message`,
  `runner://headspace/docker`) — shared placeholders for the same actors,
  not fresh ones.
