# Build Plan — codex-bridges-thor-orin

slug: `codex-bridges-thor-orin` · status: `exported` · from frame: `codex-bridges-thor-orin`

> Managed Codex actor bridges run on thor and orin: each host runs a codex-bridge systemd user service over authenticated codex exec, registered append-only as company/codex-thor and company/codex-orin in thor's actor registry, dispatchable from either worker with per-host bearer tokens, gated by a non-billable preflight, and proven by a two-node smoke workflow through thor's normal API and ledger

## Tasks

### t1 — Non-billable codex preflight script (deploy/prod/codex-preflight.sh) + fake-based tests

- instruction: New files only: deploy/prod/codex-preflight.sh + a test under tests/deploy/. Bash, no third-party deps. Checks in order: explicit codex binary exists+executable at the path given by the bridge config, 'codex --version' parses, 'codex login status' reports authenticated, every `repo_allowlist` path is a git checkout (git -C <p> rev-parse), state dir writable, and refuse non-loopback bind without `auth_token`. Distinct exit message per failure. Never invoke codex beyond --version/login status (non-billable). Mirror the fake-executable test technique from adapters/codex/tests/`test_codex_cli.py` (TestRunSyncAgainstAFakeExecutable).
- covers: c5, h5
- acceptance:
  - Refuses startup with a distinct message per failure class — missing/wrong binary at the explicit configured path, unauthenticated 'codex login status', an allowlist entry that is not a git checkout, an unwritable state dir, a non-loopback bind without `auth_token` — each exercised in tests via fake codex executables
  - Runs zero billable codex invocations (only --version and login status) and records the measured codex version at start

### t2 — codex-bridge systemd user unit + per-host bridge config templates (deploy/prod)

- instruction: New files only: deploy/prod/codex-bridge.service (systemd user unit) + deploy/prod/codex-bridge.json.template (or per-host generation in-lane) + a definition test under tests/deploy/. Model on deploy/prod/nodes-runner.service. ExecStartPre=codex-preflight.sh; Restart=always; EnvironmentFile=%h/.culture-nodes/codex-bridge.env carrying `CODEX_BRIDGE_AUTH_TOKEN`. Config: `codex_bin`=~/.local/bin/codex explicit, `codex_env`.`CODEX_HOME` per host, host=0.0.0.0 port=8086, `always_async`=true, `state_dir`=~/.culture-nodes/codex-bridge-state, `repo_allowlist`=\[~/git/culture-nodes-agent\], `default_sandbox`=read-only (q3). Definition test asserts these keys and greps that no token literal appears.
- covers: c2, c3, c12
- acceptance:
  - Unit runs the preflight as ExecStartPre (startup refused on failure), Restart=always, token via EnvironmentFile — a service-definition test asserts these properties and that no token material appears in unit or config
  - Config template pins explicit ~/.local/bin/codex, the host `CODEX_HOME`, port 8086, `always_async`=true, durable state dir, `repo_allowlist`=\[~/git/culture-nodes-agent\], `default_sandbox`=read-only per the q3 decision, and exposes no app-server/WebSocket surface

### t3 — deploy.sh bridge lane: archive-independent install, agent checkout provisioning, nodes CLI ship

- instruction: Single file: deploy/prod/deploy.sh (plus helper functions in it). Add a bridge lane after the runner lane: (1) uv tool install --force ./culture-nodes-prod/adapters/codex so the tool venv is archive-independent (c21); (2) provision ~/git/culture-nodes-agent — clone github.com/agentculture/culture-nodes if absent, else git fetch + fast-forward only if clean, refuse dirty/diverged with a clear message (q2/h6); (3) install unit+config+env, daemon-reload, restart, enable, wait-active (mirror runner block); (4) build cmd/nodes locally + scp to ~/.culture-nodes/bin/nodes on each host (no Go remotely — c19). Zero changes under adapters/codex/src (h2).
- depends on: t1, t2
- covers: c3, h2, c6, h6, c19, h17, c21, h19
- acceptance:
  - Bridge installs as a persistent uv tool into ~/.local from the shipped archive and keeps running after the archive is deleted — re-deploy reinstalls and restarts the unit (h19)
  - Clones github.com/agentculture/culture-nodes to ~/git/culture-nodes-agent on first run, fast-forwards a clean checkout on later runs, and refuses a dirty/diverged checkout with a clear message leaving it untouched (h6)
  - Builds the Go nodes CLI locally and ships it to both hosts (scp, same aarch64 pattern as the runner fallback); a fresh non-login shell on each host can run it (h17)
  - git diff over the branch shows zero changes under adapters/codex/src (h2)

### t4 — Bridge token generation + secret installation lane (install-secrets.sh extension)

- instruction: Single file: deploy/prod/install-secrets.sh. Add a bridge-token lane: generate two distinct tokens (openssl rand -base64 32), write ~/.culture-nodes/codex-bridge.env on the owning host and append `NODES_ACTOR_CODEX_THOR_TOKEN` / `NODES_ACTOR_CODEX_ORIN_TOKEN` to prod.env on BOTH hosts — all via ssh stdin umask 077, never argv (h11); refuse overwrite without FORCE=1, mirroring the existing prod.env discipline.
- covers: c12, h11
- acceptance:
  - Generates two distinct per-host bridge tokens, lands them in mode-0600 files over ssh stdin (never argv), refuses overwrite without FORCE=1 — matching the existing prod.env discipline
  - A repo-wide grep and a bridge/worker log inspection find no token material; actor rows carry env var names only (h11)

### t5 — Worker env wiring: both codex token envs in both compose files

- instruction: Two files: deploy/prod/compose.thor.yml and compose.orin.yml — add `NODES_ACTOR_CODEX_THOR_TOKEN`: ${`NODES_ACTOR_CODEX_THOR_TOKEN`:-} and `NODES_ACTOR_CODEX_ORIN_TOKEN`: ${`NODES_ACTOR_CODEX_ORIN_TOKEN`:-} beside the existing `NODES_ACTOR_CLAUDE_TOKEN` line in each worker env block. Cite internal/worker/registry.go's missing-env loud failure (and its test) in the task notes for h4.
- covers: c4, h4
- acceptance:
  - compose.thor.yml and compose.orin.yml worker env blocks both pass `NODES_ACTOR_CODEX_THOR_TOKEN` and `NODES_ACTOR_CODEX_ORIN_TOKEN`
  - The loud-failure path for a missing token env is covered by a cited registry test (worker: actor requires credential from environment variable ... which is not set)

### t6 — Idempotent IP-based actor registration helper + tests

- instruction: New files only: deploy/prod/register-actor.sh + test under tests/deploy/. Inputs: actor key, endpoint URL (numeric IP required — refuse hostnames, c20), `auth_token_env` name. Reads latest revision for the key; identical endpoint+metadata -> no-op; changed -> INSERT with revision+1. psql over the existing compose exec pattern; INSERT only, no UPDATE/DELETE (h7). Registration commands for company/codex-thor -> thor-IP:8086 and company/codex-orin -> orin-IP:8086, each naming only its own token env.
- covers: c8, h7, c20
- acceptance:
  - Running the helper twice with unchanged endpoint+metadata leaves the actors row count unchanged; a changed endpoint appends a new revision row; the helper contains no UPDATE or DELETE statement (h7)
  - Registers numeric LAN IPs and refuses hostname endpoints (containers cannot resolve them — c20); registrations for company/codex-thor and company/codex-orin each reference only their own token env var

### t7 — Codex AGENTS.md guidance in-repo

- instruction: New file: AGENTS.md at repo root (codex reads it by convention; nodes doctor maps colleague->AGENTS.colleague.md so this is additive — verify doctor still passes). Content: `NODES_API_URL`=<http://thor:18080>; the shipped nodes CLI verbs for listing runs, node runs, ledger records, pending human tasks; repository invariants (never edit vendored .claude/skills, version-bump on PRs); sandbox expectation: sessions default read-only, write tasks are explicit and their diffs are harvested by the operator (q3). Keep it concise; markdownlint clean.
- covers: c14, h13
- acceptance:
  - Documents `NODES_API_URL`=<http://thor:18080>, the nodes CLI verbs for runs / node runs / ledger / pending human tasks, repository invariants, and the read-only-default sandbox expectation (q3)
  - markdownlint passes; nodes doctor still passes (colleague backend mapping unaffected)

### t8 — Two-node smoke workflow + dispatch script (manual, billable, live-only)

- instruction: New files only under examples/ (e.g. examples/codex-smoke-pair/): workflow YAML with two read-only codex nodes — one placed on company/codex-thor, one on company/codex-orin (use the <workflow>/<node> placement or uses references per node), each with sandbox read-only in input and an explicit node timeout (c22) — plus a dispatch script that creates the run via <http://thor:18080>, polls to completion, and prints the two proposed ledger claims with their actor ids. Script header states it is billable and live-only; verify no .github/workflows file references it (h12).
- covers: c18, h8, c13, h12, c22, h20
- acceptance:
  - Workflow YAML validates through the compiler; both codex nodes declare read-only sandbox (h8) and an explicit node timeout (h20/c22)
  - The dispatch script states it is billable; no CI workflow file references the smoke run or a real codex binary (h12)

### t9 — Operator docs: bridge section in deploy/prod/README + runbooks + AC mapping

- instruction: Single file: deploy/prod/README.md. Add a codex-bridge section: architecture (host bridges beside containerized workers), install/deploy/verify commands, the unbounded-concurrency stance + orin placement guidance (h21), runbooks (codex re-login on auth expiry; checkout harvest-then-reset after write tasks per q3; token rotation = new token file + prod.env update + restarts), and an issue-#14 acceptance-checkbox -> frame-claim mapping table (h14) stating the measured 2026-08-11 before-state (h15).
- covers: c23, h21, c15, h14, c16, h15, c17
- acceptance:
  - Documents the unbounded-concurrency stance with orin placement guidance (h21), the codex re-login runbook, the checkout harvest/reset lane (q3), and token rotation
  - Carries an issue-#14 acceptance-checkbox -> frame-claim mapping table (h14) and states the measured 2026-08-11 before-state, not the issue's stale text (h15)

### t10 — Production rollout on thor + orin (ops)

- instruction: Ops task (operator/main agent, not a code subagent): run install-secrets bridge lane, deploy.sh thor then orin, register both actors via t6's helper with numeric LAN IPs, then verify each acceptance criterion and capture evidence (systemctl output, worker->both-endpoints auth probe, actors-table before/after diff showing only two new rows, stopped-orin-bridge dispatch failure naming the actor). Do not touch model-gear containers on thor.
- depends on: t3, t4, t5, t6
- covers: c9, h9, c10, h10, h3, h4, h18, c15
- acceptance:
  - Both codex-bridge units enabled + active; after a stop + re-deploy both come back active (h3), and a full re-deploy that deletes the archive leaves both bridges serving (h19 live)
  - Both worker containers reach and authenticate to BOTH endpoint IPs (h18); removing one token env makes that actor's dispatch fail loudly at resolution (h4 live)
  - All six pre-existing actor rows and historical run/ledger records are byte-unchanged after registration (h9); nodes-runner units untouched
  - With orin's bridge stopped, dispatch to company/codex-orin surfaces an actor-unreachable failure naming that actor — no silent retry against thor (h10)

### t11 — Live acceptance: smoke run + evidence

- instruction: Ops task: run t8's smoke script live; capture run id, both node outcomes, and the two proposed ledger claims with actor attributions; dispatch one codex session that queries runs/node-runs/ledger/human-tasks via the installed nodes CLI against <http://thor:18080> and capture the transcript (h13). Evidence lands in the delivery summary; a mid-smoke failure is reported faithfully, never smoothed over.
- depends on: t7, t8, t10
- covers: c1, h1, c18, h13, h16, c17
- acceptance:
  - One read-only codex node completes on each host through thor's normal API; the ledger shows exactly two proposed claims attributed to company/codex-thor and company/codex-orin (h1)
  - A codex session dispatched through a bridge reports runs, node runs, ledger state, and pending human tasks from <http://thor:18080> with the installed nodes CLI (h13 live)
  - Capacity is demonstrated, not asserted: at least one real workflow node completed on each codex actor (h16)

## Risks

- [unknown_nonblocking] orin has ~8 GiB RAM free and bridge concurrency is unbounded (c23) — concurrent codex sessions beside the worker could OOM; containment is placement discipline, measurable only live
- [unknown_nonblocking] codex ChatGPT auth can expire under a long-running unit — preflight re-checks only on restart; mid-life expiry surfaces as invocation failures until an operator re-runs codex login
- [unknown_nonblocking] the standalone codex install self-updates via the 'current' symlink — the measured version can drift under a running bridge and an update may require re-auth
- [unknown_nonblocking] no cost guardrails on codex dispatch (enforcement is PRD Phase 4; #12 item 3 is observation-only) — a runaway workflow burns ChatGPT quota unmetered
