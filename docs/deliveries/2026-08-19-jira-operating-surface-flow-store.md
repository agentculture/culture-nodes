# Delivery Summary — Jira operating surface + flow store

plan: `jira-operating-surface-flow-store` · run: `partial` · date: `2026-08-19`
(summary written 2026-08-29)
baseline: `devague summary skeleton`
(`devague summary --plan jira-operating-surface-flow-store`)

## Intent

> Jira is a peer operating surface for culture-nodes: the sweep replays issue
> history faithfully instead of sampling point-in-time state, a
> technically-failed pickup re-arms bounded by the control plane, and proven
> flows live in a browsable store users pull into their own control plane and
> extend on their own machine/db

The run executed
`docs/plans/2026-08-19-jira-operating-surface-flow-store.md` (13 tasks, 5
waves, spec merged in PR #201) through the culture-nodes actor fleet itself —
dispatch log on issue #203, work merged in PR #208 (0.39.0) with one follow-up
in PR #209 (0.39.1). This summary is written ten days after the merge, because
the cycle closed its PR without closing its loop: no delivery record existed,
and the two undelivered tasks (`t10`, `t13`) sat in the PR body's "Not in this
PR" rather than anywhere countable.

**The single fact that governs every claim below:** the prod control plane
(`GET /v1alpha1/version` at the prod API on 2026-08-29) reports revision
`c041f28`, a pre-squash commit reachable only from
`origin/scrum2/hands-free-pickup`. Everything this cycle merged is on `main`
and **nothing this cycle merged is deployed**. Every "proven on the live prod
lane" honesty condition in the spec is therefore unmet, and every claim here is
a *merged* claim, not a *running* one.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — Sweep emits history-position facts: one fact per unseen changelog entry and per unseen comment, in order, watermarked by changelog id / comment id
- `t2` — Discovery keeps a resolved issue eligible until its terminal history is consumed: bounded recently-resolved lookback in `fetch_jira_issues` JQL
- `t3` — Watermark cutover migration: seed every known issue's `signal_event_watermarks` row at its current history head so the first history-aware pass replays nothing
- `t4` — Transition self-echo discipline: changelog entries carry an author, and the system's own board moves never become trigger-firing facts
- `t5` — Control-plane re-mint of technically-failed trigger-created runs: backoff, N-attempts-per-window bound, derived ledger record naming the original event and attempt count, then park on a human
- `t6` — Re-mints enter the same EnqueueWork seam as triggered and manual runs: subject ceiling, pacing, and breaker gates bound them identically
- `t7` — Store entry model and private registry API: a catalog entry is graph digest PLUS evidence manifest (proving prod run ids, deviations recorded against it, required actor/runner capabilities), full fidelity, internal server
- `t8` — Store pull with actor mapping: the import binds the entry's declared actor/runner capability requirements to local registrations; the graph document stays byte-identical
- `t9` — Board-parity consumers: a human's bare comment on a tracked ticket has a consumer, operator questions round-trip on the board, and ticket creation gets a lane verb
- `t10` — Board-driven planning continuation: the spec-chain leg is reachable from the ticket — frame decisions land as marked questions, the human's reply transacts exactly the stated decision, plan and assignment continue from the board
- `t11` — Start/finish ticket reports: a run minted from a ticket-derived fact posts an engine-driven start update (run id, workflow, trigger event id) and a finish update (terminal outcome) through the narrow jira bridge, never the emitter
- `t12` — Regression-proof suite for the cited live failures: every failure named in the spec's why-it-matters maps to a named test
- `t13` — Live prod proof cycle, ticket-to-store, measured from prod records alone: the whole announcement demonstrated on the real lane with zero operator shell commands

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered (merged, undeployed) | History replay in `examples/pr-upkeep/pr_upkeep_jira.py` (split out of `sweep.py` under `d1`): one fact per unseen changelog entry / comment, watermarked by history position; non-status changelog items advance the watermark without firing (#193 comment 2026-08-19T17:47Z). WP-A, codex-orin run `01M0DMFX9F09P0FQ48VNY9SMC7`. |
| `t2` | delivered (merged, undeployed) | Bounded recently-resolved lookback in the discovery JQL; `TestJiraCredentialIsActorOnly` deliberately narrowed at the gate to pin the read shape (`6e4cc2d` on the integration branch). |
| `t3` | delivered (merged, undeployed) | `migrations/0041_jira_history_watermark_cutover.sql` + `internal/jiracutover/` (adopt-don't-emit); deploy-order contract in `migrations/README.md`: 0041→0045, adoption-aware control plane, and history-aware emitter ship as ONE unit (risk r4). WP-D, codex-thor. Gate caught a NULL `source_key` that would have 500'd every non-Jira event delivery. |
| `t4` | delivered (merged, undeployed) | accountId-exact transition self-echo; SCRUM-3 changelog entry 10180 replay fixture proves the intake flow's own board move re-fires nothing. WP-E, codex-orin run `01M0DYPYBEM26CP4VTZAC0Z6HB`. |
| `t5` | delivered (merged, undeployed) | `internal/engine/remint.go` + `internal/store/postgres/remint.go`: backoff, 2 attempts per 24 h window, derived record naming the original event and attempt count, then a human task; `RemintSchedulerActorID` on the t32 repair-router pattern after the gate found an empty `origin.actor_id` + actors FK defect pair. WP-B, codex-thor run `01M0DMG4N7TGX05C27TMHRMC2E`. |
| `t6` | delivered (merged, undeployed) | Re-mints enter `EnqueueWork`; #202's breaker attach point documented at `Store.ScheduleRunRemint`. |
| `t7` | delivered (merged, undeployed) | `migrations/0042_store_entries.sql`, `internal/store/postgres/storeentries.go`, `internal/api/storeentries.go`: insert-only entries, origin-split identity, `entry_digest` stable across planes, four registry routes. WP-C, developer run `01M0DR8MCJKTPG7S65Y6915YAG` (first attempt `01M0DMG4RT96VREYMBE7NG0W58` `policy_denied` on an allowlist mismatch — the #204 evidence). |
| `t8` | delivered (merged, undeployed) | Store pull with actor-mapping **bindings** (`migrations/0044`, `0045`, `internal/api/storebindings.go`); graph document hash-compared byte-identical before and after. WP-F, developer run `01M0E0DTPC1YANW9DQKXHNGGWK`. PR #209 closed the newest-entry-wins hole (entries sharing a ref must agree, 409 by name). |
| `t9` | delivered (merged, undeployed) | `examples/jira-comment-consumer/` (bare human comments get a consumer), `adapters/jira/src/jira_bridge/create_issue.py` behind an exact-match project allowlist, shared `question_correlation` helper. WP-T9, developer run `01M0DYPYFTWRWF3AXH24PD9FXF` — harvested despite a **false** `policy_denied` (#207). **Not published to prod**: the prod workflow registry lists no `jira-comment-consumer` key. |
| `t10` | **dropped** | Not dispatched. Gated on plan risk r2 (which lane may mutate `.devague/` from an engine-driven run); no decision record was ever written. Now decided on frame `jira-flow-spec-read-related-bugs` q1 (2026-08-29): a minimal custody declaration on the existing `owe-developer` lane, not waiting on #204. Carried by #199 (custody decision commented there 2026-08-29) and #230. |
| `t11` | delivered (merged, undeployed) | `migrations/0043_jira_ticket_report_outbox.sql`, `internal/store/postgres/jiraticketreport.go`, `internal/ticketreport/dispatcher.go`: engine-driven start/finish updates through the narrow jira bridge outbox. WP-D, codex-thor. |
| `t12` | delivered | `docs/audits/2026-08-20-flow-store-regression-map.md` maps every live failure the spec cites to a named test, including a real fix for the s14 quoting-suppression case. WP-G, codex-orin run `01M0E0DTV9ZXR8AX8CXGXWYEF4`. |
| `t13` | **blocked** | Never run. Depends on `t3`, `t6`, `t8`, `t10`, `t11`, `t12` *deployed*; the r4 deploy unit never shipped (prod at `c041f28`), `t10` is dropped, and #191 (`deploy.sh` un-grants `NODES_API_URL` and the Jira half of `PR_UPKEEP_REPOSITORIES` on every deploy) still reproduces on `main`, so the deploy that would unblock it is itself unsafe until #191 is fixed. |

Task count: 13 planned — 11 delivered (10 merged-undeployed + `t12`), 1
dropped (`t10`), 1 blocked (`t13`).

## Mid-work Decisions

- `d1` — sweep.py crossed the 1000-line hard limit (1149) at the WP-A merge gate: the t4 file-length contract collides with the t16 single-script bootstrap, so the sweep splits into sweep.py + a Jira history module and sweep-cycle's bootstrap fetches N declared (URL, SHA-256) source pairs instead of one; rides the r4 single deploy unit; adds one codex session beyond the approved split plan — WP-A's spec-required history replay (+183 lines) pushed an already-966-line file over a hard limit that has no exemption mechanism, and the file deploys as one fetched script so a plain module split breaks the bootstrap. (Recorded via `/deviate`, approved by the operator — re-affirmed 2026-08-29.)
- WP-C was re-dispatched onto the `upkeep-lane` worktree after the developer bridge's `repo_allowlist` refused the assign verb's default `owe-developer` path — config-vs-script drift invisible to the dispatcher; became the motivating evidence on #204 (hand-turn 2 on #203). No deviation record covers this; captured here.
- The developer-bridge allowlist was widened to include `owe-developer` and the bridge restarted mid-cycle (operator-approved, hand-turn 4 on #203).
- WP-T9's `policy_denied` was overruled at the gate and its commits harvested after the committed diff was verified clean of `.github/` paths — the bridge had measured the step-0 base move as session changes. Filed as #207. No deviation record; captured here.
- WP-F's worktree was pre-synced by the operator to dodge #207 (hand-turn 5 on #203).
- The t3 gate found and fixed a production-breaking defect the brief never named: a non-Jira trigger event's NULL `source_key` 500'd every `/v1alpha1/events` delivery (COALESCE fix, WP-D gate `c9db7aa`).
- `t10` was left undispatched rather than deviated: the plan's own risk r2 said the custody story "must be declared before the spec-chain leg dispatches", and it never was. This is the cycle's un-recorded scope cut — the reason this summary exists.
- Five counted hand-turns are logged on #203 (checkout-sync delegated into briefs; allowlist routed around; #205 noticed; allowlist widened + restart; worktree pre-sync).

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t1` (`d1`) | WP-A's spec-required history replay (+183 lines) pushed an already-966-line file over a hard limit that has no exemption mechanism, and the file deploys as one fetched script so a plain module split breaks the bootstrap | needs-follow-up |
| `t9` | Delivered to `main` but never published to the prod workflow registry; the spec's "bare human comment has a consumer" holds in the tree and not on the board. No record covers this. | needs-follow-up |
| `t10` | Not dispatched — custody decision (risk r2) was never made during the cycle; no deviation was recorded for leaving it out | needs-follow-up |
| `t13` | Never run — the r4 deploy unit it depends on did not ship; prod remains at `c041f28`. The plan's honesty conditions ("each leg proven on the live prod lane") are unmet | risky |
| all of `t1`–`t8`, `t11` | Merged but not deployed; the plan assumed one r4 deploy closing the cycle, and the cycle ended at the PR merge instead. Ten days later the prod db has never seen migrations 0041(cutover)–0045. | risky |

## Evidence

Read-only checks run on 2026-08-29 against `main` (`449b679`):

- tests: `tests/test_pr_upkeep_sweep_jira.py` + `tests/test_question_correlation.py` — 38 passed (`uv run pytest -q -n auto`)
- tests: `go test ./internal/engine/ -run Remint` — ok
- tests: `go test ./internal/api/ -run StoreEntr` — ok
- tests: `go test ./internal/jiracutover/` — ok (`internal/ticketreport/` has no test files)
- files present: `examples/pr-upkeep/pr_upkeep_jira.py`, `examples/jira-comment-consumer/`, `adapters/jira/src/jira_bridge/create_issue.py`, `internal/engine/remint.go`, `internal/store/postgres/{remint,storeentries,jiraticketreport}.go`, `internal/api/{storeentries,storebindings}.go`, `internal/jiracutover/`, `internal/ticketreport/dispatcher.go`, `migrations/0041_jira_history_watermark_cutover.sql`, `0042_store_entries.sql`, `0043_jira_ticket_report_outbox.sql`, `0044_store_entry_bindings.sql`, `0045_store_entry_bindings_resolution_idx.sql`, `docs/audits/2026-08-20-flow-store-regression-map.md`
- commits: squash `c0f6c4a` (PR #208, 81 files, +8328/−294), `bd81319` (PR #209, 12 files)
- prod control plane: `GET /v1alpha1/version` → revision `c041f28` (`git branch -a --contains c041f28` → `remotes/origin/scrum2/hands-free-pickup` only)
- prod workflow registry (`GET /v1alpha1/workflows`, 2026-08-29): `pr-upkeep-sweep-cycle` v1, `pr-upkeep` v11, `jira-intake` v4 — no `jira-comment-consumer`, no spec-chain
- PRs / issues: #201 (spec), #203 (dispatch log), #208, #209; #192 #193 #194 #197 #198 (features, open pending `t13`); #199 #200 (`t10` legs); #202 (verified healed on prod as pr-upkeep v11); #204 #205 #207 (filed from the cycle); #191 (blocks the deploy)

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| The sweep emits one fact per unseen changelog entry and comment, watermarked by history position, and the code is on `main` | high | `examples/pr-upkeep/pr_upkeep_jira.py` · `tests/test_pr_upkeep_sweep_jira.py` (pass) · commit `c0f6c4a` |
| The watermark cutover adopts without emitting, and the deploy-order contract is written | high | `migrations/0041_jira_history_watermark_cutover.sql` · `internal/jiracutover/` (go test ok) · `migrations/README.md` |
| A technically-failed trigger-created run re-mints (2 per 24 h, derived record, then a human task) and domain outcomes never re-mint | high | `internal/engine/remint.go` · `go test ./internal/engine/ -run Remint` ok · `internal/store/postgres/remint.go` |
| Store entries carry graph digest + evidence manifest; pull binds capabilities to local registrations with the graph byte-identical | high | `internal/api/storeentries.go`, `storebindings.go` (go test ok) · migrations 0042/0044/0045 · PR #209 |
| Bare human comments have a consumer and ticket creation has an allowlisted lane verb | medium | `examples/jira-comment-consumer/` · `adapters/jira/src/jira_bridge/create_issue.py` · `tests/test_question_correlation.py` (pass) — merged only, not published to prod |
| Runs minted from ticket facts post start/finish updates through the jira bridge outbox | medium | `migrations/0043_jira_ticket_report_outbox.sql` · `internal/ticketreport/dispatcher.go` — no test files in that package; not deployed |
| Every live failure the spec cites maps to a named test | high | `docs/audits/2026-08-20-flow-store-regression-map.md` |
| The board-driven planning continuation is reachable from a ticket (`t10`) | unverified | not built — not claimed done |
| The whole announcement runs ticket-to-store on prod with zero operator shell commands (`t13`) | unverified | never run; prod at `c041f28` — not claimed done |
| A two-comment reply on a real ticket emits two facts in order **on prod** (spec honesty condition) | unverified | the history-aware emitter is not deployed — not claimed done |
| A flow proven here publishes into a second control plane and runs (spec h17/c19) | unverified | no second control plane exists (plan risk r2, second bullet) — not claimed done |

## Remaining Work / Follow-up

- **Fix #191 first** — `deploy/prod/deploy.sh:88–97` rewrites `runner.env` without `NODES_API_URL`, and `PR_UPKEEP_REPOSITORIES` defaults jira-less; until fixed, the r4 deploy amputates the sweep's Jira source or fail-closes it. Owner: next cycle (frame `jira-flow-spec-read-related-bugs`, claim c5).
- **Reconcile the prod checkouts, then ship the r4 deploy unit** — both prod checkouts are detached with hand-modified tracked files (`docs/deliveries/2026-08-27-qwen-bridge-first-dispatch.md`); migrations 0041(cutover)–0045 + both sweep source pairs + the adoption-aware control plane deploy as one unit per `migrations/README.md`. Also publish `jira-comment-consumer` and `pr-upkeep-sweep-cycle` v2 (two-pair bootstrap). Owner: operator lane; every hand-turn commented on #230.
- **`t13` — the live prod proof** — after the deploy: two-comment reply → two facts in order; a killed pickup re-mints bounded and parks; a start/finish report on a real ticket; measured from prod records alone. Closes #192 #193 #194 #197 #198 on evidence. Tracked by #230.
- **`t10` — board-driven planning** — custody decided (q1, 2026-08-29: minimal declaration on `owe-developer`); build the declaration, publish `examples/spec-chain`, wire it to a ticket fact. Carried by #199; composes with #200 (the spec as an HTML page on the ticket).
- **#207 / #205** — the scope-boundary false denial and the dial-in 401 loop both bit this cycle and will bite the next developer-lane dispatch; in scope for the next cycle.
- **`internal/ticketreport/` has no tests** — the start/finish report dispatcher shipped untested at the package level; the regression map covers the outbox, not the dispatcher. Add tests before `t13` relies on it — #231.
- **The second control plane** for the store-pull proof (spec h17/c19) is still unscoped setup work; spark's dev plane remains the candidate — #232.
- **The delivery store gap** — this cycle closed its PR without a summary and left `t10`/`t13` in prose. `/summarize-delivery` belongs before the final PR gate, not ten days after; the `Record` issue-type convention exists so the omission is countable next time.
