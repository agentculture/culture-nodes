# Delivery Summary — nodes-culture-dev hand-turns (#273)

plan: `nodes-culture-dev-hand-turns-273` · run: `complete` · date: `2026-09-02`
baseline: `devague summary skeleton`

## Intent

> nodes.culture.dev is live: 0.47.0 runs on thor and orin, the tunnel unit on thor serves the loopback Access listener, the Access app pins an 8h session to ori's email, ticket-page links carry the public origin, and the sweep and jira bridge act as the dedicated service account so the operator's own Jira comments count as human facts — every host step ticked on #273 with its output

After: Both prod hosts run 857cb49; <https://nodes.culture.dev> answers through the tunnel with the same revision as the LAN port; the Access app allows ori.nachum@gmail.com with an 8h session and its AUD is pinned in thor's prod.env; every Jira page link reads <https://nodes.culture.dev/tickets/><KEY>; the sweep and jira bridge authenticate as the service account via the gateway base with its accountId granted as `jira_bot_account_id`; every step is ticked on #273 with the output the recipe asks for; the token runbook is a nodes verb and a doc

## Planned Work

- `t1` — Stash the codex engine-clone leftovers on thor and orin (hand-turn)
- `t2` — Deploy 0.47.0 (857cb49) to thor then orin (hand-turn)
- `t3` — Install cloudflared 2026.8.3 arm64 at /usr/local/bin/cloudflared on thor (hand-turn)
- `t4` — Provision the tunnel, CNAME, Access app and allow policy with cultureflare from thor (hand-turn, once)
- `t5` — Pin the Access tuple in thor's prod.env and restart the api (hand-turn)
- `t6` — Re-grant `NODES_UI_BASE_URL` to the public origin on both hosts and redeploy (hand-turn)
- `t7` — Install the Jira service-account trio on thor and orin and re-grant `jira_bot_account_id` (hand-turn)
- `t8` — Prove an operator comment is a human fact under the service account (hand-turn)
- `t9` — Place the tunnel token file and enable cloudflared-nodes.service under thor's login user (hand-turn)
- `t10` — Run the verification pair through the tunnel and on the LAN (hand-turn)
- `t11` — Add the nodes jira-token verb (mint | verify | install) and docs/operations/jira-service-account.md (repo change, PR)
- `t12` — Close the #273 ledger: tick every step with output, add the two missing hand-turns, record the boundaries (hand-turn)

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | `git stash push -m "#273 codex leftovers 2026-09-02"` as `culture-codex` on thor (19 files) and orin (7 files); both clones clean at preflight |
| `t2` | delivered | `deploy.sh thor` (exit 3, sweep paused) then `deploy.sh orin` from a clean `857cb49` worktree; orin's first provision refused on a bridge cache file (#276), moved aside and re-run; both hosts stamp `857cb49ddd95`, sweep schedule resumed |
| `t3` | delivered | `/usr/local/bin/cloudflared` = `2026.8.3 (built 2026-08-31-10:05 UTC)` on thor |
| `t4` | delivered | `cultureflare remote-login setup … --session-duration 8h --with-service-token --apply` (0.15.0): tunnel `nodes-culture-dev` (`49e52726-…`), proxied CNAME, Access app (8h, operator email), service token + `non_identity` policy. The envelope's `tunnel_token` was not a connector token; the real one came from `GET cfd_tunnel/<id>/token` (cultureflare#55) |
| `t5` | delivered | `NODES_ACCESS_LISTEN=:8081`, `NODES_ACCESS_TEAM_DOMAIN`, `NODES_ACCESS_AUD=aaa2cd41…` in thor's `prod.env`; api recreated; `Access API listening on :8081`; `127.0.0.1:18081` only |
| `t6` | delivered | `remove-secret.sh NODES_UI_BASE_URL --yes thor orin` + `install-secrets.sh` + recreate/redeploy; both hosts and all containers read `https://nodes.culture.dev`; `runner-secrets.env` untouched |
| `t7` | delivered | token sealed on spark (`JIRA_SERVICE_ACCOUNT_TOKEN`), `verify` → accountId `712020:5e0ae915…`; Jira lane of `install-secrets.sh` on both hosts (pair + `JIRA_API_BASE`, other refs kept); thor bridge env merged by hand; bridge redeployed after the first deploy skipped the lane (`JIRA_SITE` unset); `jira_bot_account_id` re-granted, runners restarted |
| `t8` | delivered | operator browser comments SCRUM-8 #10267 and #10307 emitted as `pr-upkeep.jira.comment` facts; bot comment #10268 filtered; consumer run `01M1GMSN91PXWM2EJ7BXDFRX9J` posted reply #10308 as the service account |
| `t9` | delivered (d1) | no token file: `NODES_CULTURE_DEV_TUNNEL_TOKEN` hidden in thor's grant, unit `ExecStart=%h/.local/bin/grant run --inject TUNNEL_TOKEN=… -- /usr/local/bin/cloudflared tunnel --no-autoupdate run`, login user, `Linger=yes`, 4 registered connections |
| `t10` | delivered | tunnel (with the operator's Access session) and LAN version bodies byte-identical at `857cb49`; service-token half reaches the plane and is refused `401 malformed` (#277, filed as unmet) |
| `t11` | delivered (d2) | `nodes jira-token mint \| seal \| verify \| install`, `docs/operations/jira-service-account.md`, catalog entries, 238 tests; version 0.47.1 → 0.47.10 after the upkeep actor's review fixes |
| `t12` | delivered | #273 ledger comment with every output, items 10 and 11 added, boundaries recorded, bridge proof comment; SCRUM-8 status comment |

## Mid-work Decisions

- `d1` — The nodes.culture.dev tunnel token is sealed as a hidden secret in thor's grant store (`NODES_CULTURE_DEV_TUNNEL_TOKEN`) and cloudflared-nodes.service execs through `grant run --inject TUNNEL_TOKEN=…` instead of reading `TUNNEL_TOKEN_FILE` from a 0600 file; the unit file and recipe step 3 change accordingly, widening spec non-goal c11 by one unit file and one doc — operator decision 2026-09-02: no plaintext token on disk; grant (the renamed shushu) is on thor and spark; rotation is one `grant set`.
- `d2` — The Jira service-account token is sealed hidden in spark's grant as `JIRA_SERVICE_ACCOUNT_TOKEN` by the new `nodes jira-token seal` verb and every install step consumes it via `grant run --inject`; the 0600 env file the first t11 build assumed is gone — operator decision 2026-09-02: the token must not be saved to disk in plaintext.
- No record covers these, captured directly: the connector token was read from the Cloudflare API because cultureflare's envelope value is invalid; the Access service token was rotated once while diagnosing; `pr-upkeep-sweep-cycle` v3 was published and `NODES_API_URL` restored to the LAN IP on both hosts to get the sweep green (#279, #281); the operator identity (`company/ori`, Access binding with `namespace_administrator`, break-glass credential) was created because publishing needed a namespace-admin principal; the jira bridge was redeployed with `JIRA_SITE` exported; six issues were filed rather than fixed in-cycle.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|------------------------|-----------------|
| `t9` (`d1`) | operator decision 2026-09-02: no plaintext token on disk; grant (the renamed shushu secrets manager) is installed on thor and spark, hidden secrets are consumable only via grant run, and rotation becomes one grant set | `acceptable` |
| `t11` (`d2`) | operator decision 2026-09-02: the token must not be saved to disk in plaintext; grant is installed on spark and thor | `acceptable` |
| `t4` | the sealed connector token comes from the Cloudflare API, not cultureflare's `setup --json` envelope, which cloudflared rejects | `needs-follow-up` (cultureflare#55) |
| `t7` | the sweep could not go green on the credential alone: workflow v2 predated `pr_upkeep_emit.py` and the deploy rewrote `NODES_API_URL`; both fixed live, neither in the plan | `needs-follow-up` (#279, #281) |
| `t10` | the service-token half of the verification is unmet (`401 malformed`); the operator-session half passed | `needs-follow-up` (#277) |
| `t12` | the ledger also records identity onboarding and break-glass steps the plan did not list; `NODES_HUMAN_DECISION_TOKEN_SECRET` deliberately not removed | `acceptable` |

## Evidence

- tests: `uv run pytest -n auto` — 1014 passed (6 subtests) at `7015a9d`; `tests/test_jira_token.py` — 238 passed; `uv run pytest -m behavioral` — 1 passed
- tests: `tests/deploy` — `TestTunnelUnitIsTokenModeLoopbackAndUnprivileged`, `TestTunnelDocNamesTheSamePortAndVerification`, `TestTheLANPublishIsUnchangedBesideTheTunnel` — pass
- lint: `scripts/lint-all.sh root` — all lint steps passed; `uv run teken cli doctor . --strict` — exit 0
- commits: `857cb49..91fd8f7` on `hand-turns/273` (incl. the upkeep actor's `d300361..41b830a`)
- PRs / issues: PR #282; #273 (ledger comments 2026-09-02); #276 #277 #278 #279 #280 #281; cultureflare#55; grant#28
- devague: obligations `o1`–`o14`, evidence `e1`–`e14` (`e13` = fail), deltas `b1`–`b6`, all proposed; deviations `d1`, `d2` approved
- live: `signal_events` rows for SCRUM-8 comments 10267/10307; run `01M1GHBH67PWM005EA1ZB9KCEP` (sweep exit 0); run `01M1GMSN91PXWM2EJ7BXDFRX9J` (comment_posted)

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| nodes.culture.dev serves the control plane behind Access with an 8h session | high | `e5`, `e6`, `e14` · #273 ledger · `curl -sI` 302 |
| tunnel and LAN answer the same revision `857cb49` | high | `e12` (byte-identical bodies) |
| the tunnel token and service-token secret exist only as hidden grant secrets on thor | high | `e14` · `grant list` on thor · commit `45bb9a5` |
| both hosts run 0.47.0 and every person-facing link uses the public origin | high | `e1`, `e7` · docker inspect on both hosts |
| the sweep and jira bridge act as the service account and operator comments are human facts | high | `e8`, `e9` · runs `01M1GHBH…`, `01M1GMSN…` · SCRUM-8 #10308 |
| `nodes jira-token` mints/seals/verifies/installs and never prints the token | high | `e11` · `tests/test_jira_token.py` · commit `b56a452` |
| a service-token caller is a recognised principal through the tunnel (spec c9) | unverified | `e13` fail — refused `malformed`, #277 |
| `whoami` reports the bound actor for an Access identity | unverified | observed `unbound: true` after a successful admin write, #280 |
| the shared decision secret is retired | not claimed | deliberately left in place until the next sitting (h25) |

## Remaining Work / Follow-up

- Confirm the proposed validation records (`o1`–`o14`, `e1`–`e14`, `b1`–`b6`) — operator.
- #273 items 10 and 11: Access Bypass policy for `/v1alpha1/webhooks/jira`, Jira system webhook registration + auth mode — operator hand-turns.
- t18 second measurement sitting (rows a, b, d are now measurable; `whoami` for row b is blocked by #280) — operator lane.
- `remove-secret.sh NODES_HUMAN_DECISION_TOKEN_SECRET` on thor, sequenced after the sitting — operator.
- #277 service-token principal, #280 whoami, #279 admin-write path + nodes-op.sh bearer, #281 `NODES_API_URL` default, #276 provision false positive — bugs to schedule.
- #278 move every production secret into grant — the pattern this cycle started.
- cultureflare#55 (`tunnel_token`, `--shushu`), grant#28 (rename compat, unit path note) — upstream.
- The four bot replies to historical operator comments (SCRUM-8 #10308–#10310 and the earlier drafts) are a side effect of the self-echo switch; review and delete noise — operator.
- Delete the sealed session cookie `NODES_CULTURE_DEV_CF_AUTHORIZATION` on spark when no longer needed — operator.
