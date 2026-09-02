# Login-from-anywhere measurement sitting (2026-09-02, pre-deploy)

Task t18 of the login-from-anywhere plan
(`docs/plans/2026-09-01-login-from-anywhere-sso-identity-permissions-jira.md`).
This is the **first** sitting, taken on the operator host at 03:10 IDT with
`feat/login-from-anywhere` at `6164e0c` **before** the branch was deployed to
prod. Every number below is what was actually measured; every measurement the
spec asks for that needs the deploy is recorded as **unmeasured**, not as
passed. A second sitting follows the deploy and the #273 hand-turns.

## Prod floor as found (re-verification of the 2026-08-30 audit)

| Check | 2026-08-30 audit said | Measured now | Verdict |
|---|---|---|---|
| Control plane revision | `66a3b26` | `15fefde` (`GET /v1alpha1/version`) | changed since the audit (presentable-floor deploys); the login-from-anywhere code is **not** on prod yet |
| Unauthenticated GET `/v1alpha1/tickets/{id}` | 200 from any LAN host | 200 | unchanged (expected until deploy) |
| Unauthenticated GET `/v1alpha1/actors` | 200 | 200 | unchanged |
| `POST /v1alpha1/runs/{id}/grades` unauthenticated | audit line said the shared secret gates it | 404 for an unknown run, i.e. the route is reached without any credential | the audit line was wrong; corrected in the spec's before-state (c5) and gated by t8 |
| `GET /v1alpha1/whoami` | did not exist | 404 | route lands with the deploy |
| `https://nodes.culture.dev/v1alpha1/version` | did not exist | unreachable (no DNS/tunnel) | provisioning is hand-turn #273 items 1-6 |
| `cloudflared` on thor | not installed | not installed | hand-turn #273 item 1 |
| `JIRA_API_BASE` granted on thor | did not exist | not granted | hand-turn #273 (token install), after deploy |
| Jira identity | operator's personal account for sweep, bridge, skills | still the personal account; a service account `culture-nodes` now exists with a scoped token minted 2026-09-02 (accountId `712020:5e0ae915-ba1a-43ef-bce0-c0d5ff9bb615`), not yet installed | install waits for deploy (t21) |

## The six success-signal measurements (spec c29)

| # | Measurement | Status | Why |
|---|---|---|---|
| a | Unauthenticated curl to `https://nodes.culture.dev/v1alpha1/tickets/SCRUM-N` gets an Access redirect; same path via the LAN address gets 401 on writes | **unmeasured** | no tunnel yet (#273 items 1-6); LAN write gating lands with the deploy |
| b | A human-task decision from the ticket page off-LAN lands a ledger record whose `origin_actor_id` is the actor bound to the signed-in email, no decider typed | **unmeasured** | needs deploy + an `actor_identities` binding (`scripts/bind-identity.sh`, docs/operations/people.md) |
| c | One bridge completion with a self-declared foreign `origin_actor_id` is refused with a diagnostic and consumes no redispatch | **unmeasured on prod**; proven by test | `internal/api` + engine tests from t10 (422, diagnostic naming both ids, no redispatch) run green on the merged branch |
| d | An intake run's agent output quotes a comment from the ticket it could not see before | **unmeasured** | needs the `read_issue` verb deployed to the jira bridge on thor and a run through jira-intake |
| e | A merged PR naming a SCRUM key moves the ticket to Done inside one sweep interval with zero operator transitions | **superseded by decision c32** | Done is never automatic: the merge raises a "Ticket done?" human task; the measurable form is "merge → task raised → done outcome → Done transition by the bot account", to be measured after deploy |
| f | The shared decision secret is removed from every human's hands (`remove-secret.sh` run recorded) | **not done, by design** | sequenced after t22 (break-glass principal from an issued credential, deviation d2): removing the secret before that leaves the LAN with no fallback |

## Invariants (t18 acceptance 3) — measured now

- `git diff --stat main...feat/login-from-anywhere -- internal/actors/token.go internal/actors/inbound_issuance.go internal/awsauth/` is empty across 168 changed files.
- The unversioned callback and runner-callback route registrations in `internal/api/server.go` do not appear in the branch diff.
- `go test ./internal/actors/... ./internal/awsauth/...` passes unchanged on the merged branch.

## Counts the delivery summary needs

- Operator hand-turns for Done and In Review before the cycle: every board move beyond In Progress and Pending (#118 step 7). After the cycle: the code plans In Review on `pr.opened` and Done on the human node's outcome; **zero measured yet** because nothing is deployed.
- Decisions under the shared secret vs under a principal after deploy: **0 / 0** until deploy.
- Operator gate fixups during the build: 7 (t15 no-run merge path; t15 expiry exclusion; engine_store.go split; grade tests authenticate; telemetry allowlist constructor; e2e route-mock await + fixture split; t21 fixture placeholders + changelog consolidation).
- Codex sessions: 10 dispatched, 8 succeeded, 2 failed on quota (deviation d3).

## What closes this sitting

After the deploy of 0.47.0 and #273 items 1-6 and the token install, re-measure
a-e above, run `remove-secret.sh NODES_HUMAN_DECISION_TOKEN_SECRET` once t22's
break-glass credential is issued and verified, and append the second sitting
below this section with the exact commands, ids and records.
