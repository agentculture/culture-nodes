# pr-upkeep — the loop that keeps this repo's own quality debt worked

Plan task t21 (spec claims c15/c26, honesty condition h21): culture-nodes
uses itself to sweep its **own** unresolved SonarCloud issues and open Qodo
PR findings, and works the resulting list one item at a time through a
human-gated fix/review cycle. Every loop iteration passes a person; the
flow can propose, fix, and review — it can never merge.

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
  `exit_code`: items → fix, clean-empty → `backoff` (issue #71: idle just
  re-sweeps, no human involved), broken → the SAME `backoff` wait, then
  re-sweep.
- **fix** (agent node, `company/developer` — the spark claude-code bridge)
  takes the top item, fixes it on a branch, and opens/updates a PR. Its
  bindings carry the sweep report and the sweep node's own observed
  evidence (`/nodes/sweep/evidence`), plus the ledger's decision history so
  a `changes_required` verdict reaches the next pass.
- **review** (agent node, `company/codex-thor` — a different model family
  and bridge than fix, on the independent-review pattern) is **read-only**
  (issue #18: codex sessions are analysis-only; the run-input contract pins
  `review_sandbox` to the literal `read-only`, so a writable review run is
  unpublishable). It binds the fix node's self-reported output, the fix
  node's own evidence records (`/nodes/fix/evidence` — the node-run-scoped
  surface task t7 made resolvable) and the run-wide evidence projection
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
people (the merge assignment, or the decision) for hours or days;
manual overrides happen in the web `/inbox` or via
`POST /v1alpha1/human-tasks/{id}/decision`. When a run ends, re-invoke the
driver for the next cycle — issue #71 means that is rarer now, since an
empty sweep re-sweeps on its own instead of ending the run.

**Known gap, pre-dating issue #71:** `nodes validate` currently reports
errors on `human-merges-pr` AND `human-answers-review`'s `observe:`
bindings — both bind a NESTED object (`{kind: ..., pr: ...}`) where
`schemas/workflow/workflow.schema.json`'s `inputBinding.bindings` requires
every value to be a single JSON-Pointer STRING. This is not new: the
identical shape already existed on `human-merges-pr` before this pass (task
t16) and was verified present on the pre-#71 workflow.yaml too. Every OTHER
part of the redesigned graph compiles clean — swapping both `observe:`
blocks for a placeholder pointer during a local sanity check produces
`valid: pr-upkeep 1.0.0 (0 errors, 0 warnings)`. Closing this gap needs a
compiler/schema change (e.g. a typed literal-with-embedded-pointer binding
shape) outside issue #71/#72's scope; it affects the ALREADY-SHIPPED merge
observable exactly as much as the new reply observable, so it is worth its
own issue rather than a silent workaround here.

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
  `company/notify-discord` — the last from `examples/notify-message`,
  `runner://headspace/docker`) — shared placeholders for the same actors,
  not fresh ones.
