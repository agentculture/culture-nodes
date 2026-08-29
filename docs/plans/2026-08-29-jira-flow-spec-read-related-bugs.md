# Build Plan — jira-flow-spec-read-related-bugs

slug: `jira-flow-spec-read-related-bugs` · status: `exported` · from frame: `jira-flow-spec-read-related-bugs`

> Jira flow closes its loop: the merged flow-store cycle is deployed and proven on prod records, the spec a ticket is deciding on is readable from the ticket itself as a rendered page, the /think leg can be driven from the board, and the bugs that bit the last cycle (deploy.sh un-granting the sweep, the bridge scope-boundary false denial, the dial-in 401 loop) no longer reproduce

## Tasks

### t1 — Fix #191: deploy.sh's runner.env write carries `NODES_API_URL` and a jira-bearing `PR_UPKEEP_REPOSITORIES` forward from the existing file, or refuses loudly before touching it

- covers: c5, h3
- acceptance:
  - A fake-host harness runs the env-writing section of deploy/prod/deploy.sh twice against a runner.env that already holds `NODES_API_URL` and a jira-bearing `PR_UPKEEP_REPOSITORIES`; both survive byte-exact
  - With `NODES_API_URL` absent from both the shell and the existing file, the section exits non-zero with a hint naming the key, before any write
  - tests/`test_deploy_runner_env.py` names both cases; deploy.sh is the only production file touched

### t2 — deploy.sh becomes the two-host r4 procedure: preflight nodes doctor + checkout-state check BEFORE the thor stack is stopped, a forced pg dump before migrate, orin's worker stopped across migrate/cutover and restarted after, and an api↔worker revision-parity check before the sweep schedule resumes

- depends on: t1
- covers: c25, h17, c26, h18
- acceptance:
  - Fake-host harness: with a dirty or detached ~/git/culture-nodes-agent the thor lane exits non-zero before any docker compose stop
  - The harness records the order: doctor → dump → stop thor services → stop orin worker → migrate → nodes-cutover → up → start orin worker → parity check → resume; a parity mismatch refuses to resume the sweep
  - migrations/README.md's 0041 entry documents the two-host sequence and the dump path; the dump path is printed in the deploy summary

### t3 — Register the re-mint producer: wire Options.RemintProducerActorID to `NODES_REMINT_PRODUCER_ACTOR_ID` in cmd/nodes/worker.go (default `engine_remint_scheduler`) and make deploy/prod/register-actor.sh able to register it as a kind=engine actor

- covers: c27, h19
- acceptance:
  - internal/worker test: a technical failure of a trigger-created run schedules a re-mint whose derived record's origin.`actor_id` equals the configured id, against a PG-backed store where that id is registered
  - register-actor.sh --engine `engine_remint_scheduler` is idempotent (second run is a no-op) and is named in #230's step list

### t4 — Fix #207: the claude bridge's workflow-scope boundary measures git diff --name-only <branch-base>..HEAD plus uncommitted changes, never the workspace delta from the pre-base-move tree

- covers: c8, h6
- acceptance:
  - adapters/claude-code test: a session whose step-0 base move drags .github/workflows changes between bases ends completed
  - adapters/claude-code test: a session that itself edits .github/ still ends `policy_denied` with the same message
  - The boundary implementation is in one module with no change to the byte-identical shared bridge modules

### t5 — Fix #205: dialin.py backs off exponentially (ceiling) on HTTP 401 and surfaces the credential mismatch once; propagated byte-identically to all five adapters; the developer unit's dial token is reissued

- covers: c9, h7
- acceptance:
  - A dialin unit test drives five consecutive 401s and asserts the sleep sequence grows to the ceiling and the warning is logged once, not per attempt
  - tests/lint's byte-identical guard passes for dialin.py across all five adapters after the change
  - Operator step (hand-turn on #230): reissue the developer dial token with deploy/prod/issue-dialin-credential.sh; /v1alpha1/dial-in-presence lists company/developer as connected within one hour

### t6 — internal/ticketreport gains package tests (#231): start and finish for one run, delivery through the jira bridge only, retry/backoff to terminal failure, and the sub-interval case where finish arrives before start delivered

- covers: c3
- acceptance:
  - go test ./internal/ticketreport/ runs at least four named tests covering the four cases and they pass
  - No production code change except what the tests reveal as a defect, each such fix cited in the PR

### t7 — Operator lane: measure the before-state, reconcile the prod checkouts per q3 (discard; no qwen config), then run the r4 deploy — deploy.sh thor, deploy.sh orin — publish pr-upkeep-sweep-cycle v2 and jira-comment-consumer, register `engine_remint_scheduler`; every hand-turn a comment on #230

- depends on: t2, t3, t6
- covers: c3, h2, c15, h13
- acceptance:
  - Before touching anything: prod /v1alpha1/version, the workflow key list, and both hosts' git status are pasted into #230 (the before-state)
  - After: /v1alpha1/version contains c0f6c4a, GET /v1alpha1/workflows lists pr-upkeep-sweep-cycle v2 and jira-comment-consumer, GET /v1alpha1/actors lists `engine_remint_scheduler`, and both workers report the same revision
  - No 'Jira history cutover is pending' delivery error appears in the deploy window; the dump path is in #230

### t8 — t13 live proof of the deployed flow-store legs on a real ticket, from prod records only (signals 1, 2, 5; a bounded re-mint; a start/finish report; a bare comment reaching its consumer); close #192 #193 #194 #197 #198 on that evidence and #202 with the ledger.propose audit

- depends on: t7
- covers: c2, h1, c10, h11
- acceptance:
  - Two comments posted inside one sweep interval on a real ticket emit two facts in order (fact ids cited)
  - A deliberately killed trigger-created run re-mints once (derived record id cited) and, on a second kill, parks on a human task
  - The ticket shows a start and a finish comment for one run id; #192–#198 each close citing a prod record; #202 closes citing the v11 runs and a one-line audit that every published workflow's actor nodes declare ledger.propose

### t9 — Ticket projection in the control plane: migration `ticket_frames` (append-only, keyed by ticket id + version), POST /v1alpha1/tickets/{id}/frame (decision-token guarded), GET /v1alpha1/tickets/{id} composing runs by subject, ledger, human tasks, ticket reports, replies, and the latest frame; a subject filter on listRuns; openapi updated

- covers: c33, h23
- acceptance:
  - PG-backed test: two frame posts for one ticket yield versions 1 and 2 and GET returns version 2 with claim states byte-equal to the posted JSON
  - GET /v1alpha1/runs?subject=SCRUM-9 returns only runs whose subject matches (test with a decoy subject)
  - POST /frame without the decision token is 401; api/openapi/openapi.yaml documents the two routes and the filter

### t10 — Reply endpoint: POST /v1alpha1/tickets/{id}/replies (decision-token guarded, required replier) appends a signal event in-process with origin.kind human and the replier named, and enqueues a display-only mirror comment 'via <replier>' through the jira bridge outbox; the page-reply fact and the sweep's comment fact validate against one schema

- depends on: t9
- covers: c29, h20, c30, h21
- acceptance:
  - PG-backed test with a DECOY run: two runs parked on the same until.signal name, one whose subject is the fixture ticket and one whose subject is a different ticket; one POST yields exactly one `signal_events` row (origin.kind human, replier in payload, and the ticket id + `originating_question_id` in the same payload fields the sweep's comment fact carries) and one outbox row. A signal park matches (namespace, `event_name`) only, with no subject or payload filter (internal/store/postgres/signal.go selectLockedSubscriptionsSQL; examples/jira-question-round-trip/`question_correlation.py` states this contract), so BOTH subscriptions fire: the test asserts the ticket's run resumes and its resume cites the fact id, AND that the decoy run's consumer gets `answer_for`(payload, `its_question_id`) == None and re-parks without acting — the cross-ticket wake is observed and neutralised, not assumed away
  - A schema test validates a fixture page-reply fact and a fixture sweep comment fact against the same JSON Schema
  - `jira_comment_is_self_echo` in examples/pr-upkeep/`pr_upkeep_jira.py` is unchanged (test pins its bytes)

### t11 — Web route /tickets/:id: renders the projection (frame claims with state, questions/decisions, reports, runs, reply thread), a link back to the Jira ticket, a reply box using the existing decision-token store, and a freeze banner; PRD §8.8 rules hold on the route; no secret enters the bundle

- depends on: t9
- covers: c24, h16, c31, h22, c14, h8
- acceptance:
  - vitest: the route renders a fixture projection with every claim's state as icon+word and the back-link href equal to the ticket URL
  - playwright e2e: keyboard walk reaches the reply box and submits with the decision token; reduced-motion honoured
  - A test greps web/src for every \*`_SECRET` / bearer name and finds only the decision-token path

### t12 — Link at intake and freeze: jira-intake's intake node posts ONE page-link comment carrying a link marker (re-posted/edited in place, never duplicated); the sweep emits pr.merged for closed PRs with `merged_at` (watermarked) and links the PR to its ticket by branch name or body carrying the ticket id; a pr.merged fact freezes the ticket's page (frozen flag + merged PR pointer on the projection)

- depends on: t9
- covers: c24
- acceptance:
  - Sweep test: a merged PR yields exactly one pr.merged fact across two passes (watermark), with `issue_key` parsed from the branch or body; an open PR yields none
  - Intake workflow test: two intake milestones leave one link-marker comment on the fixture ticket
  - PG-backed test: a pr.merged fact for the linked PR sets frozen=true and `merged_pr` on GET /v1alpha1/tickets/{id}; a decision-token freeze action does the same

### t13 — Board-driven /think leg (q4 = B): declare .devague/ custody for the owe-developer lane; a lane workflow minted from a ticket fact runs the devague moves in the lane worktree, commits .devague/, posts the frame snapshot to POST /tickets/{id}/frame after each move, and surfaces frame decisions as marked questions the human answers on the ticket or the page

- depends on: t9, t10
- covers: c7, h5
- acceptance:
  - The lane config declares the custody (checkout path, branch prefix, .devague/ write allowed) and a test asserts the assign verb refuses a .devague/ write for any other lane
  - Workflow test: a fixture ticket fact mints the lane workflow; a marked question's answer (via the sweep fact or the page reply fact) transacts exactly the stated devague move and no other
  - Live: one run on prod whose `trigger_event_id` is a ticket fact and whose frame snapshot appears on GET /v1alpha1/tickets/{id}

### t14 — Live proof of the ticket-page legs on a real ticket (signals 3, 4, 8, 9) from prod records only; close #199 and #200 on that evidence

- depends on: t8, t10, t11, t12, t13
- covers: c1, h10, c17, h9, c18, h15, c38, h24, c39, h25
- acceptance:
  - Signal 8: a reply typed on /tickets/<id> appears as one Jira comment 'via <replier>', one human-origin fact, and one resumed run — ids cited from prod
  - Signal 9: the ticket's comment list at two milestones shows exactly one link-marker comment whose path carries the ticket id
  - Signal 4: a spec-chain lane run id whose `trigger_event_id` is a ticket fact; every one of signals 1–9 is recorded with its prod read or marked unverified

### t15 — Session accounting and closure: the split plan declares model-session counts per wave before any dispatch (colleague for review/explore, codex + claude-developer for build; qwen parked); /summarize-delivery is written BEFORE the cycle's final PR merges; every hand-turn is a comment on #230 or the issue it touches

- depends on: t14
- covers: c13, h12, c16, h14
- acceptance:
  - The split plan artifact lists sessions per wave and is linked from #230 before wave 1 dispatches
  - docs/deliveries/<date>-jira-flow-spec-read-related-bugs.md is in the final PR, and #192–#200 are closed only by citations in it or in #230

## Risks

- [out_of_scope] The store-pull second control plane (#232) is not built in this cycle; the h17/c19 claims of the flow-store spec stay unverified
- [unknown_nonblocking] The ticket page is LAN-only (control plane on thor:18080, authless SPA): outside users cannot reach it until an auth/exposure story exists — t11 ships for the operator on the LAN; #235 carries per-user identity
- [unknown_nonblocking] t12 changes examples/pr-upkeep/sweep.py, which redeploys by digest — whether #221's SonarCloud-boundary fold rides the same digest change or waits for #218 is undecided (task t12)
- [follow_up] The headspace code-node form of the spec chain (#237) is deferred; t13's lane sessions are agent-reported moves, not engine-verified ones (task t13)
- [unknown_nonblocking] t7 is operator-lane and irreversible in part (discarding prod checkouts, applying migrations): the dump from t2 is the only rollback; rehearse RESTORE.md on spark before t7 (task t7)
- [follow_up] Signal subscriptions match (namespace, `event_name`) only — no subject filter (signal.go selectLockedSubscriptionsSQL) — so a page reply or sweep comment for one ticket wakes every run parked on that event name; t10 neutralises it consumer-side (`question_correlation`.`answer_for` + re-park, pinned by a decoy-run test) and subject-scoped subscriptions are an engine feature deferred to #239 (task t10)
