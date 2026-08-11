# Delivery Summary — codex-bridges-thor-orin

plan: `codex-bridges-thor-orin` · run: `partial` · date: `2026-08-11`
baseline: `devague summary skeleton`

## Intent

Deploy managed Codex actor bridges on thor and orin (issue #14): each host
runs a codex-bridge systemd user service over authenticated `codex exec`,
registered append-only as `company/codex-thor` and `company/codex-orin` in
thor's actor registry, dispatchable from either worker with per-host bearer
tokens, gated by a non-billable preflight, and proven by a two-node smoke
workflow through thor's normal API and ledger. Executed as an 11-task,
4-wave `/assign-to-workforce` run (8 parallel wave-1 agents, one wave-2
agent, waves 3–4 operator-run), with `/deviate` armed and live production
acceptance included.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — Non-billable codex preflight script (deploy/prod/codex-preflight.sh) + fake-based tests
- `t2` — codex-bridge systemd user unit + per-host bridge config templates (deploy/prod)
- `t3` — deploy.sh bridge lane: archive-independent install, agent checkout provisioning, nodes CLI ship
- `t4` — Bridge token generation + secret installation lane (install-secrets.sh extension)
- `t5` — Worker env wiring: both codex token envs in both compose files
- `t6` — Idempotent IP-based actor registration helper + tests
- `t7` — Codex AGENTS.md guidance in-repo
- `t8` — Two-node smoke workflow + dispatch script (manual, billable, live-only)
- `t9` — Operator docs: bridge section in deploy/prod/README + runbooks + AC mapping
- `t10` — Production rollout on thor + orin (ops)
- `t11` — Live acceptance: smoke run + evidence

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | `deploy/prod/codex-preflight.sh` + 16 fake-executable Go tests; plus a post-plan fix: accepts `CODEX_BRIDGE_AUTH_TOKEN` from the environment (commit `5fb0aca`) after the config-only check falsely refused the designed env-file token path on the first live deploy |
| `t2` | delivered | `deploy/prod/codex-bridge.service` (ExecStartPre preflight, Restart=always, EnvironmentFile token) + `codex-bridge.json.template` (`__HOME__` substitution contract, port 8086, always_async, `default_sandbox: read-only` per q3) + 10 definition tests |
| `t3` | delivered | deploy.sh bridge lane: archive-independent `uv tool install` (verified live — bridge runs from `~/.local/share/uv`, survives `rm -rf` of the archive), agent-checkout clone/ff-only/refuse-dirty, Python `nodes` CLI install per `d1`; plus a post-plan fix: resolves the registered `actors.id` into the bridge config (commit `2a57c0d`) |
| `t4` | delivered | install-secrets.sh codex lane (per-host bridge tokens, both worker token envs on both hosts, stdin-only, FORCE-guarded) + 4 discipline tests; restructured post-merge for safe re-runs on a provisioned pair (see Mid-work Decisions) |
| `t5` | delivered | both compose worker env blocks carry both `NODES_ACTOR_CODEX_*_TOKEN` envs + parse test; verified live via `docker inspect` on both workers |
| `t6` | delivered | `deploy/prod/register-actor.sh` (INSERT-only, IPv4-only, namespace-scoped) + 8 tests; live: both actors registered at revision 1, re-run printed `unchanged (revision 1)` |
| `t7` | delivered | `AGENTS.md` documenting the real Python CLI query surface (its verification of `cmd/nodes` produced deviation `d1`); `nodes doctor` unaffected |
| `t8` | delivered | `examples/codex-smoke-pair/` (workflow with node timeouts + read-only sandbox, `CONFIRM_BILLABLE` gate, README) + offline compile test; no CI reference to the billable lane |
| `t9` | delivered | `deploy/prod/README.md` codex section: architecture, install/verify, concurrency stance, three runbooks, issue-#14 AC→claim mapping, measured 2026-08-11 before-state; one filename reconciled post-merge (`run-smoke.sh`) |
| `t10` | delivered | Both bridges active+enabled+healthz on thor and orin, surviving restart; six pre-existing actor rows byte-identical and run/ledger counts unchanged (h9); both workers carry both tokens (h4) and cross-host reachability holds both directions (h18); nodes-runner units untouched; honest downed-actor failure proven (h10: `dial tcp 192.168.1.138:8086`, no failover) |
| `t11` | partial | Headline acceptance met: run `01KZS6XX6EA82PDT8GDN74YCBV` completed with both codex nodes succeeded and exactly two proposed ledger claims attributed to the two distinct registered actor ids (h1, h16). The in-session CLI check (h13) is unsatisfiable on these hosts: codex sandboxes cannot exec shell at all (`bwrap: loopback: RTM_NEWADDR EPERM`) in read-only AND workspace-write — tracked as issue #18; the CLI itself works from the hosts and from spark |

## Mid-work Decisions

- `d1` — t3 ships the Python nodes CLI (`uv tool install culture-nodes`, the REST client with run/node-runs/ledger/human-tasks verbs) as the host query surface, instead of building+scp'ing the Go `cmd/nodes` binary — the Go binary has no query verbs at all (run/inspect are stubbed 'not implemented yet'; its real verbs are serve/scheduler/worker/validate/migrate, none needed on hosts). Recorded via `/deviate`, user-approved.
- install-secrets.sh re-run semantics (no deviation record — an implementation correction inside t4's own acceptance intent, made by the merging agent at rollout): the shipped script aborted at the prod.env keep-refusal before the codex lane could run, would have rotated live runner secrets unconditionally, and evaluated FORCE remotely where ssh never forwards it. Fixed so keep-existing refusals continue, runner secrets are guarded with local-mirror consistency, FORCE propagates, and a kept bridge token never overwrites prod.env lines (commits `9d461d8`, `3345326`, `3ac4398`).
- Zombie work-item cancellation: run cancel left a leased work item re-dispatching billable sessions against a cancelled run (attempt 22); after permission-classifier blocks on both mitigations, the user explicitly approved a one-row `UPDATE work_items SET state='cancelled'` in prod. Filed as issue #19.
- Four follow-up issues filed during execution: #16 (terminal-commit failures invisible + sequence-ratchet + no retry budget), #17 (deploy.sh stale-runner scp swallow), #18 (bwrap blocks shell in codex sandboxes on these hosts), #19 (cancel doesn't reap waiting/leased work items).
- Cost note, reported faithfully: the two engine-loop incidents burned roughly 25 discarded ~20-second codex sessions before being stopped; the three accepted acceptance runs used 5 more.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t3` (`d1`) | t7's verification of cmd/nodes (main.go commands map + modes.go) measured that the plan's assumption behind c19/h17 — 'the installed nodes binary lists runs, node runs, ledger, human tasks' — is unsatisfiable by the Go binary; the query surface actually shipped in this repo is the Python `culture_nodes` CLI, published to PyPI, installable host-side with the uv already present from the headspace lane | acceptable |
| `t1` | h5's auth check as planned ("non-loopback bind with no auth_token" read from config) contradicted the also-planned no-token-in-config template; fixed to accept the env-var path the unit actually uses — caught by the first live deploy, no record needed beyond the fix commit | acceptable |
| `t4` | delivered script was not actually re-runnable on a provisioned pair (aborted before the codex lane); restructured at rollout — same contract, corrected mechanics | acceptable |
| `t11` | h13's in-session CLI query cannot pass while bwrap cannot create its sandbox netns on these hosts (read-only and workspace-write both fail before any command runs); the capability exists host-side, the in-session lane is blocked at the host-configuration layer | needs-follow-up |

## Evidence

- tests: `go test ./tests/deploy/` — ok (60+ new assertions across 6 new test files)
- tests: `uv run pytest -n auto` — 112 passed
- lint: `markdownlint-cli2` on all touched markdown — 0 errors; `bash -n` on all four shell scripts — clean
- commits: `a645695..516f4a1` (28 commits on `spec/codex-bridges-thor-orin`: spec, plan, 9 task merges, 6 fix/reconcile commits, version bump 0.9.0)
- production runs: `01KZS6XX6EA82PDT8GDN74YCBV` (acceptance pass, 2 proposed claims), `01KZS6ZWY4FZW2XWXQDZ83P52C` (h13 probe, honest bwrap claims), `01KZS5R1WKW3GSQ5FTMTDE5274` (h10 honest-failure pass), `01KZS4EYR5DRWN830P2CRYE4MN` (cancelled; exposed #16/#19), `01KZS5TJE0ZR7JZEARKCM96KHS` (timed out during zombie contention)
- registry evidence: actors-before/after diff — six pre-existing rows byte-identical, two new rows (`company/codex-thor` → `192.168.1.146:8086`, `company/codex-orin` → `192.168.1.138:8086`), runs/ledger counts unchanged at rollout (9/16)
- issues: #16, #17, #18, #19 filed; #18 updated with the workspace-write finding

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| Both codex bridges run as enabled systemd user units surviving restart, preflight-gated, healthz-green | high | t10 verification transcript · `systemctl --user is-active/is-enabled` on both hosts · run `01KZS6XX6EA82PDT8GDN74YCBV` |
| A workflow run completes one codex node on each host and writes two proposed claims attributed to the correct registered actor ids (issue #14 headline AC) | high | run `01KZS6XX6EA82PDT8GDN74YCBV` ledger (2 proposed records, distinct `origin.actor_id`) |
| Registration is append-only and idempotent; pre-existing actors/runs untouched | high | `register-actor.sh` re-run output `unchanged (revision 1)` · actors before/after diff |
| An unavailable host fails its explicitly selected actor honestly, no failover | high | run `01KZS5R1WKW3GSQ5FTMTDE5274` attempt error naming `192.168.1.138:8086` |
| Bridge install is archive-independent | high | `readlink` evidence (`~/.local/share/uv/tools/...`) · bridge active across archive deletion |
| Token material appears nowhere greppable; actor rows name env vars only | high | `tests/deploy/codexsecrets_test.go` · registry rows' metadata |
| Codex sessions can query runs/ledger/tasks via the CLI from inside a dispatched session | unverified | blocked by bwrap on these hosts (issue #18) — not claimed done; CLI verified host-side and from spark instead |
| The live-test lane works via CLI, web UI, and raw HTTP | high | `uv run nodes run list` against `http://192.168.1.146:18080` · web root 200 · `/v1alpha1/runs/{id}/ledger` items=2 |

## Remaining Work / Follow-up

- `t11`/h13 in-session CLI — blocked on host sandbox configuration for bwrap (unprivileged userns/apparmor) or a codex sandbox netns opt-out; tracked in issue #18. Until then, read-only codex nodes on these hosts are analysis-only (no shell).
- Engine hardening from the live incidents: #16 (terminal-commit observability, sequence-ratchet-on-rollback, actor retry budget) and #19 (cancel must reap waiting/leased work items) — both caused real, unbounded billable loops and deserve priority.
- #17 — deploy.sh runner-binary ship failure is swallowed; stale runner risk on future runner changes.
- The two engine-loop runs (`01KZS4EYR5…`, `01KZS5TJE0…`) remain in history as failed/cancelled — deliberately left as evidence, per the append-only discipline.
