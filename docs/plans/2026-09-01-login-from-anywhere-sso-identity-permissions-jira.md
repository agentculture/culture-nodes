# Build Plan — login-from-anywhere: sso, identity, permissions, jira coverage

slug: `login-from-anywhere-sso-identity-permissions-jira` · status: `exported` · from frame: `login-from-anywhere-sso-identity-permissions-jira`

> Culture Nodes is reachable at nodes.culture.dev behind Cloudflare Access SSO on the same Zero Trust team as chat.agentculture.org; every person who signs in is a named human actor and every agent or bridge holds its own API token, and the ledger stamps origin from the authenticated caller rather than a request field; a ticket's human tasks, proposed claims and confirmations are decided on its own page under that identity; agents can read a Jira ticket's body, comments and links, and the system moves the board itself: In Review on a PR, Pending on a parked question, Done when a human says so after live validation. OIDC / Entra workload auth is scoped as the follow-on.

## Tasks

### t1 — internal/auth: stdlib Cloudflare Access JWT verification

- instruction: Port the shape of /home/spark/git/irc-lens/src/`irc_lens`/web/auth.py (JWKS cache, aud+iss pinning, email vs `common_name` principal) to Go stdlib: crypto/rsa, crypto/sha256, encoding/base64, encoding/json, net/http. Package internal/auth only (today it holds doc.go). Export Verifier{TeamDomain, Audience}, Principal{Subject, Email, CommonName, Kind}. No middleware, no store access here — t8 wires it. Keep internal/actors/token.go untouched.
- covers: c3, h1, c22, h23
- acceptance:
  - Verify(token) against a fake JWKS server: valid RS256 token passes and yields {subject, email|`common_name`, kind}; wrong aud, wrong iss, expired, nbf in future, unknown kid after one refetch, and bad signature each refuse with a distinct reason class
  - JWKS cache is keyed by kid, refetches once on a miss with an anti-flood window, and survives a key rotation test (old key dropped, new key served)
  - go list -deps ./internal/auth shows only standard library packages; scripts/check-zero-runtime-deps.sh and tests/lint import guards pass; no log line in the package contains the token value

### t2 — `actor_identities` table + store + role model

- instruction: Follow the `inbound_authentication` shape (migrations 0031-0037: `revoked_at`, no address keys). Live under internal/store/postgres/identities.go + a Go role enum in internal/auth/roles.go (file-disjoint from t1's verifier files). Decision c37: keyed by (provider, subject); provider is 'cloudflare-access' for emails and 'cloudflare-service-token' for `common_name`. Decision c38: approver and `namespace_administrator` only; default viewer.
- covers: c49, h35
- acceptance:
  - Migration file is the highest-numbered, additive, creates `actor_identities`(provider, subject, `actor_id` FK actors, roles text\[\], `created_at`, `revoked_at`) with UNIQUE(provider, subject) and no email or credential column; migrations/pending/0036 unchanged
  - Store methods BindIdentity/LookupIdentity/RevokeIdentity are covered by tests against the embedded Postgres; a revoked binding is not returned by lookup; roles are validated against the closed set {viewer, approver, `namespace_administrator`}

### t3 — SSE keepalive for both event streams

- instruction: Small, self-contained: internal/api/events.go only, plus the two web consumers' tests. Cloudflare closes idle proxied connections at ~100 s; 25 s leaves margin. Do not change event names or payloads. NOTE: this task's coverage of c23/h24/c44/h30 is an authoring slip (ids shifted); those targets are owned by t19.
- covers: c47, h33, c23, h24, c44, h30
- acceptance:
  - internal/api/events.go writes an SSE comment line every 25 s on /v1alpha1/events and /v1alpha1/runs/{id}/events while idle; a test with a fake clock observes two keepalives in 60 s and no keepalive after the client disconnects
  - web/src EventSource consumers ignore comment frames and reconnect after a forced drop (vitest with a mocked EventSource)

### t4 — jira bridge: four-target allowlist, deploy-managed, custody record amended

- instruction: The bridge already parses a list (config.py:41,93-95). The work is the deploy write path (today a hand-turn) and the record. Do not put policy in the bridge: it keeps exact membership. Engine-side policy is t15.
- covers: c6, h18, c17, h21
- acceptance:
  - deploy/prod/deploy.sh's `deploy_jira` writes `JIRA_TRANSITION_TARGETS`='In Progress,Pending,In Review,Done' and `JIRA_TRANSITION_PROJECT_PREFIX` into jira-bridge-jira.env from deploy-time values, and lanes/grant-check.sh names both keys; a tests/deploy test asserts the written file
  - adapters/jira/tests/`test_transition_issue.py` gains cases for In Review and Done accepted and a fifth name refused; `test_bridge.py`, tests/deploy/`jirabridgeaudit_test.go`, tests/`test_pr_upkeep_sweep_jira.py` and tests/`test_jira_comment_fact_schema.py` pass unchanged
  - docs/decisions/2026-08-18-jira-transition-custody.md is superseded by docs/decisions/2026-09-XX-jira-transition-allowlist.md stating the list form, engine-side policy, and that Done is a human node (c32)

### t5 — Adopt /validate-delivery: behavioral-test convention + obligations

- instruction: Skill is already vendored (PR #271). devague 0.24.0 is installed: see devague explain oblige / evidence / delta. Docs and convention only; evidence is filed per wave by the operator running /validate-delivery. NOTE: this task's coverage of c14/h6 is an authoring slip (ids shifted); those targets are owned by t20.
- covers: c31, h13, c14, h6
- acceptance:
  - tests/behavioral/README.md declares the convention (dedicated folder; Go tests tagged //go:build behavioral, pytest marker behavioral) and scripts/lint-all.sh --list shows it is not part of lint; one placeholder test exists per language
  - devague oblige is run for every confirmed requirement claim on the frame and devague plan oblige for every acceptance criterion of confirmed tasks; devague status shows no unmet-obligation warning that names a claim without an obligation
  - docs/operations/validate-delivery-lane.md explains when the leg runs (after each wave merges, before summarize-delivery) and that a merge never asks a human node for Done before evidence is filed

### t8 — Principal middleware: one gate, loopback listener, whoami, roles, refusal telemetry

- instruction: Biggest package; keep it in one warm codex session. Read internal/api/server.go:30-165,428-555, errors.go:221-229 (wrap does no auth), humantasks.go:102-137 and its five siblings, events.go:81,419, `inbound_transport.go`:16-39. Principal resolution: t1's Verifier -> t2's LookupIdentity -> roles. Do not change internal/actors/token.go, the unversioned callback routes, or dial-in issuance. Origin stamping is t10, not here.
- depends on: t1, t2
- covers: c18, h9, c9, h3, c43, h29, c48, h34, c4, h16, c19, h10, c12, h19, c46
- acceptance:
  - internal/api has one principal middleware; the six require\*Auth helpers are deleted or delegate to it; every mutating route in the server.go table (incl. grades, runs create/cancel/patch, workflows, workflow-generations, namespaces, schedules, preflights, plan-imports) returns 401 without a principal and 403 for a viewer; `authgate_test.go`'s table lists every mutating route and asserts both
  - cmd/nodes serve gains `NODES_ACCESS_LISTEN` (loopback) + `NODES_ACCESS_TEAM_DOMAIN` + `NODES_ACCESS_AUD`; a valid Access JWT is honoured only on the loopback listener and ignored on `NODES_LISTEN`; GETs and both SSE streams (registered outside wrap) accept the principal; the existing bearer secrets keep working on `NODES_LISTEN` for the transition
  - GET /v1alpha1/whoami returns {principal, `actor_id`, roles} or 200 {unbound: true} for an allowlisted principal with no binding, and every write for an unbound principal is 403 reason unbound
  - Refusals log one line with reason class (`no_principal`, `bad_signature`, `bad_audience`, expired, unbound, `forbidden_role`) and subject, never the token; a counter per reason is exposed via internal/telemetry; a test greps the log output for the token and finds nothing
  - api/openapi/openapi.yaml replaces the 'authless by design' paragraph with the principal scheme; a contract test still passes; grep web/src and internal/ finds no OAuth client, session issuer, password hash or users table

### t9 — Web: replace the browser identity model

- instruction: Depends on t8's /whoami. Files: web/src/api/\*, components/Header.tsx, DeciderActorField.tsx (delete), OutcomeButtons.tsx (drop the token gating), routes/Inbox.tsx, Decisions.tsx, TicketView.tsx token panels only — the ticket page's new decision surface is t14, do not start it here.
- depends on: t8
- covers: c8, h2
- acceptance:
  - web/src/api/decision-token.ts is deleted; grep web/src finds no sessionStorage decision token, no localStorage actor id, no DeciderActorField, no free-text reviewer/replier input; postJson sends no Authorization header (the edge cookie carries identity)
  - Header renders the signed-in email from GET /v1alpha1/whoami and an unbound principal sees a full-page 'no actor is bound to this login' state naming the principal; no-bundled-secrets.test.ts passes with its allowlist shrunk to zero entries
  - Inbox, Decisions and TicketView tests pass with the identity fields removed; e2e ticket-view.spec.ts no longer types a token

### t10 — Origin stamped from the principal on every write and on dial-in completions

- instruction: Touches internal/api/{tickets,humantasks,reviews,grades,suiteverdicts,gatereports,`inbound_transport`}.go, internal/engine completion accept, internal/ledger origin construction — the #117 class. For each site leave a one-line comment saying whether the origin is resolved (from principal) or asserted (and why). Runs after t8 merges because both touch server.go.
- depends on: t8
- covers: c10, h4, c20, h11, c50, h36
- acceptance:
  - For replies, frame posts, freeze, human-task decisions, reviews, grades, suite-verdicts and gate-reports an API test sends a body naming a different actor than the principal and the stored record's `origin_actor_id` is the principal's actor; the body field is accepted only when equal, otherwise ignored with a 200 and a warning field
  - A dial-in completion whose ledger delta names an actor other than the authenticated actor key is refused at accept time with a diagnostic on the run naming both ids and no redispatch (#183); `inbound_transport.go` no longer hardcodes party kind
  - `humantaskfanout_test.go`'s payload invariant also asserts no Cf-Access-Jwt-Assertion value appears in any fan-out payload

### t11 — Developer lane and merge-gate scripts use their own agent principal

- instruction: Read docs/operations/spec-chain-lane.md:52, scripts/merge-gate.py, scripts/collect-handover.py, examples/merge-gate/README.md, examples/development-loop/workflow.yaml. Small package.
- depends on: t10
- covers: c45, h31
- acceptance:
  - grep across docs/operations, scripts/, deploy/ and examples/ finds no `NODES_HUMAN_DECISION_TOKEN` outside install-secrets' generation; scripts/merge-gate.py and collect-handover.py authenticate with `NODES_ACTOR_`\*`_TOKEN` or an issued dial-in credential and their suite verdicts land origin agent, authority proposed
  - docs/operations/spec-chain-lane.md §2 `claude_env` no longer lists the human token; the flows that relied on human-authority verdicts are listed in the PR body and re-pointed at a human node or accepted as proposed

### t14 — Ticket API: pending records at the served version, one review per run, reply identity

- instruction: Files: internal/api/tickets.go, ticketpending.go, a new ticketreviews.go, decisions.go (`run_id` filter already exists). Honour the stale-guard comment at ticketpending.go:44-53. Decision c40 fixes the per-run shape.
- depends on: t10
- covers: c11, h5, c16, h8
- acceptance:
  - GET /v1alpha1/tickets/{id} returns `pending_records` grouped per run with the `ledger_version` each group was read at, in one response; a test proves a record appended after the read is refused by the guard when the client submits the served version
  - POST /v1alpha1/tickets/{id}/reviews accepts {runs: \[{`run_id`, `expected_ledger_version`, records, verdict}\], rationale} and commits one review per run in order, returning per-run results; a stale run reports conflict while the others commit (decision c40)
  - A page reply's engine fact carries origin actor = the principal's actor and the Jira mirror reads the text followed by 'via' and the verified display name; the fact still validates against schemas/events/`jira_comment`.schema.json and `jira_comment_is_self_echo` is unchanged

### t15 — Engine: In Review on an open PR, Done as a human node after evidence, Pending unchanged

- instruction: Files: internal/store/postgres/signal.go:579-594, internal/engine/humantaskfanout.go (keep comment-before-move and NoGitHubPRCommentReason honesty), examples/pr-upkeep/sweep.py (+ the sweep-change checklist in docs/operations/pr-upkeep-lane.md:150-160), docs/drive-from-jira.md. The human node's evidence precondition is procedural (t5): the node is raised on merge but its instruction text tells the human to check the filed evidence.
- depends on: t4
- covers: c15, h7, h38
- acceptance:
  - examples/pr-upkeep/sweep.py emits pr.opened {repository, number, url, `issue_key`} using `merged_pr_fact`'s correlation; a workflow or engine hook plans `transition_issue` In Review from it exactly once per PR (idempotent on the fan-out UNIQUE)
  - pr.merged no longer only freezes the ticket: in the same transaction it raises a human task 'Ticket done?' whose options are done / not yet, addressed to the approver role, and whose done outcome plans `transition_issue` Done; the transition is never planned from pr.merged directly (test asserts absence) — decisions c32, c35
  - docs/drive-from-jira.md trigger table and the 'Move a ticket to Done' section are rewritten to the new behaviour in the same PR; `test_pr_upkeep_sweep_jira.py`'s transition self-echo tests pass with the bot account authoring In Review and Done

### t19 — Edge + deploy: nodes.culture.dev tunnel, loopback origin, UI base URL

- instruction: Files: deploy/prod/\*.yml, deploy/prod/install-secrets.sh, deploy/prod/cloudflared-nodes.service (new), docs/operations/nodes-culture-dev.md (new). Do NOT touch cmd/ or internal/ — the loopback listener flag itself is t8. The operator installs cloudflared on thor and runs cultureflare from thor (authenticated 2026-09-01); the API token stays on spark/thor shells, never in the repo. Count every hand-turn on an issue. Assumption c7: tunnel unit on thor.
- covers: c44, h30, c23, h24
- acceptance:
  - deploy/prod carries cloudflared-nodes.service (token-mode, `TUNNEL_TOKEN_FILE` 0600, the cloudflared-chat.service shape) targeting the loopback listener port on thor over plain HTTP, and docs/operations/nodes-culture-dev.md records the one-time cultureflare remote-login setup command with placeholders, the AUD tag location, and the session-duration value
  - compose.thor.yml and compose.orin.yml plus install-secrets' prod.env block carry `NODES_UI_BASE_URL`=<https://nodes.culture.dev>; a grep test in tests/deploy asserts both files agree; the LAN publish 18080:8080 is unchanged
  - curl <https://nodes.culture.dev/v1alpha1/version> (after the operator applies the unit) returns the same revision as <http://192.168.1.146:18080/v1alpha1/version>; recorded in the delivery summary as a hand-turn if applied by hand

### t20 — jira bridge: `read_issue` verb

- instruction: Mirror `create_issue.py`'s module shape and README section. Copy `pr_upkeep_jira`.`jira_description_text` with attribution for ADF flattening (the sweep module must stay import-free of the bridge). Add the read allowlist env to config.py's env parsing, NOT to `_FILE_FIELDS`. Update adapters/jira/README.md input examples.
- covers: c14, h6
- acceptance:
  - adapters/jira/src/`jira_bridge`/`read_issue.py` accepts exactly {verb, issue} and returns summary, description (ADF flattened, 4000-char cap), status, up to `JIRA_READ_COMMENT_LIMIT` comments (id, author accountId, created, body) and issue links; refuses a key outside `JIRA_TRANSITION_PROJECT_PREFIX` and refuses everything when the limit is unset
  - server.py dispatches it, /v1/capabilities lists it under verbs with the read custody block; tests cover prefix refusal, empty-config refusal, cap, and the exact GET path; `test_no_transitions.py` still passes and client.py is unchanged
  - The result ledger record is authority proposed, origin agent, like the three write verbs (decision c41)

### t12 — Web: the ticket page decides everything

- instruction: Lift RunDecisionCard (Decisions.tsx:515-698) into a shared component; keep OutcomeButtons' expired filter and the stated-absence rendering. Decision c40 shape from t14 (ticket API). Functional half only; the visual redesign is t17.
- depends on: t9, t14
- covers: c11, h5, c13, h20, c12, h19
- acceptance:
  - TicketView renders pending records per run with confirm/reject, required rationale, and one submit that calls the per-run review endpoint and shows per-run results; a confirmed record still reads proposed with a confirmed review beside it
  - Frame claims from the custody checkout render read-only with their confirmation state; no grading form exists anywhere in web/src; /decisions' inert claim checkbox is removed
  - Playwright: web/e2e/ticket-view.spec.ts walks decide-a-task and confirm-a-claim with intercepted API and asserts the exact request bodies; new inbox.spec.ts and decisions.spec.ts exist

### t13 — Onboarding/offboarding recipe, break-glass credential, session policy

- instruction: Docs + two small scripts. Uses t8's middleware and t2's store. Cloudflare session duration is set with cultureflare (`session_duration` on the Access app) — record the chosen value, not the default.
- depends on: t8, t19
- covers: c46, h32, c48, c26, h25
- acceptance:
  - docs/operations/people.md is the three-place recipe (Access policy, human actor revision via register-actor.sh, BindIdentity via a new scripts/bind-identity.sh) plus offboarding (revoke Access session, RevokeIdentity) and the recorded Access session duration; a second person is allowlisted and bound and decides a human task from off-LAN (recorded)
  - scripts/issue-dialin-credential.sh (or a sibling) can issue a service credential bound to the operator's human actor for LAN break-glass; the credential decides a human task from curl on the LAN and the run records it

### t16 — Jira push receiver: system webhook through a path-scoped Bypass

- instruction: Read docs/drive-from-jira.md:16-19, signalevents.go:136-283, `pr_upkeep_jira.py`:302-400. Keep the sweep module import-free of internal/; the receiver is Go and cites the Python normalisation as a spec, with a shared fixture test in tests/ to prove identity.
- depends on: t8, t19
- covers: c42, h14, h37
- acceptance:
  - POST /v1alpha1/webhooks/jira (on the loopback listener only) verifies the webhook secret (X-Hub-Signature HMAC where the tenant supports it, else a per-webhook URL token compared constant-time), normalises the payload with `pr_upkeep_jira`'s functions (cited copy, byte-identical test), and forwards to the same DeliverSignalEvent path with the same source keys and watermark; a fixture test proves receiver and sweep emit byte-identical facts
  - docs/operations/nodes-culture-dev.md gains the Bypass policy (path /v1alpha1/webhooks/jira, Everyone) and the Jira webhook registration steps; a comment posted on a SCRUM ticket produces a pr-upkeep.jira.comment fact within seconds and the next sweep pass reports duplicate=true
  - The sweep schedule stays at five minutes until a week of push runs green; the relaxation is a recorded follow-up, not part of this task (decision c51: no Automation rules)

### t17 — Web: human-welcoming UX on the human-facing pages

- instruction: Load the frontend-design skill before starting. Reuse EventTimeline, ActiveGraphCanvas, StatusChip. No new chart library without an ADR. Post each UX finding as a comment on #270 with page + file. Depends on t12 so the functional decision surface exists first.
- depends on: t12
- covers: c25, h12
- acceptance:
  - The ticket page's first screen is a flow diagram (xyflow/elk, reusing ActiveGraphCanvas primitives) of where the ticket is plus the one pending decision with its buttons; claims, runs and the reply thread are below or in tabs
  - A signed-in person with nothing pending lands on a first-visit page that names the tickets waiting on others and how to get a link; Inbox and Decisions become secondary views reachable from the header, or are retired where the ticket page replaces them (decision c33), with the retirement named in the PR; non-human-facing pages unchanged except shared components (c39)
  - `NODES_SHOTS` screenshot pass captured for every human-facing page before and after and attached to #270 (c34); the operator reviews them before merge

### t18 — Secret removal, measurement sitting, invariants diff

- instruction: Operator lane, last. Runs /validate-delivery per wave before this, then /summarize-delivery after. Every hand-turn counted on an issue.
- depends on: t8, t9, t10, t11, t12, t13, t14, t15, t16, t17, t4, t5, t19, t20
- covers: c29, h28, c27, h26, c28, h27, c1, h15, c5, h17, c21, h22
- acceptance:
  - A dated docs/audits/ measurement sitting for login-from-anywhere records all six c29 measurements with the exact command, id or record, failures recorded as failures; `NODES_HUMAN_DECISION_TOKEN_SECRET` is removed from every human's hands via remove-secret.sh and the run is recorded
  - The 2026-08-30 audit is re-verified live and every changed line gets a dated correction; the delivery summary maps each after-state sentence to a measurement and quotes the Done/In Review hand-turn counts before and after and decisions under the secret vs under a principal
  - git diff main...branch shows no change under internal/actors/token.go, internal/actors/`inbound_issuance.go`, internal/awsauth/ or server.go's unversioned callback registration; go test ./internal/actors/... ./internal/awsauth/... passes unchanged

## Risks

- [unknown_nonblocking] The codex write path is still unproven (#18): t9, t11, t15 and t16 are large write packages; if a codex lane loses writes the fan-out falls back to the operator's window and the split plan must budget it
- [unknown_nonblocking] prod-api-1 egress to agentculture.cloudflareaccess.com/cdn-cgi/access/certs is verified only from the thor host (container image has no shell); the first in-container JWKS fetch is proven at the first login after deploy (task t8)
- [unknown_nonblocking] Jira Cloud system-webhook signing on this tenant is unverified: if X-Hub-Signature is unavailable the receiver falls back to a URL token, which weakens replay protection to TLS + Bypass path scoping (task t16)
- [unknown_nonblocking] No automated test exercises the Access path (Playwright drives the built bundle with an intercepted API); the c29 measurements are manual by design and t18 is the only proof (task t18)
- [unknown_nonblocking] Cloudflare Access session revocation is a dashboard action; a removed person keeps a valid session until it expires or is revoked — t13 records the chosen duration (task t13)
- [unknown_nonblocking] The codex write path is still unproven (#18): t8, t14, t12 and t17 are large write packages; if a codex lane loses writes the fan-out falls back to the operator's window and the split plan must budget it
