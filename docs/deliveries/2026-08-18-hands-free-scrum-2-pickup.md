# Delivery: hands-free SCRUM-2 pickup

Plan: `docs/plans/2026-08-18-hands-free-scrum-2-pickup.md` (10 tasks, 6
waves). Spec: `docs/specs/2026-08-18-hands-free-scrum-2-pickup.md`. Branch:
`scrum2/hands-free-pickup`, PR #190. All evidence ids below resolve by plain
GET against thor's v1alpha1 API at write-up time (h4/h11).

## The headline

**The loop closed.** Run `01M0B7ETAGQ2MS0YDCZP9762WX` (2026-08-18T20:01:59Z,
`completed`): a sweep tick read SCRUM-2's transition to To Do, emitted
`pr-upkeep.jira.transitioned.to-do`, the published `jira-intake` workflow
minted a run, claude-intake drafted the intake comment, the Jira bridge
posted it (comment `10071`, marker-stamped), and the bridge's allowlisted
transition moved the board to In Progress — run output
`{"issue": "SCRUM-2", "target": "In Progress"}`, ledger carrying the
actor's proposed completion claim. No human step between the arming
transition and the board move.

The run before it (`01M0B6WGCQNSQD4TBY5J5HBG61`, 19:51Z) had already landed
comment `10070` and moved the board; only the final node's outcome-name
mismatch kept its record red. The one before that
(`01M0B5QWGSG2F86P0NC76AJT8V`, 19:31Z) drafted honestly and died on the
completion contract; the first (`01M0B4K8JSY3QRQJ3M7D2WTZDC`, 19:11Z) died
at dispatch on a stale bridge process. Each failure named its cause in the
run record and was fixed forward within minutes — that ladder is itself the
delivery: the system made its own failures legible enough to debug from the
API alone.

## Planned versus actual, per task

| Task | Planned | Actual |
|---|---|---|
| t1 | #188 claim-path fix | Done — authored by **codex-thor** via live `workspace-write --handover` dispatch (run `01M0AS8PZ76T520BW1TBX6DGPW`, handover commit `31183b1`); one test-pumping fix at the merge gate. With t2, the first live proof of #18's write path. |
| t2 | Jira transition verb + allowlist | Done — authored by **codex-orin** (run `01M0AS95G4GY0WE4WVC787Y2WQ`, handover `51bf39e`), merged unmodified; audit test narrowed, custody decision committed (`docs/decisions/2026-08-18-jira-transition-custody.md`). |
| t3 | #189 stdout capture | Done — local subagent (one crash-resume); zero diff to the emitter (h9). |
| t4 | intake workflow | Done — authored in-session; evolved v1.0.0 → 1.1.0 → 1.1.1 → 1.2.0 against live contract findings (below). |
| t5 | deploy + register Jira bridge | Done — secrets installed (sweep pair reuse, operator-approved), `company/jira-comment` registered rev 1, bridge active on thor. |
| t6 | redeploy workers, 4+ green sweeps | Done — both workers on branch revisions; **7 consecutive green scheduled sweeps** (19:31:51–20:01:51Z) after the last config fix, claim lottery gone. |
| t7 | publish + read back | Done — published early (safe: watermark-armed trigger), IR read back with both guards + ceiling. |
| t8 | live hands-free measurement | Done — the headline run above, after three diagnosed-and-fixed attempts. |
| t9 | drop staged guard | Done — v1.2.0 (registry v4) live, guard now `source == "jira"` only. Observed: the first open-guard sweep (20:06:51Z) ran green and minted **zero** runs — the predicted no-stampede behavior (watermark dedup; no fresh transitions), bounded further by the subject-run ceiling. |
| t10 | this document + dispositions | Done — see closures. |

## Deviations

- **d1 (approved)**: #189 needed the production path, not just the bridge:
  the runner service uploads captured stdout via its per-operation callback
  token (`internal/runners/artifactclient`), the API grew the GET read-back
  routes, then two more live-found lifecycle fixes — runner attempts resolve
  via the **parked** invocation (audit rows only exist post-completion), and
  migration 0041 dropped the timing-hostile `artifacts.attempt_id` FK.
  Proven: `{"sweep":"pr-upkeep","emitted":2}` answered by one GET against
  the emitting sweep's attempt.
- **Wave-order overlap** (unrecorded at the time, recorded here): t5's
  secrets/registration ran while t3 was still building — its dependency (t2)
  was merged, so the plan's graph was honored even though the wave boundary
  was not.
- **Live contract reconciliations** (v1.1.0/v1.1.1): the claude bridge
  reports `input.success_outcome` + `output.summary` (it does not parse
  outcomes from model text), and the Jira bridge's transition outcome is
  `issue_transitioned`. Both found by real runs, both now encoded in the
  workflow document with the measuring run cited.

## Hand-turns, all counted (h6)

Counted on #187 (comment) and #191, plus this tally:

1. Jira bridge env files + bearer on thor; `NODES_ACTOR_JIRA_TOKEN` into
   thor's `prod.env` (t5).
2. `register-actor.sh company/jira-comment` (t5).
3. Branch push so the sweep-source raw URL resolves.
4. `deploy.sh thor` ×4 and `deploy.sh orin` ×2 (t5/t6/fix redeploys) — the
   first thor/orin pair also run independently by the operator.
5. `runner.env` grant restores ×3 (#191, bitten three times: NODES_API_URL,
   Jira repositories config; plus the container-DNS fix `thor` → LAN IP).
6. Jira token synced thor → orin `prod.env` (post `policy_denied`).
7. Schedule swap to 5-minute cadence (operator-directed, via the API).
8. Intake bridge restart on spark (stale resident process).
9. SCRUM-2 status flips: operator ×3 early, then scripted via the new
   first-party `jira-status` skill (operator-directed) — the arming action
   the spec allows, now itself automated.

## Runbook (h17) — kill switch, verbatim

Disable pickup entirely (only the newest workflow version triggers):

```bash
# 1. copy examples/jira-intake/workflow.yaml, DELETE the `triggers:` block,
#    bump metadata.version, then:
bash .claude/skills/nodes-operator/scripts/nodes-op.sh publish <file>
# stop one in-flight run:
curl -X POST http://thor:18080/v1alpha1/runs/<run-id>/cancel
```

Required hosts (h19/c23): **thor** (control plane, workers, jira bridge,
runner), **orin** (second worker), **spark** (claude-intake push bridge —
a spark outage stalls the intake leg visibly).

## Success signals (c18/h15), each checked

- System-authored comment on SCRUM-2 without a human posting: **checked** —
  comments `10070`, `10071`.
- Board transition not made by a human: **checked** — To Do → In Progress by
  the bridge's allowlisted verb, twice (19:51Z, 20:01Z).
- Prod run id resolving with trigger + ledger claims: **checked** —
  `01M0B7ETAGQ2MS0YDCZP9762WX`, ledger claim `proposed`.
- Sweeps stop failing on the claim lottery: **checked** — 7 consecutive
  green scheduled runs post-fix; the #188 fix pinned by
  `internal/worker/claimskip_test.go`.
- Green sweep answers "emitted?" in one query: **checked** —
  `GET /v1alpha1/attempts/<att>/artifacts/stdout` →
  `{"sweep":"pr-upkeep","emitted":2}`.

## What this cycle also surfaced (follow-ups, all filed + dispositioned)

- #191 deploy.sh clobbers hand-granted sweep env (bitten ×3, mitigation in
  place).
- #192 store of working flows (operator direction).
- #193 history-aware sweep — kills the between-polls blindness and the
  arming dance's dependence on poll timing.
- #194 bounded re-mint when a trigger-created run fails technically — kills
  the dance entirely, with #193.
- Audit classification hint for `NODES_ACTOR_JIRA_TOKEN` (deploy warning).
- Sweep cadence is now 300s by schedule row `01M0B29RF8ZTBHF9NQWB0MZNP1`
  (the 1800s row `01M0A8RK4Z53KDYKYQQBATFX97` is disabled);
  `sweep-cycle.workflow.yaml`'s header still names 1800s — left untouched
  by this cycle's h9 zero-diff pin; the next cycle that edits that file
  should refresh the comment.

## Issue dispositions

- **#187** — closed on this document + the headline run id.
- **#188** — fix merged (t1) and proven by the 7-green streak; close on
  read.
- **#189** — capture + upload + read-back live-proven; close on read.
- **#18** — write path proven (comment already posted with both runs/refs).
- **#118** — the "no subscriber for Jira facts" gap is gone; the loop's
  remaining gaps are #193/#194-shaped, not joint-shaped.
