# codex-bridges-thor-orin

> Managed Codex actor bridges run on thor and orin: each host runs a codex-bridge systemd user service over authenticated codex exec, registered append-only as company/codex-thor and company/codex-orin in thor's actor registry, dispatchable from either worker with per-host bearer tokens, gated by a non-billable preflight, and proven by a two-node smoke workflow through thor's normal API and ledger
> instruction: Verify end-to-end: deploy bridge units to both hosts, register both actors, then run the two-node smoke workflow through <http://thor:18080> and check the ledger shows two proposed claims from the two distinct actor ids; systemctl --user is-active codex-bridge on both hosts before and after a restart

## Audience

- The operator (Ori) delegating work to the mesh, and both production workers (thor+orin) that dispatch actor nodes; codex sessions themselves consume the CLI/API surface from inside dispatched work

## Before → After

- Before: The conformant adapters/codex bridge exists but no instance runs anywhere; no codex actor rows or tokens exist; no bridge deployment lane exists (deploy.sh installs only nodes-runner); no host has a git checkout codex exec could run in — and the four claude actors registered from spark are currently dark, showing ad-hoc bridge processes don't survive
- After: Workflows can place nodes on company/codex-thor or company/codex-orin and get real codex exec sessions: both bridges enabled+active as systemd user units surviving restart, preflight-gated, reachable+authenticated from both workers, writing proposed ledger claims attributed to the correct registered actor

## Why it matters

- Actor capacity is the bottleneck for delegating real work through culture-nodes: zero live bridges means zero delegable workflows; a second, managed backend both restores capacity and enables cross-backend review (issue #13's independent-review direction)

## Requirements

- Deployment reuses the shipped adapters/codex bridge unmodified over stable 'codex exec' — its config surface already carries everything the issue needs: `repo_allowlist`, `codex_bin` (explicit path), `codex_env` (`CODEX_HOME`), `auth_token`, host/port (default 8086), `always_async`, `state_dir` (adapters/codex/README.md + src/`codex_bridge`/config.py)
  - honesty: Deployment requires zero changes under adapters/codex/src — configuration (bridge.json + env) alone carries the host specifics; any needed code change is a recorded deviation, not a silent patch
- Each host gets a codex-bridge systemd user unit + env file following the existing nodes-runner host-service lane: deploy/prod/deploy.sh installs binary+env+unit under ~/.culture-nodes with enable-linger, install-secrets.sh ships secrets over ssh stdin into mode-0600 files, never argv
  - honesty: After 'systemctl --user disable+stop / re-deploy', both units come back enabled and active, and survive a host reboot via loginctl enable-linger — same discipline nodes-runner.service already proves on both hosts
- Registering company/codex-thor and company/codex-orin as two distinct `actor_keys` fits internal/worker/registry.go without code changes: newest-revision-wins per key, `endpoint_ref` carries the URL, metadata.`auth_token_env` names the per-actor worker env var; both workers therefore need both `NODES_ACTOR_CODEX_THOR_TOKEN` and `NODES_ACTOR_CODEX_ORIN_TOKEN` added to compose.thor.yml and compose.orin.yml worker env blocks (today they pass only `NODES_ACTOR_CLAUDE_TOKEN`)
  - honesty: Either worker resolves and authenticates to BOTH bridge endpoints; removing one token env from a worker makes dispatch of that actor fail loudly at resolution (registry.go's missing-env error), never fall back silently
- A non-billable bridge preflight must gate startup — probed 2026-08-11: both hosts have codex-cli 0.147.0 authenticated (ChatGPT) at ~/.local/bin/codex, but that path is a symlink into the self-updating standalone install (~/.codex/packages/standalone/current/bin/codex), so the pinned version is a moving 'current' pointer; preflight must measure and record the actual version/login/allowlisted-repo/state-path facts at start, not assume them
  - honesty: Preflight runs zero billable codex invocations and refuses bridge startup on: missing/wrong codex binary at the explicit configured path, unauthenticated 'codex login status', a `repo_allowlist` entry that is not a git checkout, an unwritable state dir, or a non-loopback bind with no `auth_token` — and it records the measured codex version at start
- A dedicated git checkout must be provisioned per host for the repo allowlist: culture-nodes-prod is a git archive with no .git on both hosts, ~/git/culture-nodes-agent exists on neither (probed 2026-08-11), and codex exec refuses to run outside a git repo — the bridge never passes --skip-git-repo-check
  - honesty: codex exec runs inside ~/git/culture-nodes-agent with no --skip-git-repo-check; deploy.sh fast-forwards a clean checkout and refuses (with a clear message, leaving it untouched) when the checkout is dirty or diverged
- An idempotent registration helper appends actor rows only when endpoint/identity metadata changed, reusing an identical latest row — actors are append-only raw SQL today (four claude rows at 192.168.1.157:8086-8089 all via `NODES_ACTOR_CLAUDE_TOKEN`); broader registration API/CLI stays issue #8
  - honesty: Running the registration helper twice with unchanged endpoint+metadata leaves the actors table row-count unchanged; a changed endpoint appends a new revision row; no code path ever UPDATEs or DELETEs an actor row

## Honesty conditions

- A two-node smoke run completes one codex node per host via thor's API and the two resulting proposed ledger claims are attributed to company/codex-thor and company/codex-orin respectively
- After the cycle, all six pre-existing actor rows and every historical run/ledger record are unmodified (append-only table, no UPDATEs/DELETEs), and both nodes-runner units are untouched
- No routing or failover code lands; dispatching to a downed host's actor surfaces an actor-unreachable attempt failure naming that actor — never a silent retry against the other host
- No token material appears in the repo, compose files, deploy scripts, actor rows, or bridge/worker logs — tokens live only in mode-0600 files on the hosts and worker process env; a repo-wide grep for the generated values finds nothing
- No CI workflow file invokes the smoke workflow or a real codex binary; the smoke lane is a documented manual script that states it is billable
- A codex session dispatched through either bridge can report runs, node runs, ledger state, and pending human tasks from <http://thor:18080> with the supported CLI
- Every acceptance checkbox in issue #14 maps to at least one confirmed claim in this frame, and the exported spec covers all of them
- The before-state facts are the 2026-08-11 measured probes recorded in scope entries s1-s9, not assumptions carried from the issue text (two of which were already stale)
- Capacity is demonstrated, not asserted: at least one real workflow node completes on each codex actor during acceptance
- The smoke nodes are read-only (sandbox read-only) so a smoke run can never mutate the agent checkout, and the run goes through the normal engine dispatch path — no bypass endpoint

## Success signals

- One smoke workflow run completes one read-only codex node on each host through thor's normal API, the ledger shows two proposed claims attributed to the two distinct registered actor ids, and systemctl --user reports both codex-bridge units active after a service restart

## Scope / boundaries

- Existing Claude actor registrations (company/intake|planner|developer|verifier at spark's LAN address), historical runs, and the nodes-runner units stay untouched — the codex work adds rows and services beside them, never edits or supersedes them
- No active-active or failover claim across the two codex actors: internal/worker/registry.go resolves exactly one newest-revision endpoint per `actor_key`, so an unavailable host fails its explicitly selected actor honestly
- Token material stays out of Postgres, argv, logs, and committed files: actor rows name env vars only (metadata.`auth_token_env`), secrets ride ssh stdin into mode-0600 files per install-secrets.sh discipline
- The two-node smoke workflow is live-only and billable (codex has no offline mock engine, unlike colleague's `COLLEAGUE_ENGINE`=mock) — it is a manual verification lane, never a CI step; CI keeps running the adapter's fake-based unit suite only

## Non-goals

- Codex's experimental app-server WebSocket surface is not used — the bridge speaks stable, non-interactive 'codex exec --json' only (adapters/codex/README.md contract pin)

## Assumptions

- The stale /usr/local/bin/codex 0.55.0 the issue flags on thor no longer exists (ls fails, probed 2026-08-11) — the concern is moot, but preflight still asserts the explicit ~/.local/bin path so ambient PATH can never select a wrong binary again

## Scope exploration

- `s1` — `adapters/codex (README.md, src/codex_bridge/config.py)`: conformant actor-protocol bridge over 'codex exec --json' already ships with the full config surface deployment needs (allowlist, explicit `codex_bin`, `CODEX_HOME` passthrough, `auth_token`, port 8086 default, `always_async`, `state_dir`); proposed-only trust stance; conformance kit is manual/billable, CI runs fakes only
  - seeds: `c2`, `c11`, `c13`
- `s2` — `deploy/prod (deploy.sh, install-secrets.sh, nodes-runner.service, README.md)`: the host-service lane to extend: systemd user units under ~/.config/systemd/user with enable-linger, env files + secrets in mode-0600 ~/.culture-nodes via ssh stdin, argv-only ssh; deploy.sh currently installs only the nodes-runner unit — no bridge lane exists anywhere yet
  - seeds: `c3`, `c12`
- `s3` — `internal/worker/registry.go`: DBRegistry resolves one newest-revision endpoint per `actor_key`; credential comes from the env var named in metadata.`auth_token_env`, missing env fails resolution loudly — two distinct codex actor keys with per-host token envs fit with zero engine changes, and no active-active is expressible
  - seeds: `c4`, `c10`
- `s4` — `deploy/prod/compose.thor.yml + compose.orin.yml worker env blocks`: workers pass only `NODES_ACTOR_CLAUDE_TOKEN` today; both compose files need the two new codex token envs because either worker may dispatch either actor
  - seeds: `c4`
- `s5` — `thor host state (ssh probe 2026-08-11, read-only)`: codex-cli 0.147.0 authenticated via ChatGPT at ~/.local/bin/codex -> ~/.codex/packages/standalone/current/bin/codex (self-updating 'current' symlink, soft pin); stale /usr/local/bin/codex is gone; only nodes-runner.service active, no codex-bridge unit; culture-nodes-prod has no .git; no ~/git/culture-nodes-agent
  - seeds: `c5`, `c6`, `c7`
- `s6` — `orin host state (ssh probe 2026-08-11, read-only)`: identical shape to thor: codex 0.147.0 ChatGPT-authenticated behind the same standalone 'current' symlink, nodes-runner active, no bridge unit, no git checkout for an allowlist; ~8 GiB RAM free per topology memory
  - seeds: `c5`, `c6`
- `s7` — `thor Postgres actors table (read-only SELECT 2026-08-11)`: six rows: four claude actors at 192.168.1.157:8086-8089 sharing `auth_token_env` `NODES_ACTOR_CLAUDE_TOKEN`, plus headspace/docker and human/ori with no endpoint; all revision 1, raw-SQL registered (issue #8); spark's claude bridge processes behind those rows are currently NOT running
  - seeds: `c8`, `c9`
- `s8` — `spark local state (ss -tlnp + ps, 2026-08-11)`: no listeners on 8085-8089 and no bridge processes — the four registered claude actors are dark; the issue's 'existing Claude setup is healthy' (verified 2026-08-09) is stale, which is itself evidence that ad-hoc bridge processes without systemd management do not survive
- `s9` — `issues #8, #10, #12 (gh, 2026-08-11)`: \#8 owns broader actor/runner registration tooling (this cycle adds only a minimal idempotent helper); #10 owns claude-bridge production follow-ups incl. always-async default; #12 item 3 already expects codex usage mapping for cost visibility — no overlap conflicts
  - seeds: `c8`

## Open parks

- [unknown_nonblocking] orin has ~8 GiB RAM free and is already tight for agent sessions (machines-dev-prod-topology memory); whether concurrent codex exec sessions fit beside the worker is measurable only at runtime
