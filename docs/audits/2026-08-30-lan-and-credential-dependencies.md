# LAN reachability and personal-credential dependencies (2026-08-30)

Recorded during the `presentable-floor-before-oauth` cycle (task t2, frame
claim c16) as the **input to the OAuth / login-from-anywhere cycle** (#111,
issue #6, #235). Nothing here is changed by this cycle; each line is where the next
one starts. Every item was read live on 2026-08-30 against prod revision
`66a3b26` (`GET http://thor:18080/v1alpha1/version`).

## Reachability — everything is LAN or tailscale, HTTP only

| Surface | Address | Where it is set | Read as |
|---|---|---|---|
| Control-plane API + web UI | `http://thor:18080` = `192.168.1.146` (LAN), `100.105.216.63` (tailscale); no TLS | `deploy/prod/compose.thor.yml` | `curl /v1alpha1/version` |
| Unauthenticated GET surfaces | runs, ledger, tickets (`/v1alpha1/tickets/{id}`), human tasks, actors, workflows, schedules | `internal/api/server.go` route table; no auth middleware on GET | any LAN host |
| Runner service | `http://192.168.1.146:17070` (thor), orin equivalent; bearer secret from `NODES_RUNNER_SECRET_FILE` | `~/.culture-nodes/runner.env` (`NODES_RUNNER_LISTEN`) | `systemctl --user show nodes-runner` |
| Worker → API callback | `NODES_CALLBACK_BASE_URL=http://thor:18080` | prod-worker-1 env on thor and orin | `docker inspect prod-worker-1` |
| Sweep → API | `NODES_API_URL=http://192.168.1.146:18080` (must be container-resolvable: LAN IP, not hostname) | `~/.culture-nodes/runner.env` on thor and orin | `grep NODES_API_URL` |
| Registered actor endpoints | codex-thor `192.168.1.146:8086`, codex-orin `192.168.1.138:8086`, jira-comment `.146:8089`, notify-discord `.146:8088`; spark's developer/intake/planner/verifier/human-ops/qwen-developer at `100.127.105.72` (revisions 2–3 after the DHCP move, #248); revisions 1 still list `192.168.1.157` | `deploy/prod/register-actor.sh` rows | `GET /v1alpha1/actors` |
| Ticket page link (after t16) | `NODES_UI_BASE_URL` will be one of the addresses above | `internal/store/postgres/jiraticketreport.go:63` | Jira comment on the ticket |

Consequence: a Jira or Discord reader outside the LAN/tailnet receives links
they cannot open; an actor off-LAN cannot read its ticket page.

## One shared write secret

`NODES_HUMAN_DECISION_TOKEN_SECRET` (thor `~/.culture-nodes/prod.env`) gates
page replies, frame posts, freeze, human-task decisions, reviews and grades.
It is handed to humans by the operator and to the developer lane's
`claude_env` (`docs/operations/spec-chain-lane.md` §2) — an agent writes with
the human credential. Identity is a free-text `replier` / `decider_actor_id`;
nothing binds it to a person (`internal/api/inbound_transport.go:29` hardcodes
party kind `actor`; `grades.go` is unauthenticated; `internal/auth/` holds only
`doc.go`).

## Jira identity — the operator's personal account is the system

One Basic-auth pair — accountId `557058:dbaf9fdd-70a0-4779-ae4f-12fb6c6d73c9`
("Ori Nachum", `/rest/api/3/myself`) — is used by:

- the sweep (`~/.culture-nodes/runner-secrets.env` on thor and orin; re-granted
  2026-08-30 after #253),
- the jira bridge (`~/.culture-nodes/jira-bridge-jira.env` on thor),
- both operator skills (`/jira`, `/jira-status`, which `ssh thor` to read it).

Since 2026-08-30 the same id is also `jira_bot_account_id` in
`PR_UPKEEP_REPOSITORIES`, so the operator's own comments and transitions are
self-echo: the operator answers on the ticket page, not on the board, until
issue #235 gives replies their own identity. Every system comment carries the
operator's display name.

## Operator-only lanes

- `ssh thor` / `ssh orin` for re-grants, `/jira*` skills, and harvesting
  handover refs as the engine accounts (`culture-codex@`, `culture-claude@`).
- `sudo` bootstrap of engine accounts (`deploy/prod/lanes/unix-user.sh`).
- `devague confirm` in the custody checkout on spark.
- GitHub credentials issued from the operator's accounts: `GITHUB_TOKEN_WORKER`
  (claude push bridge), tracker `GITHUB_TOKEN` (human-inbox), sweep
  `GITHUB_TOKEN` + `SONAR_TOKEN` (runner-secrets.env).

## What #111 / #6 inherit

1. Human identity for the web UI and `/tickets/:id` (login from anywhere) with
   TLS and auth in front of `thor:18080`.
2. Per-user page replies (#235) so the Jira bot id can stay set without
   silencing the operator.
3. Binding ledger origin to the authenticated caller — the custody thread
   stated in #117, #183 and #6, owned once.
4. Stable addresses (name or reservation) so registrations stop decaying
   (#121 / #226).
