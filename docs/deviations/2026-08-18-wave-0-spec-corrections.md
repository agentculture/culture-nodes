# Deviation record — wave 0 of `jira-driven-idea-to-shipped-loop` (2026-08-18)

Three deviations surfaced while executing wave 0 of
`docs/plans/2026-08-18-jira-driven-idea-to-shipped-loop.md`. All three are
recorded as proposed deviations on the plan's frame (`devague deviate --list`:
d1–d3) and await human confirmation; none was folded silently into drift.

## d1 — t10 partial harvest: the sweep's Jira read path survives

codex-orin's t10 delivery (run `01M09R58Z4ND6Y81B4299KX5AF`) built the
requested comment-only Jira actor bridge — and also **removed the sweep's
entire Jira read path** (`fetch_jira_issues`, Basic-auth plumbing, rate-limit
constant, their tests), rewrote `install-secrets.sh` to delete the runner's
secret file on deploy, and edited `nodes-runner.service`. That was a literal
reading of spec boundary c5's "the sweep … holds only the event-ingress
token" — wording this spec over-compressed from #118's sweep description.
The #76/#106 design deliberately places the backlog *read* in the sweep;
only *write* authority is actor-shaped (#68).

**Resolution at staging**: the bridge, its tests, the additive `deploy_jira`
lane and the custody audit were kept; the read-path removal, secret-file
deletion, service edit, README rewrite and per-package version bump were
rejected. The custody audit's sweep half now forbids a **write-shaped
endpoint** (`/rest/api/3/issue/`) instead of the read pair. Frame boundary
c5 is amended accordingly (pending human re-confirmation).

## d2 — spec claim c13 is half-true: the artifact carrier is one-way

Measured at HEAD during t18: `POST /v1alpha1/attempts/{attemptID}/artifacts`
is the only artifact route — there is **no GET** (pinned by
`TestArtifactSurfaceExposesOnlyAuthenticatedPost`), and the shipped runner
forwards no credential a container could use to fetch. So assumption c13
("frame state travelling as an artifact … is now possible rather than
aspirational") holds for ingest and **not for retrieval**, and decision
c25's frame-as-artifact travel is not yet implementable for consumers.
Claim c13 is amended (pending re-confirmation); the read side is filed as a
Feature issue.

## d3 — spec claim c6's "human-inbox approval nodes" is a non-existent composition

An `approval` node parks a `human_tasks` row answered via
`GET /v1alpha1/pending-decisions` + `POST /v1alpha1/human-tasks/{id}/decision`;
`adapters/human-inbox` is a §13 **actor** bridge serving **agent** nodes.
No wire connects the two (grep of `adapters/` for
`human_tasks`/`pending-decisions`: zero matches). t18's committed
`examples/spec-chain/workflow.yaml` uses the engine's own approval surface
and says so in its header. Claim c6 is amended to name the engine surface
(pending re-confirmation).

## Also fixed in-flight (not deviations, logged for the hand-turn count)

- t9's merge pushed `tests/test_pr_upkeep_sweep.py` over the 1000-line hard
  limit; caught by the combination gate, repaired by splitting the Jira
  tests into `tests/test_pr_upkeep_sweep_jira.py`.
- thor's t1 delivery duplicated the `git()` test helper that already exists
  in `internal/handover/handover_test.go` (its checkout predates it);
  deduplicated at staging.
- `adapters/jira` shipped without the byte-identical `dialin.py` the
  transport guard demands of every bridge; copied verbatim from
  `adapters/notify` at staging.
