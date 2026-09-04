# Build Plan — harness-interop-pi-qwen

slug: `harness-interop-pi-qwen` · status: `exported` · from frame: `harness-interop-pi-qwen`

> Culture Nodes supports Pi and Qwen Code as actors again: each runs as its own OS account on thor and orin through the existing actor contract, a workflow can name any of them as a node's actor, and a run records which harness and model served it — so a flow that sends the same request through several actors can compare them

## Tasks

### t1 — adapters/qwen: answer #228 and admit yolo under the engine account

- covers: c12
- acceptance:
  - wire.`ACP_MODES` includes yolo; acp/gate.py still refuses a missing, out-of-vocabulary, or agent-unoffered mode (existing tests stay green)
  - a recorded ACP transcript with one cancelled session/`request_permission` classifies as a distinct non-completed outcome (`permission_blocked`), never completed — new fixture + test
  - a workspace-write dispatch whose `workspace_measured`.`changed_files` is empty classifies as `no_changes`, never completed — new test
  - README.md: 'parked' status removed, trust-model section rewritten for the account (unix-user confinement), yolo row says admitted; 354+ tests pass with uv run pytest -q
  - files touched only under adapters/qwen/

### t2 — adapters/pi: new pi-bridge over 'pi --mode json -p' with fake pi, transcripts and process-group cancel

- covers: c2, h1, c40, h34
- acceptance:
  - adapters/pi/ exists with pyproject.toml dependencies = \[\], src/`pi_bridge`/ ported from adapters/qwen minus acp/, and preflight.py, dialin.py, deployment.py, reap.py byte-identical to adapters/qwen's (md5 equal)
  - `pi_cli.py` drives 'pi --mode json -p INSTRUCTION' with cwd = the allowlisted repo, --no-session, provider/model from config; it parses `message_end` (usage + model) and `agent_end`; a fixture with a failed tool call still classifies from `agent_end`
  - tests/`fake_pi.py` emits the documented json-mode events from a recorded stream captured from pi 0.85.0 (recording command in the fixture header); the whole suite runs offline with no real pi
  - every invocation's stdout JSONL is tee'd to `state_dir`/pi-transcripts/ per invocation; cancel kills the process group (`start_new_session` + killpg) and a test proves the fake's child sleeper is gone and the result classifies as cancelled
  - capabilities.py confinement prose starts 'unix-user:<name>:' and mentions no sandbox; README.md documents trust model, invocation input, config, systemd unit; uv run pytest -q green

### t3 — deploy/prod/lanes/unix-user.sh + bootstrap-accounts.sh: pi engine, node tarball install, pins, host map

- covers: c4, h3
- acceptance:
  - `unix_user_engine_ok` accepts pi; `UNIX_USER_PI_VERSION`=0.85.0, `UNIX_USER_QWEN_VERSION` stays 0.22.0, `UNIX_USER_PI_NODE_VERSION` pinned (22.x aarch64 tarball URL)
  - the pi install step untars node into ~/.local/share/pi-node/<node>/, npm-installs @earendil-works/pi-coding-agent@PIN into it, and writes ~/.local/bin/pi as a wrapper that prepends that node to PATH; idempotent on the pin (second run prints 'already installed')
  - bootstrap copies ~/.pi/agent/models.json (pi) the way it copies ~/.qwen/settings.json, saying 'absent' when missing; `unix_user_roles` pi -> pi-developer
  - bootstrap-accounts.sh maps thor|orin -> 'codex qwen pi' and spark stays 'claude qwen'
  - tests/`test_deploy_unix_user.py` covers the pi engine on the fake hosts (thor-fake/orin-fake naming per repo convention); files touched only: deploy/prod/lanes/unix-user.sh, deploy/prod/bootstrap-accounts.sh, tests/`test_deploy_unix_user.py`

### t4 — deploy/prod/pi-preflight.sh: non-billable deploy-time and ExecStartPre checks for a pi actor

- covers: c36, h31, c15, h15
- acceptance:
  - pi-preflight.sh <config.json> checks: pi --version equals the pin, ~/.pi/agent/models.json names the configured provider and model, GET <`model_endpoint`>/v1/models lists the model, every allowlisted checkout is owned by the running uid, id -nG contains neither sudo nor docker; each failure names the condition and exits non-zero
  - no Dockerfile, bwrap, or container reference appears in the script or its docs
  - shell tests (the codex-preflight.sh test shape) cover every check with a fake pi and a fake endpoint; files touched only: deploy/prod/pi-preflight.sh and its test file

### t5 — examples/harness-compare: fan one request to a run-time actor list and join the results

- covers: c31, h26
- acceptance:
  - examples/harness-compare/ workflow takes an actors list as run input, fans the same instruction to each, and a join node emits one result carrying every actor's outcome, usage.model, `changed_files` and handover ref
  - tests/lint examplescompile passes for it; a fixture run with two fake actors (existing test harness) produces the joined result
  - README.md in the example says comparison is optional and per-harness shape is kept (frame decision on q4); files touched only under examples/harness-compare/ and its test

### t6 — registration + operator surfaces: compose token names, audit classification, nodes-op.sh actor rows, skill docs

- covers: c6, c8, c12
- acceptance:
  - compose.thor.yml and compose.orin.yml declare `NODES_ACTOR_QWEN_THOR_TOKEN`, `NODES_ACTOR_QWEN_ORIN_TOKEN`, `NODES_ACTOR_PI_THOR_TOKEN`, `NODES_ACTOR_PI_ORIN_TOKEN` in both api and worker env blocks; audit-credentials.sh `audit_classification`() classifies all four
  - nodes-op.sh actor table gains qwen-thor, qwen-orin, pi-thor, pi-orin with repo paths /home/culture-ENGINE/git/culture-nodes-ROLE; nodes-operator SKILL.md lists them with harness, host, port (8092 qwen / 8093 pi)
  - deploy/prod/README.md documents the register-actor.sh invocation with --os-user and --metadata harness=, model=, `model_endpoint`= for each of the four, and register-actor.sh's help text names those three keys as the comparison tags
  - tests/deploy `registeractor_test.go` (or the existing shell test) pins that the four token names are classified; files touched only: compose.\*.yml, audit-credentials.sh, register-actor.sh (help/docs), deploy/prod/README.md, .claude/skills/nodes-operator/\*\*

### t7 — deploy.sh thor|orin qwen + pi bridge lanes, units, templates, ports, install-secrets account steps

- depends on: t3, t4
- covers: c12, h11, c32, h33
- acceptance:
  - deploy.sh thor and deploy.sh orin run `deploy_qwen_bridge` and `deploy_pi_bridge` as culture-qwen@HOST / culture-pi@HOST after the codex lane, using `account_prepare`, `stamp_revision`, uv tool install of the adapter, config rendered from a template with `__HOME__` substituted on the target, the unit installed into the account's systemd --user, and `account_register_os_user`
  - culture-nodes-pi-developer.service (ExecStartPre = pi-preflight.sh) and pi-developer.json.template exist; qwen reuses its versioned unit/template; actor-placement.sh knows 8092 (qwen) and 8093 (pi) on thor and orin
  - install-secrets.sh gains `install_qwen_account_env` and `install_pi_account_env` for thor and orin mirroring `install_codex_account_env` (bridge auth-token env + bridge-push.env under umask 077)
  - a second deploy run is a no-op per step (tests/`test_deploy_account_bridges.py` fake-host run asserts idempotence); tests/deploy Go lane tests locate the new lanes by literal (no case globbing before the real case)
  - files touched only: deploy/prod/deploy.sh, lanes/account-bridges.sh, \*.service, \*.json.template, actor-placement.sh, install-secrets.sh, their tests

### t8 — guards + CI: enrol the sixth adapter, widen lint-all.sh to five jobs, adapter-pi.yml and adapter-qwen.yml with the conformance kit

- depends on: t2
- covers: c10, h9, c11, h10
- acceptance:
  - tests/lint/`preflightsurface_test.go` advertisingAdapters, `workspacereaper_test.go`'s reap/reclaim list and `dialintransport_test.go`'s package list include pi; go test ./tests/lint passes
  - .github/workflows/adapter-pi.yml and adapter-qwen.yml exist in the adapter-claude-code.yml shape (in-directory lint via scripts/lint-all.sh adapter-pi|adapter-qwen, pytest, then the tests/conformance kit against the fake-backed bridge started in the job with the SAME -input JSON for both)
  - scripts/lint-all.sh --list prints five jobs; tests/`test_lint_all.py` pins five; CLAUDE.md's lint section replaces 'exactly three jobs' with the five and cites the frame decision (c33)
  - scripts/check-zero-runtime-deps.sh passes with adapters/pi included; no real pi or qwen is invoked anywhere in CI
  - files touched only: tests/lint/\*.go, scripts/lint-all.sh, tests/`test_lint_all.py`, CLAUDE.md, .github/workflows/adapter-pi.yml, .github/workflows/adapter-qwen.yml

### t9 — operator hand-turns: bootstrap culture-qwen + culture-pi on thor and orin, secrets, deploy, register four actors, verify host-level honesty conditions

- depends on: t1, t2, t7, t8, t6
- covers: c4, h3, c6, h5, c15, h15, c16, h16, c17, h17, c29, h29, c12, h11, c32, h33
- acceptance:
  - bootstrap-accounts.sh thor and orin run (one #294 comment per typed sudo naming host and command); ssh culture-qwen@HOST and culture-pi@HOST open with the operator key on both hosts
  - install-secrets.sh, then deploy.sh thor and deploy.sh orin (with `NODES_API_URL` exported to the LAN address) complete; a second deploy run pastes no-op per step; #289's exit-0 defect is checked by reading each lane's output, not the exit code
  - register-actor.sh ×4 with --os-user and harness/model/`model_endpoint` metadata; GET /v1alpha1/actors shows the four rows; audit-credentials.sh thor and orin pass
  - pasted outputs in docs/audits/2026-09-DD-harness-interop-cutover.md: is-active under each account, id -nG, ~/.local/bin/pi --version and qwen --version per account, curl thor:8000/v1/models from each account, spark's qwen-developer still active at rev 2, thor:8000 model list unchanged, nodes-op.sh assign smoke to each of the four succeeds, a workflow YAML naming actor://company/pi-thor validates and publishes

### t10 — proof dispatches: one pi run and one qwen run on thor/orin, read run + ledger, grade, record actor-quality notes

- depends on: t9
- covers: c28, h23, c30, h25, c8, h7, c26, h27
- acceptance:
  - nodes-op.sh assign pi-thor (or pi-orin) and qwen-thor (or qwen-orin) with the same scoped brief and --handover; both runs complete with a ledgered run and a fetchable handover ref (git fetch from the account's clone or origin)
  - run <id> + ledger <id> for both expose usage.model = unsloth/Qwen3.8-27B-NVFP4, token counts, `termination_reason`, `workspace_measured`, and the actor row's harness= tag; a side-by-side table (harness, host, outcome, tokens, changed files, handover ref) is written for the delivery without claiming a full comparison
  - a grade record per run through the approval surface, and a /remember actor-quality note per harness (actor, task kind, verdict, why)
  - the briefs sent to the pi and qwen actors are real scoped work from this cycle (for example the adapters/pi README review or the docs/deliveries draft), not smoke prompts — the new actors are exercised as workforce, and their outputs are harvested or rejected like any other task

### t11 — close the loop: validate-delivery, delivery record, memory + docs updates, version bump, PR

- depends on: t10, t5
- covers: c23, h18, c25, h20, c30, c12, h11
- acceptance:
  - /validate-delivery files evidence per honesty condition; docs/deliveries/2026-09-DD-harness-interop-pi-qwen.md names the audiences (operator dispatch verb, lobes-cli/colleague follow-up issue, adapters/pi README for the next author), cites the before-state to scope entries s5, s8, s12, pastes the two run ids, the four-run-capable table from t8, the green adapter-pi.yml/adapter-qwen.yml checks and both deploy outputs
  - auto-memory qwen-lane-parked note and the in-repo eidetic note cite #294's reversal; git diff touches nothing under ../lobes-cli; version bumped (minor) with CHANGELOG entry; PR opened via the cicd skill and #294 linked
  - docs/triage disposition rows added for any issue this cycle opens so the lint triage step stays green

## Risks

- [unknown_nonblocking] issue #289: deploy.sh exits 0 when an account provision lane refuses; the two new lanes per host inherit it, so t7 reads each lane's output rather than the exit code (task t9)
- [unknown_nonblocking] thor:8000's concurrent-session capacity with up to four bridge sessions plus colleague/spark load is unmeasured; a local-vLLM capacity refusal class may be needed later (task t10)
- [unknown_nonblocking] pi 0.85.0's json-mode event schema was read from the 0.84.2 docs; t2's recorded fixture from 0.85.0 is the check — if the events differ, `pi_cli.py` follows the recording, not the docs (task t2)
- [unknown_nonblocking] codex dispatch lanes: one task per host, .git read-only (harvest via --handover), no Postgres suites; t3b/t4 Go lane tests need Go on the host (thor has it) — budget one operator fixup per package
- [unknown_nonblocking] widening lint-all.sh from three to five jobs changes what 'lint is green' means for every caller; CLAUDE.md and the pinned test move together in t4 or CI goes red on the count (task t8)
- [unknown_nonblocking] pi's streamed usage may report zero tokens if pi does not request `include_usage`; t8's table records tokens as measured and says so if zero (task t10)
