# Delivery Summary — the easy-pickings batch

plan: `easy-pickings-batch` · run: `complete` · date: `2026-08-17`
spec: `docs/specs/2026-08-17-easy-pickings-batch.md`
plan: `docs/plans/2026-08-17-easy-pickings-batch.md`

## Intent

Clear the issues whose fix was **already written down in the issue itself** —
work that needed no triage, because triage had already happened and was
committed in `docs/triage/open-issues.md`. The selection rule was that an
issue's disposition must name an *action*, not a decision.

Ten were selected. Twelve closed. The difference is the point of this record.

## What actually shipped

| PR | Issues | What |
|---|---|---|
| [#158](https://github.com/agentculture/culture-nodes/pull/158) | #153 #148 #152 | merge-gate: order-independent aggregate, unknown-key refusal, a matrix that does not read `.venv` |
| [#159](https://github.com/agentculture/culture-nodes/pull/159) | #146 #123 | a baseline that survives a failed probe; one script that runs every lint job CI runs |
| this PR | #131 #150 #134 #94 | doctor's fourth check named; the four inbound dial-in routes specified; the credential relay contained; git-metadata writability reported |
| — | #66 #151 | closed as **verify-then-close**: already shipped, no code written |

**Ten fixed, two found already fixed.** Those are different facts and this
record does not present them as one.

## The finding that changed the batch

The `/challenge` pass — run rigorous, because the batch hit four escalation
signals (security, destructive operations, hard-to-reverse, CI-gate integrity)
— **falsified two confirmed claims before any code was written**:

- **#66 was already implemented, including its test.**
  `internal/notifier/rundetail_test.go:222` asserts the exact string the issue
  requested, `"parallel-live-proof (8d4c768)"`.
- **#151 was already largely implemented.** The runbook ordering
  (`deploy/prod/README.md:14-16`), the idempotent add-if-absent issuance install
  (`install-secrets.sh:436-455`), and the re-issue script
  (`issue-dialin-credential.sh`) all existed.

Both had been selected by reading the issue text and the triage disposition.
Neither had been checked against `HEAD`.

That produced the batch's one real process change: **an issue enters a work
batch only after its defect is reproduced at HEAD.** A third candidate, #50, was
caught by that rule before it entered — its deviation record already existed.

The rule paid for itself again in execution. Reproducing #146 showed the issue's
*second* claim was false: an unreachable host exits **255**, not 0, across four
invocation shapes. But a *worse* exit-0 path existed that the issue had not
described — a probe that **answers with nothing** truncates the baseline and
reports success. A fix written to the issue as filed would have checked exit
status and left the real defect live.

## Honest limits on what is claimed

- **Production still runs the pre-fix merge gate.** The pinned matrix in
  `examples/merge-gate/workflow.yaml` is part of the published workflow's
  content digest by design, so #152's and #148's fixes do not reach the gate
  running on thor until that workflow is republished. Republishing is a
  deployment step this batch did not take. The source of truth is fixed; the
  running gate is not.
- **#134 is contained, not eliminated.** The test harness no longer relays live
  operator credentials, but `deploy/prod/install-secrets.sh:554` still relays
  `DISCORD_WEBHOOK_URL` from whatever environment invokes it. A probe run
  outside the harness reproduces the original near-miss exactly.
- **#94's codex lane reports `not-probed`, deliberately.** The bridge process is
  not inside the confinement its sessions get, so reporting `supported` would
  have stated the opposite of what a `workspace-write` dispatch receives — in
  the exact field #94 exists to fix. A `git_probe=` seam takes a confined probe
  the day one exists.
- **Five of the eight original fixes were verified by reading, not by
  reproducing**, at selection time. Reproduction happened during execution (t1),
  which is where #146's discrepancy surfaced.

## Boundary held (t16)

No command in this batch wrote to thor or orin, applied a migration, restarted a
bridge, or republished a workflow. The single dispatch — the #117 audit — ran
`--sandbox read-only`. Everything else is verifiable by the repo's own CI, which
is what made "easy" mean something more than "small".

## What was deliberately not taken

- **The structural half of #152** — refusing a command whose argv contains a
  git-untracked path, or gating against a clean export of the commit. A change
  to the gate's contract; stays open.
- **#116** — the issue says "Observed, not diagnosed" in its own words. It needs
  an investigation before it needs a patch.
- **#125, #129, #133, #135, #119, #140** — decisions wearing a bug's clothes.
- **#136, #128, #121** — small-sounding, but each needs a live production touch.
- **#50** — considered and rejected: `docs/deviations/2026-08-15-headspace-egress-allowlist.md`
  already exists, so it would have been a third already-done pick.

## Findings routed elsewhere rather than fixed here

- **#117's audit found a real security defect.** `internal/api/grades.go:66-99`
  takes `grading_actor_id` from the request body and derives the record's origin
  and authority from the *named* actor's registered kind — with no
  authentication on that route. Naming any registered human yields a
  human-origin, `confirmed` ledger record. Routed to
  [#6](https://github.com/agentculture/culture-nodes/issues/6) (OIDC / workload
  authentication), because the fix is an authentication story, not a guard
  bolted onto one handler. No POST was sent; this is a code-read finding with
  strong evidence, not a demonstrated exploit.
- **Two dial-in defects** found while specifying the routes — a 500 for three
  caller-caused conditions (so a bridge retries a permanently-invalid id
  forever), and `authenticateInbound` hardcoding party kind to `actor` — are
  recorded on [#111](https://github.com/agentculture/culture-nodes/issues/111).
- **Issue types** proposed as [#157](https://github.com/agentculture/culture-nodes/issues/157):
  the org already defines `Task`/`Bug`/`Feature` and **56 of 56** open issues
  carried none, so the backlog cannot distinguish work from a record. #66 and
  #151 are the evidence.

## Deviations

- **d1 (approved)** — PR3 and PR4 merged as one pull request, so the batch
  shipped three PRs rather than the four `c24` confirmed. This satisfies the
  reasons behind the constraint rather than breaking them: `c31` required
  sequential merges because concurrent PRs collide on `pyproject.toml` and
  `CHANGELOG.md`, and one PR cannot collide with itself; `c30` required PR1
  first because it changed the gate program, and PR1 merged first.

## Operator hand-turns (the count that matters)

Per the standing rule that manual work stays countable:

1. Reproducing all eight defects at HEAD (t1) — operator lane.
2. Adding #157's triage disposition after filing it turned CI red repo-wide.
3. Regenerating the triage table three times, after each merge auto-closed
   issues.
4. Rebasing three work-package branches after each squash merge dropped their
   ancestry.
5. Re-running two CI jobs that failed on infrastructure (a `setup-go` 429 and a
   GitHub 503).

Items 2, 3 and 5 are the same underlying coupling: **the issue tracker and a
flaky API are inputs to CI on every open PR.** Recorded on #157 with options,
and this PR fixes the flake half — `triage-report.py` retries, and returns 2
("could not measure") rather than 1 ("the table is wrong") when GitHub cannot be
read at all; `lint-all.sh` routes an exit-2 step to `UNRUNNABLE`.

## Method note

The work was fanned out to five parallel agents in isolated worktrees, grouped
into **file-disjoint work packages** rather than one agent per plan task — two
tasks that both rewrite `scripts/merge-gate.py` belong to one agent, and the
dependency graph guarantees logical independence within a wave, never file
disjointness.

**Every one of the five agents contradicted its brief on something real, and
each was right**: `black --check scripts` fails at HEAD for pre-existing
reasons; `pytest tests/lint/` collects zero items because that directory is Go;
four adapters ship `preflight.py`, not five; #131's premise understated the
drift by one check; and #146's exit-0 claim is false while a worse one is true.
