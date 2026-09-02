# Delivery Summary — login-from-anywhere: sso, identity, permissions, jira coverage

plan: `login-from-anywhere-sso-identity-permissions-jira` · run: `partial` · date: `2026-09-01`
baseline: `devague summary skeleton`

## Intent

Put Culture Nodes in front of people from anywhere: nodes.culture.dev behind
Cloudflare Access, a real principal in the control plane bound to a registered
actor with roles, ledger origin stamped from that principal, the ticket page as
the one decision surface with a human-welcoming first screen, and the Jira loop
completed (read verb, In Review and Done as board moves, push instead of
polling). The plan is
`docs/plans/2026-09-01-login-from-anywhere-sso-identity-permissions-jira.md`
(PR #272), executed 2026-09-01/02 through culture-nodes itself: 10 codex
dispatches (8 handed over), 4 developer-actor dispatches, 6 worktree
subagents, every merge TDD-gated by the operator on `feat/login-from-anywhere`.
Tracking ticket SCRUM-8; issues #6 #111 #235 #255 #256 #257 #270 #273.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — internal/auth: stdlib Cloudflare Access JWT verification
- `t2` — `actor_identities` table + store + role model
- `t3` — SSE keepalive for both event streams
- `t4` — jira bridge: four-target allowlist, deploy-managed, custody record amended
- `t5` — Adopt /validate-delivery: behavioral-test convention + obligations
- `t8` — Principal middleware: one gate, loopback listener, whoami, roles, refusal telemetry
- `t9` — Web: replace the browser identity model
- `t10` — Origin stamped from the principal on every write and on dial-in completions
- `t11` — Developer lane and merge-gate scripts use their own agent principal
- `t12` — Web: the ticket page decides everything
- `t13` — Onboarding/offboarding recipe, break-glass credential, session policy
- `t14` — Ticket API: pending records at the served version, one review per run, reply identity
- `t15` — Engine: In Review on an open PR, Done as a human node after evidence, Pending unchanged
- `t16` — Jira push receiver: system webhook through a path-scoped Bypass
- `t17` — Web: human-welcoming UX on the human-facing pages
- `t18` — Secret removal, measurement sitting, invariants diff
- `t19` — Edge + deploy: nodes.culture.dev tunnel, loopback origin, UI base URL
- `t20` — jira bridge: `read_issue` verb

Added during the run (proposed, not yet confirmed by the operator):

- `t21` — Jira API base for scoped service-account tokens (deviation follow-up)
- `t22` — Break-glass principal from an issued dial-in credential (deviation d2 follow-up)

`t6` and `t7` are rejected duplicates from the plan's authoring (ids shifted
when the CLI refused decision ids as coverage) and carried no work.

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | `internal/auth/verifier.go` + tests (codex-thor, run `01M1FES1BNC5RF3R3E5506MQ6P`), merge `c8f317e` |
| `t2` | delivered | migration `0053_actor_identities.sql`, `internal/store/postgres/identities.go`, `internal/auth/roles.go` (codex-orin, run `01M1FESDF56H0QFZM6CX23A8MZ`), merge `8c497aa` |
| `t3` | delivered | 25 s comment keepalive on both SSE streams, injectable interval, Go + vitest tests (subagent), merge `e5dc6e1` |
| `t4` | delivered | deploy_jira writes `JIRA_TRANSITION_TARGETS`, grant-check names it, `docs/decisions/2026-09-02-jira-transition-allowlist.md` (codex-orin, run `01M1FF6ZYE7ZP94Q4NS23RAAYW`), merge `6c7b1be` |
| `t5` | delivered | `tests/behavioral/` convention, `docs/operations/validate-delivery-lane.md`, 22 frame + 52 plan obligations (subagent), merge `32b93e4` |
| `t8` | delivered | `internal/api/principal.go`, dual listeners, `/v1alpha1/whoami`, refusal telemetry, openapi (codex-thor, run `01M1FFKY6NA8TF6J4FXC4Q4P7D`), merge `6a9a3f8`; operator fixup `e19bb9a` (telemetry allowlist constructor) |
| `t9` | delivered | `decision-token.ts` deleted, `useWhoami`, `IdentityGate`, Header identity, identity-boundary test (subagent), merge `9edb4aa` |
| `t10` | delivered | origin from the principal on eight write routes, dial-in completion refused on identity mismatch (422, diagnostic, no redispatch), fan-out invariant extended (codex-thor, run `01M1FGDCBVQBPY6V14PNGGCFZT`), merge `1909f18`; operator fixups `af9dcc7`, grade tests authenticate |
| `t11` | delivered | `internal/api/actorbearer.go`, merge-gate/collect-handover on `NODES_ACTOR_MERGE_GATE_TOKEN`, consumer list in `docs/operations/spec-chain-lane.md` (subagent), merge `6e308fa` |
| `t12` | delivered | shared `RunDecisionCard`, `TicketClaimReviews`, Playwright specs for ticket/inbox/decisions (developer actor, run `01M1FJX5PSYQ36TVAA9R9VJZ2E`), merge `f39d92f`; operator fixup `c0c368b` (awaited route mocks, fixture split) |
| `t13` | partial | `docs/operations/people.md`, `scripts/bind-identity.sh`, `register-actor.sh --human`, 8h session policy (subagent), merge `d2813f9`; the break-glass credential is documented as a **gap** (deviation d2 → t22) and the second-person off-LAN proof waits for deploy |
| `t14` | delivered | `pending_records` at the served version, `POST /tickets/{id}/reviews` per run, reply mirror `via <display name>` (developer actor, run `01M1FHT1FZGKS8Q0937R5WXS8T`), merge `673afc2` |
| `t15` | delivered | `pr.opened` → In Review, `pr.merged` → "Ticket done?" human task → Done on outcome, migration 0054, drive-from-jira rewritten (codex-orin, run `01M1FFNX8G7FFQG2G7ZPNTJCPB`), merge `84ca869`; operator fixups `944f92c` (no-run tickets freeze without a task; engine_store.go split) and `f8464a2` (task excluded from merged-PR expiry) |
| `t16` | delivered | `POST /v1alpha1/webhooks/jira` on the loopback listener, HMAC/URL-token auth, Go port of the sweep seam with a Python/Go parity fixture (codex-orin, run `01M1FGJ6DY59GYTATWGE94JKW3`), merge `7ebb23b` |
| `t17` | delivered (h12 review pending) | SVG flow rail + pending decision above the fold, `/` first-visit page, Inbox/Decisions demoted not retired, before/after shots and findings on #270 (Opus subagent with frontend-design), merge `5f8d472` |
| `t18` | partial | first, pre-deploy sitting `docs/audits/2026-09-02-login-from-anywhere-measurement-sitting.md` (`1ca78d2`): floor re-verified live, invariants measured; signals a–e unmeasured until deploy + #273, f deferred behind t22 |
| `t19` | partial | `deploy/prod/cloudflared-nodes.service`, `docs/operations/nodes-culture-dev.md`, `NODES_UI_BASE_URL=https://nodes.culture.dev` in both compose files and install-secrets, `tests/deploy/nodesculturedev_test.go` (subagent), merge `c02dca7`; the host steps (cloudflared on thor, provisioning, token, unit) are operator hand-turns on #273, not done |
| `t20` | delivered | `adapters/jira/src/jira_bridge/read_issue.py`, capabilities, 33 adapter tests (codex-thor, run `01M1FF6GWJYHQK4DM0WHJX5TZV`), merge `588ac9d` |
| `t21` | delivered (task proposed) | `JIRA_API_BASE` across sweep, four bridge verbs, both skills, deploy lanes, docs (developer actor, run `01M1FG7ZNTTR43Z48ZTBHWC0BP`), merge `6164e0c`; fixup `e10c851` (neutral fixture placeholders, changelog consolidation) |
| `t22` | delivered (task proposed) | `internal/api/breakglass.go`: a `cnd_` credential on the LAN listener resolves through the existing dial-in admission path into an approver principal for a human actor; five new refusal classes; `people.md` recipe, `issue-dialin-credential.sh` header, ledger item 11 (developer actor, run `01M1FP63P101PAAFATYN6T0VM2`), merge `844a014` |

## Mid-work Decisions

- `d1` — Jira Cloud service-account tokens are scoped tokens that authenticate only through the `api.atlassian.com/ex/jira/<cloudId>` gateway (site URL → 401, gateway → 200); every caller built URLs from a bare site host — decision c52 (service account) was taken after the plan converged and the gateway constraint was unknown until the token was minted. Follow-up task t21.
- `d2` — an issued dial-in credential can be minted for the operator's human actor but the principal middleware never consults `inbound_authentication`, so the only LAN break-glass is the transition bearer; `remove-secret.sh` for that secret must wait — found while writing `people.md`. Follow-up task t22.
- `d3` — codex lane capacity exhausted at 01:27 after 10 sessions (both hosts share one ChatGPT pool; reset 05:59); runs for t14 and t11 failed with no handover — rerouted per the approved split plan's fallback: t14/t12/t22 to the developer actor, t11/t17 to worktree subagents.
- Jira identity is a **service account**, not a bot user (frame decision c52, operator) — same credential pair for the engine, centrally managed; minted 2026-09-02 with read:jira-work, write:jira-work, read:jira-user; the token stayed on the operator's clipboard, never in the transcript or a file the agent wrote.
- Version bumps: three packages (t15, t16, t21) bumped `pyproject.toml`/`CHANGELOG.md` out of turn; consolidated into one `0.47.0` entry (`aed1379`) rather than reverted.
- The t17 diagram is hand-written SVG, not the xyflow/ELK canvas: ELK lays out asynchronously and the canvas mounts a pannable `role="application"` surface — a fixed five-stop rail would paint the fallback then jump on the first screen. Canvas primitives stay on the graph pages (c39).
- Nothing was retired in t17: Inbox and Decisions list pending items for runs with no ticket key and therefore no ticket page (c33: retire only when replaced).
- Playwright runs `vite preview` against `web/dist` without rebuilding; a stale bundle produced 11 false failures at the t12 gate. The gate now builds first.
- Confirms that are the operator's alone — d1/d2/d3, t21/t22, the obligations and evidence, the h12 screenshot verdict — were **not** given during the run; the operator asked for the cycle to complete autonomously, so t21 and t22 were merged on their green TDD gates with the records left `proposed` for the PR gate.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t4`, `t15`, `t16`, `t18` (`d1`) | scoped service-account tokens authenticate only through the api.atlassian.com gateway; a separate `JIRA_API_BASE` was needed before the credential could be installed | needs-follow-up |
| `t13`, `t18` (`d2`) | the credential lifecycle existed but the principal resolver did not read issued credentials; t22 closed the code gap in this run, the credential is not yet issued on prod so the shared secret stays until the second sitting | needs-follow-up |
| `t14`, `t11`, `t12`, `t17`, `t22` (`d3`) | codex quota exhausted; packages rerouted to the developer actor and worktree subagents per the split plan's fallback | acceptable |
| `t15` | two defects the codex host could not catch (no Postgres): a merged PR for a ticket with no run errored the delivery; the new task was selected by the merged-PR expiry sweep — both fixed in the operator lane before the gate passed | risky |
| `t8` | added a telemetry attribute key without its typed constructor; the allowlist test was not in the package set the codex brief named — fixed at the t14 gate | acceptable |
| `t17` | acceptance 3 says the operator reviews the screenshots **before** merge; the merge went ahead on green gates with the review outstanding (shots and findings delivered to the operator and #270) | risky |
| `t18` | the six c29 measurements need the deployed branch and the #273 hand-turns; only the floor re-verification and the invariants diff were measurable; `remove-secret.sh` not run (d2) | needs-follow-up |
| `t19` | the in-repo half shipped; the host half (cloudflared install, provisioning, token, unit, re-grant) is eight counted hand-turns on #273, none done yet | needs-follow-up |
| `t21`, `t22` | tasks added mid-run (llm-proposed) and merged on their gates without the operator's confirm | risky |

## Evidence

- tests (Go): `go test ./...` on `feat/login-from-anywhere` at `5f8d472` — pass, no failures across the module (incl. `internal/api`, `internal/engine`, `internal/store/postgres` on temp Postgres, `internal/telemetry`, `tests/lint`, `tests/deploy`)
- tests (Python): `uv run pytest -q -n auto` — 773 passed at `6164e0c`; `adapters/jira`: 59 passed
- tests (web): `npm run typecheck` clean; vitest 665 passed; Playwright 97 passed on a fresh `npm run build` at `5f8d472`
- lint: `scripts/lint-all.sh root` — green at each subagent gate except the live `triage` step (fixed by dispositions for #270, #273); markdownlint 0 errors on every touched doc
- commits: `9404478..1ca78d2` on `feat/login-from-anywhere` (51 commits, 20 task merges, 7 operator fixups)
- devague: 56 evidence records against obligations o1–o55 (`devague evidence --list`), deviations d1–d3 (`devague deviate --list`)
- runs graded on prod: t1 5, t2 5, t20 5, t4 4, t8 5, t15 3, t10 4, t16 4, t21 4, t14 5, t12 5
- PRs / issues: #271 (spec, merged), #272 (plan, open), #273 (hand-turns), #270 (UX findings), #235, #18 (write path proven), #48 (capacity data point); SCRUM-8

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| The control plane verifies a Cloudflare Access JWT with the standard library and refuses each failure class distinctly | high | `internal/auth/verifier_test.go` · merge `c8f317e` · o1–o3 evidence |
| A principal is bound by (provider, subject) to a registered actor with roles; every mutating route is gated 401/403; Access JWTs are honoured only on the loopback listener | high | `internal/api/authgate_test.go`, `principal_test.go` · merges `8c497aa`, `6a9a3f8` |
| Ledger origin on every write and on dial-in completions comes from the authenticated caller; a foreign actor id is refused without redispatch | high (tests) / unverified (prod) | `internal/api` + engine tests at `1909f18` · c29(c) not measured on prod |
| The browser holds no token and no free-text identity; identity comes from `/v1alpha1/whoami` | high | `web/src/api/identity-boundary.test.ts` · e2e `ticket-view.spec.ts` |
| The ticket page decides human tasks and proposed claims at the served ledger version, one review per run | high | `internal/api/ticketreviews_test.go` · `RunDecisionCard.test.tsx` · e2e specs |
| The ticket page's first screen is a flow rail plus the pending decision; `/` is a first-visit page | medium | `e2e/home.spec.ts`, `ticket-flow.test.ts` · shots on #270 · **h12 operator review outstanding** |
| The jira bridge can read a ticket (`read_issue`) with parse-time custody | high | `adapters/jira/tests/test_read_issue.py` · merge `588ac9d` |
| The transition allowlist is four targets, deploy-managed; Done is a human node after merge, never automatic | high | `test_transition_issue.py`, engine/store tests at `f8464a2` · `docs/decisions/2026-09-02-jira-transition-allowlist.md` |
| A Jira system webhook wakes the receiver, which hydrates and replays through the sweep seam with identical source keys | high (tests) / unverified (live) | Python/Go parity fixture at `7ebb23b` · Bypass policy and webhook registration are hand-turns |
| Scoped service-account tokens work through `JIRA_API_BASE` | high (tests) / unverified (prod) | `test_api_base.py`, `test_pr_upkeep_sweep_jira_api_base.py` · credential not installed yet |
| nodes.culture.dev serves the site through Cloudflare Access | unverified | no tunnel yet — #273 items 1–6 |
| The shared decision secret is out of every human's hands | unverified | deliberately not done — t22 landed the break-glass path; the credential must be issued and proven on prod first |
| A person off-LAN decided a human task under their own identity | unverified | needs deploy + a binding (`people.md`) |

## Remaining Work / Follow-up

- `t22` landed; next: issue the operator's break-glass credential per `people.md` on thor, prove it with `whoami` and one decision, and only then `remove-secret.sh NODES_HUMAN_DECISION_TOKEN_SECRET` (t18 f).
- `t18` second sitting — after 0.47.0 deploys and #273 items 1–6: measure c29 a–e, tick the hand-turns, append to `docs/audits/2026-09-02-login-from-anywhere-measurement-sitting.md`.
- `t19` host half — #273: cloudflared on thor, `cultureflare remote-login setup`, AUD into prod.env, 8h session, tunnel token, unit, `NODES_UI_BASE_URL` re-grant, verification curl pair.
- Jira service account install — #273 comment: gateway base, three keys on thor/orin, bridge restart, `jira_bot_account_id`, restart runners; then verify the operator's own comment is a human fact.
- `t17` h12 — operator reviews the before/after shots (delivered; #270) and either files the evidence or sends the pages back.
- Operator confirms outstanding on the plan: deviations d1–d3, tasks t21/t22, 74 obligations, 53+ evidence records.
- #18 remaining half — codex runs still publish no `refs/culture-nodes/<run>/…` ref on every run (t1/t2 harvested by branch fetch; t10/t16 published `refs/culture-nodes/tN`).
- Follow-ups surfaced by the build: `register-actor.sh` needs an endpoint-less agent registration verb (t11 used a placeholder endpoint for `company/merge-gate`); Decisions tab strip and Inbox first screen defects noted in the t17 findings (#270); the sweep interval relaxation after a week of push running green (t16, recorded follow-up).
