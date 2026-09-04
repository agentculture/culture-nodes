# harness-interop-pi-qwen

> Culture Nodes supports Pi and Qwen Code as actors again: each runs as its own OS account on thor and orin through the existing actor contract, a workflow can name any of them as a node's actor, and a run records which harness and model served it — so a flow that sends the same request through several actors can compare them
> instruction: verify by: GET /v1alpha1/actors lists the four actors with harness/model tags; one pi run and one qwen run on thor or orin appear in the ledger with usage.model = unsloth/Qwen3.8-27B-NVFP4

## Audience

- the operator dispatching through nodes-op.sh / the web UI, the lobes-cli and colleague maintainers who want a controlled harness-vs-harness comparison on one served model, and the next adapter author reading adapters/pi as the non-ACP reference

## Before → After

- Before: today only codex runs on thor/orin (culture-codex), qwen runs only on spark and is parked behind #228, pi is not an actor anywhere, actor metadata carries no harness/model tag, and 'which harness did this run use' is answered by reading the `actor_key`
- After: company/pi-thor, pi-orin, qwen-thor and qwen-orin are registered, healthy actors that nodes-op.sh assign and any workflow definition can target like codex-thor today; each actor row carries harness=, model= and `model_endpoint`=; a permission-starved or no-write session reports a distinct outcome, never completed

## Why it matters

- nodes is meant to own durable orchestration while harnesses stay interchangeable workers; without two more harnesses on the same model the ledger cannot say which harness is better at what, and lobes/colleague changes cannot be evaluated under one orchestration model (issue #294's follow-up)

## Requirements

- a sixth adapter, adapters/pi (pi-bridge), drives @earendil-works/pi-coding-agent 0.84.2 as one pi process per invocation over its native JSONL surface (--mode json print run, or --mode rpc) and ports the byte-identical core (preflight.py, dialin.py, deployment.py, reap.py) from the qwen sibling unchanged
  - instruction: implement as adapters/pi: copy adapters/qwen minus acp/, replace `qwen_cli.py` with `pi_cli.py` over 'pi --mode json -p' followed by the instruction text (cwd = allowlisted repo, --no-session, --provider/--model from config, -a to trust the checkout only if the operator config says so); ship tests/`fake_pi.py`; keep dependencies = \[\]
  - honesty: adapters/pi/tests run offline against a fake pi (`fake_pi.py` emitting the documented json-mode events) and a bridge dispatch of a recorded event stream yields outcome, usage.model and `workspace_measured`; preflight.py/dialin.py/deployment.py/reap.py are byte-identical to adapters/qwen's (tests/lint guards green with 'pi' enrolled)
- thor and orin each gain two engine accounts, culture-qwen and culture-pi, by extending deploy/prod/lanes/unix-user.sh (engine allowlist codex|claude|qwen → +pi, a pinned pi install, a pi credential/models.json copy step) and bootstrap-accounts.sh's host→engine map (thor|orin: codex → codex qwen pi); the bridge units install into each account's systemd --user instance exactly as codex-bridge does
  - instruction: extend deploy/prod/lanes/unix-user.sh: engine allowlist + `UNIX_USER_PI_VERSION` + a pi installer that untars a pinned node 22 into ~/.local/share/pi-node and npm-installs @earendil-works/pi-coding-agent at the pinned version into it, symlinking ~/.local/bin/pi through a wrapper that prepends that node to PATH; `unix_user_roles` pi → pi-developer; bootstrap-accounts.sh thor|orin → codex qwen pi
  - honesty: ssh culture-qwen@thor / culture-pi@thor (and orin) open with the operator key; each account's ~/.local/bin has its pinned engine; id -nG lists neither sudo nor docker; the bridge unit is active under the account and no login-user unit exists
- four new registered actors — company/qwen-thor, company/qwen-orin, company/pi-thor, company/pi-orin — via register-actor.sh with --os-user plus harness=, model= and `model_endpoint`= metadata, tokens declared in the four-place ritual (prod.env on both hosts, both compose files, `audit_classification`), and rows in nodes-op.sh's actor table so assign can reach them
  - honesty: GET /v1alpha1/actors shows the four `actor_keys` with `os_user`, harness, model and `model_endpoint` metadata; audit-credentials.sh thor and orin both pass with the four new tokens classified; nodes-op.sh assign reaches each
- runs are comparable because the actor registration carries harness/model/`model_endpoint` metadata (the minimal slice of #204's lane tags), the result envelope already carries usage.model, usage tokens, `termination_reason` and `workspace_measured` (internal/actors/protocol.go), and grade records (ledger RecordGrade, #28) key on the actor — no control-plane Go change is needed to read a harness off a run
  - honesty: for one real run per harness, 'run <id>' + 'ledger <id>' expose usage.model, tokens, `termination_reason` and `workspace_measured`, and the actor row's harness= tag, with no control-plane Go change in the PR diff (internal/ untouched except tests)
- a deterministic swap fixture: the pi adapter ships a fake pi (the adapters/claude-code/tests/`fake_claude.py` pattern) and CI runs the tests/conformance kit against both fake-backed bridges with the same -input, proving the same InvocationRequest yields the same envelope shape from either harness; adapters/qwen's undelivered t6 (Dockerfile + CI workflow + conformance run) lands as part of this
  - honesty: adapter-pi.yml and adapter-qwen.yml run the tests/conformance kit against the fake-backed bridges on every PR touching the adapter; the same -input JSON is used for both and both pass
- the repo's adapter guards enrol the sixth adapter: tests/lint/`preflightsurface_test.go` advertisingAdapters, `workspacereaper_test.go`'s reap/reclaim list, `dialintransport_test.go`'s five packages, scripts/check-zero-runtime-deps.sh (globs adapters/\*/pyproject.toml — automatic), scripts/lint-all.sh's job list, and a dedicated .github/workflows/adapter-pi.yml plus adapter-qwen.yml
  - honesty: tests/lint's advertisingAdapters, workspacereaper and dialintransport lists include pi; scripts/lint-all.sh --list prints five jobs and tests/`test_lint_all.py` is updated to pin five; check-zero-runtime-deps.sh passes with adapters/pi/pyproject.toml declaring dependencies = \[\]
- operator surfaces follow: deploy/prod/deploy.sh thor|orin run `deploy_qwen_bridge` and `deploy_pi_bridge` as the accounts, actor-placement.sh knows the new ports, .claude/skills/nodes-operator SKILL.md + nodes-op.sh list the four actors, adapters/qwen/README.md's 'parked' status and the eidetic/auto-memory 'qwen lane parked' notes are superseded by the reignite decision
  - honesty: deploy.sh thor and deploy.sh orin run the qwen and pi lanes idempotently (second run reports no-op per step); adapters/qwen/README.md no longer says parked; the eidetic and auto-memory notes cite #294's reversal
- an example workflow under examples/ (harness-compare) fans one request to a list of actors chosen at run time and joins their results, so comparing harnesses is a flow an operator can run, not a property every dispatch must have; it is a possibility the design keeps open, and running it is optional
  - honesty: examples/harness-compare compiles (tests/lint examplescompile), takes its actor list as run input, and a fixture run with two fake actors produces one joined result carrying both actors' outcomes
- install-secrets.sh gains culture-qwen and culture-pi steps on thor and orin mirroring `install_codex_account_env`: the bridge auth token env file and bridge-push.env (the Contents-only push credential the qwen unit already loads via EnvironmentFile) land in each account under umask 077; without them the units start but every handover ref fails to push
  - honesty: install-secrets.sh thor and orin write the bridge auth-token env file and bridge-push.env into culture-qwen and culture-pi under umask 077 (ls -l shows 600, owned by the account), and a handover ref from one pi run and one qwen run is fetchable from origin
- a non-billable pi preflight in the codex-preflight.sh shape runs at deploy time and as the unit's ExecStartPre: pi --version matches the pin, ~/.pi/agent/models.json names the configured provider/model, GET `model_endpoint`/v1/models lists that model, the allowlisted checkout is owned by the running uid, and id -nG lists neither sudo nor docker; a failing check refuses the unit with the condition named, instead of the first dispatch discovering an unconfigured provider
  - honesty: deploy/prod/pi-preflight.sh exists with shell tests; removing models.json on a fake host makes deploy refuse with a message naming it; the unit file carries ExecStartPre pointing at it
- the pi bridge persists the raw --mode json event stream of every invocation as a per-invocation JSONL file under the state directory's pi-transcripts folder, the way the qwen bridge keeps acp-transcripts, and cancellation kills the pi process group (not just the pid) so a tool's child bash does not outlive the attempt
  - instruction: in adapters/pi: the driver tees pi's stdout JSONL to the transcript file while parsing it; spawn pi with `start_new_session`=True and cancel with os.killpg; tests use a fake pi whose bash tool forks a sleeper
  - honesty: a bridge test dispatches the fake pi, cancels mid-stream, and asserts the transcript file exists, the fake's child sleeper is gone, and the result classifies as cancelled

## Honesty conditions

- `pi_cli.py` parses `message_end` (usage + model) and `agent_end` from 'pi --mode json' and never depends on ACP; a fixture with a mid-turn tool error still classifies from the final `agent_end`, not from the tool failure
- the account's pi runs from a node >=22.19 tarball inside its own ~/.local/share/pi-node; running the account's ~/.local/bin/pi --version over ssh on thor and on orin prints the pinned version with no system node change
- ssh culture-qwen@thor curl thor:8000/v1/models lists unsloth/Qwen3.8-27B-NVFP4 without a key; the account's ~/.qwen/settings.json and ~/.pi/agent/models.json both name that endpoint and model; a live run's usage.model equals it
- a qwen session under yolo executes a shell command and the run reports it; a session that hits a cancelled permission (auto mode fixture) or a workspace-write with `changed_files`: \[\] reports a non-completed outcome — pinned by adapter tests
- no Dockerfile, bwrap, or Gondolin/container reference appears in adapters/pi or the pi deploy lane; codex-preflight-style check 8 (no sudo/docker group, owned checkout) runs for culture-pi and culture-qwen
- after the cycle, GET /v1alpha1/actors still shows company/qwen-developer at spark :8092 rev 2 with `os_user`=culture-qwen, and 'ssh culture-qwen@localhost systemctl --user is-active culture-nodes-qwen-developer' prints active
- the PR diff touches nothing under ~/git/lobes-cli and no deploy step runs 'lobes switch'; thor:8000/v1/models lists the same models before and after the cycle
- unix-user.sh's bootstrap copies ~/.pi/agent/models.json and ~/.qwen/settings.json from the login user when present and says 'absent' otherwise; the login users' pi/qwen are never referenced from an account path
- wire.`ACP_MODES` admits yolo; acp/gate.py still refuses a missing or unoffered mode; the capability document's confinement prose names the account; a cancelled permission still reaches the terminal event as its own outcome
- the delivery record names the operator's dispatch verb, the lobes-cli/colleague issue it is linked from, and the adapters/pi README section addressed to the next adapter author
- the before-state facts are cited to scope entries s5, s8 and s12 on this frame (actors list, host probes) as they were on 2026-09-04
- the delivery record shows, for the pi and qwen runs it cites, the fields a comparison would read (harness, host, outcome, tokens, changed files, handover ref) side by side, without claiming a full comparison was run
- the delivery record cites at least one ledgered run per new harness (pi, qwen) on thor or orin, each on the same served model via thor:8000
- nodes-op.sh assign succeeds for each of the four actors against a health-checked endpoint, and a workflow YAML naming actor://company/pi-thor validates and publishes without a control-plane change
- run ids for one pi and one qwen dispatch are pasted in the delivery with their outcomes and handover refs; adapter-pi.yml and adapter-qwen.yml are green on the PR; both deploy.sh outputs are pasted
- `UNIX_USER_QWEN_VERSION`=0.22.0 and `UNIX_USER_PI_VERSION`=0.85.0 in unix-user.sh; tests/fixtures/pi/ holds an event stream recorded from pi 0.85.0 --mode json on spark or thor, with the recording command in its header

## Success signals

- one real dispatch to a pi actor and one to a qwen actor on thor or orin each complete with a ledgered run and a handover ref; the conformance kit passes against the fake-backed pi and qwen bridges in CI; deploy.sh thor|orin installs both bridges into their accounts idempotently

## Scope / boundaries

- no docker/bwrap sandbox returns: the account is the confinement per #243 and the deviation record; a pi Gondolin micro-VM extension or container (docs/containerization.md) is explicitly not adopted in this cycle, and the capability document says so in its confinement prose (unix-user:culture-pi: …)
- spark's existing culture-qwen account and company/qwen-developer bridge (running, rev 2, :8092) stay as they are; this work adds thor/orin lanes beside it rather than moving it, and spark gets a culture-pi account only if the operator asks (it is not in the issue)
- lobes serving is not changed: the harnesses point at what lobes already serves (thor :8000, spark :8001); a different model per host, or per-harness model overrides, ride on input.model / pi --model and never on a lobes switch made by nodes

## Non-goals

- the control plane is not changed for this: internal/actors/protocol.go's ProtocolVersion 1.0 envelope, the tests/conformance kit, internal/preflight and the ledger authority model stay as they are — a pi run is comparable through existing metadata and usage fields, and any richer per-harness analytics is #204/#28 follow-up, not this cycle
- adapters/colleague is not touched; the issue's 'Colleague simplifies toward a bounded-task worker' is its own follow-up in agentculture/colleague, and colleague's engine choice (vllm-openai against the same lobes endpoint) is already independent of nodes
- no real pi or qwen is ever invoked in CI: every adapter test uses a fake engine, as adapter-claude-code.yml and adapter-codex.yml already do — a live dispatch is the operator's proof step, recorded in the delivery, not a check

## Assumptions

- pi needs node >=22.19.0 (package.json engines) and neither host has it — thor's system node is 18.19.1, orin has none — so the pi lane installs a pinned standalone node into the account's own ~/.local (the same shape as qwen's bundled node), never a system package; thor/orin are aarch64
- the comparison axis is one local model through different harnesses: lobes serves unsloth/Qwen3.8-27B-NVFP4 on thor :8000 (open, 200 without a key) and on spark :8001 (gateway key required — 401 without it); thor and orin's login-user qwen settings already point at thor:8000, so the account copies inherit that endpoint, and pi's ~/.pi/agent/models.json declares the same endpoint as an openai-completions provider
- the operator's 2026-09-04 installs of pi and qwen on thor/orin live in the LOGIN users' homes (0750, unreadable by engine accounts per #243), so they serve as the pinned-version reference and the provider-config template, not as the binaries the bridges run: unix-user.sh still installs the account's own pi (+ its node 22 tarball under ~/.local/share/pi-node, the layout the operator used) and qwen, and copies ~/.pi/agent/models.json + ~/.qwen/settings.json the way it copies codex's auth.json
- the engine pins stay measured, not latest: qwen pins 0.22.0 (the version the ACP surface was measured on, docs/specs/2026-08-23-qwen-bridge-acp.md) even though orin's login user now has 0.23.0; pi pins 0.85.0 (what the operator installed on thor/orin) and the json-mode event names read from the 0.84.2 docs are re-verified against 0.85.0 by the fake-pi fixture's recorded stream before the adapter tests are trusted

## Scope exploration

- `s1` — `issue #294 (gh issue view)`: asks for first-class Pi + restored Qwen Code behind one harness contract with a normalized envelope, a deterministic swap fixture and comparable execution metadata; the 'stable harness contract' it describes is the existing PRD §13 actor protocol, so the work is two adapters + deploy lanes, not a new abstraction
  - seeds: `c2`, `c8`, `c13`
- `s2` — `adapters/qwen (README, src/qwen_bridge, git log)`: the bridge exists, is stdlib-only, has 354 offline tests, drives qwen --acp over stdio, and is 'parked, and it runs' since 2026-08-27; its own README names #228 as the gate and t6 (Dockerfile+CI+conformance) as unstarted; preflight/dialin/deployment/reap are byte-identical with codex and colleague (md5 checked), callbacks/capabilities/idempotency/server/`async_runner` diverge per backend
  - seeds: `c2`, `c10`
- `s3` — `issue #228 + its 'how to treat' comment`: items 1–2 (cancelled permission must not report completed; empty workspace-write must say so) are unconditional; item 3 asks what an unattended dispatch's authorization means, notes yolo widens nothing because qwen has no kernel boundary, and lists 'a policy that auto-approves within the allowlisted worktree' as an uncosted option — the #243 account model is that policy's boundary
  - seeds: `c9`
- `s4` — `deploy/prod/lanes/unix-user.sh, account-bridges.sh, bootstrap-accounts.sh, deploy.sh`: engines are an allowlist (codex|claude|qwen) with pinned versions and per-engine installer + credential-copy steps; bootstrap-accounts.sh maps spark→claude qwen and thor|orin→codex; deploy.sh runs `deploy_codex_bridge` on thor/orin and a spark-only claude+qwen lane; the qwen unit/config template are already versioned (culture-nodes-qwen-developer.service, qwen-developer.json.template)
  - seeds: `c4`, `c12`
- `s5` — `live hosts thor/orin/spark via ssh (2026-09-04)`: engine accounts on thor/orin hold only codex 0.147.0 (codex-bridge active); login users still carry qwen 0.22.0 under ~/.local/lib/qwen-code, off PATH, settings pointing at thor:8000; thor node 18.19.1, orin no node/uv/npm at all; spark: culture-qwen runs qwen 0.22.0 + the qwen-developer unit, culture-claude runs four bridges, login user has pi 0.84.2 under nvm node 24.13.1
  - seeds: `c5`, `c4`, `c16`
- `s6` — `@earendil-works/pi-coding-agent 0.84.2 (package.json, docs/rpc.md, json.md, models.md, security.md, containerization.md)`: engines node>=22.19.0; headless via -p / --mode json (event stream: `message_update` usage, `message_end`, `turn_end`, `agent_end`) or --mode rpc (JSONL commands prompt/steer/abort, `extension_ui_request` confirm sub-protocol); no built-in sandbox and no tool-approval prompt; custom OpenAI-compatible providers via ~/.pi/agent/models.json (api openai-completions, baseUrl, compat flags, dummy apiKey for keyless servers); no ACP surface documented
  - seeds: `c3`, `c9`, `c15`
- `s7` — `internal/actors/protocol.go + tests/conformance/README.md + api/actor-protocol/README.md`: InvocationResult carries outcome, output, usage (tokens, cost, model, `thread_id`), `termination_reason`, `workspace_measured`, handover; the conformance kit is the runnable acceptance for any adapter (-endpoint, -input, -async-input); the README explicitly templates 'a fourth adapter over a different backend'
  - seeds: `c8`, `c10`, `c13`
- `s8` — `GET /v1alpha1/actors on thor (192.168.1.146:18080)`: sixteen actor rows; company/qwen-developer rev 2 at spark :8092 `os_user`=culture-qwen; codex-thor/orin :8086 `os_user`=culture-codex; metadata keys in use are only `auth_token_env`, `os_user`, `repository_identity`, role — no harness or model tag exists (#204 open), so comparison by harness today means joining on `actor_key`
  - seeds: `c6`, `c8`
- `s9` — `tests/lint (preflightsurface, workspacereaper, dialintransport), scripts/check-zero-runtime-deps.sh, scripts/lint-all.sh --list, .github/workflows`: adapter sets are enumerated by name in three Go guards; zero-deps globs adapters/\*; lint-all.sh has exactly three jobs (root, adapter-codex, adapter-claude-code) and tests/`test_lint_all.py` pins that count; only codex and claude-code have dedicated adapter workflows — qwen and a new pi adapter have none
  - seeds: `c11`, `c18`
- `s10` — `lobes (lobes status on spark; /v1/models on thor:8000, spark:8001; ~/git/lobes-cli/docs/colleague-stack.md; ~/git/colleague/README.md)`: spark :8001 serves unsloth/Qwen3.8-27B-NVFP4 behind a gateway key (401 unauthenticated); thor :8000 serves the same model plus embed/rerank openly; colleague's vllm-openai engine targets any OpenAI-compatible base URL — so all three harnesses can be pointed at one served model, which is the 'quality lab' the issue's follow-up wants
  - seeds: `c7`, `c17`, `c14`
- `s11` — `auto-memory qwen-lane-parked-colleague-preferred + docs/deliveries/2026-08-27-qwen-bridge-first-dispatch.md`: the park was an operator decision on 2026-08-29 grounded in #228 and hand-modified prod checkouts; the #243 cutover has since replaced the bridge-user premise, and the operator's instruction on this issue reverses the park — the memory and README must be updated so they do not drift
  - seeds: `c12`
- `s12` — `live hosts thor/orin re-probed after the operator's installs (2026-09-04, later)`: login users now carry pi 0.85.0 under ~/.local/share/pi-node/node-v22.23.2-linux-arm64 (PATH line in .bashrc, interactive shells only; pi's shebang needs that node on PATH) and qwen on PATH — thor 0.22.0, orin 0.23.0 (drift against unix-user.sh's 0.22.0 pin); neither host has ~/.pi/agent/settings.json or models.json, so pi has no provider yet; orin :8000 answers 401 (keyed gateway, model-gear-vllm-associate + gateway containers up) so orin does serve a local lobe; engine accounts still hold only codex
  - seeds: `c19`, `c5`, `c7`
- `s13` — `challenge pass / adjacent-systems lens: deploy/prod/install-secrets.sh + culture-nodes-qwen-developer.service`: the account-side secret copy is codex-only today (`install_codex_account_env`, line 533); the qwen unit loads %h/.culture-nodes/bridge-push.env, so a new account without that step serves but cannot push
  - seeds: `c32`
- `s14` — `challenge pass / adjacent-systems lens: scripts/lint-all.sh, tests/test_lint_all.py::test_it_lists_exactly_the_three_jobs, CLAUDE.md lint section`: the three-job scope is pinned by a test and documented as deliberate; c11 widens it, so the pin, the doc sentence and the codex-vs-claude-code invocation difference must be decided together
  - seeds: `c33`
- `s15` — `challenge pass / unstated-assumptions lens: adapters/qwen/acp/gate.py + c9/c16 interplay`: c9 admits yolo 'when the bridge runs as culture-qwen' and spark's bridge is also culture-qwen; the code is shared, the mode is per-dispatch — recorded so c16 is not read as pinning spark to the old revision
  - seeds: `c34`
- `s16` — `challenge pass / counter-evidence probe: POST thor:8000/v1/chat/completions (developer role, tools, stream usage) 2026-09-04`: the served Qwen3.8 accepts the developer role (200, usage with `reasoning_tokens`), returns real `tool_calls` with `finish_reason`=`tool_calls` under `tool_choice`=auto, and emits a usage chunk with `stream_options`.`include_usage` — so pi's openai-completions provider needs no compat.supportsDeveloperRole override and both harnesses' tool loops have a working backend; culture-codex@orin reaches thor:8000 by name and by IP (200)
  - seeds: `c7`
- `s17` — `challenge pass / unstated-assumptions lens: pi package.json (0.84.2 on spark) vs pi 0.85.0 on thor/orin; unix-user.sh pins`: two pi versions and two qwen versions are now on the fleet; the spec read 0.84.2's docs and 0.85.0's --help (same json/rpc/print/no-session/no-extensions/no-skills/no-context-files/tools flags) — the event schema itself was not re-read for 0.85.0
  - seeds: `c35`
- `s18` — `challenge pass / failure-modes lens: pi with no provider (thor/orin have no ~/.pi/agent/models.json today), deploy/prod/codex-preflight.sh`: pi's exit behaviour with an unconfigured provider was not probed; codex has a deploy-time non-billable preflight and pi has none — the first evidence would otherwise be a failed billable run
  - seeds: `c36`
- `s19` — `challenge pass / observability lens: adapters/qwen state_dir/acp-transcripts (README), pi docs/json.md`: the qwen README calls the transcript 'the first thing to read when a dispatch behaves oddly'; pi's print mode streams the equivalent to stdout and the bridge must keep it or it is gone; process-group kill is unverified for pi's bash tool
  - seeds: `c37` (rejected)
- `s20` — `challenge pass / operations lens: issue #289 (open) + deploy.sh account lanes`: deploy.sh exits 0 when the account provision lane refuses the actor checkout; the two new lanes per host inherit that defect, so a refused qwen/pi provision on thor or orin would look green — a plan-side risk to land with devague plan risk, not a spec change
  - seeds: `c12`
- `s21` — `challenge pass / concurrency lens: ADR 0013 (bridges single-threaded), issue #181, thor:8000 as the one backend`: each bridge runs one session at a time, so four new actors add at most four concurrent sessions against thor's vLLM beside colleague and spark's own load; per-actor ceilings are approximate (#181); no measurement of thor's concurrent-session capacity exists — residual, parked
  - seeds: `c22`
- `s22` — `challenge pass / security lens: docs/deviations/2026-08-29-agents-as-os-users.md 'LAN reachability, unexamined' (line 196)`: an engine account reaches every LAN service; the deviation record says reachability is not access (no DB credential, API needs the actor token) and explicitly does not claim harmlessness; two more no-approval harnesses per host widen what an injected prompt can try, not what it can reach — residual, parked
  - seeds: `c9`
- `s23` — `challenge pass / reversibility lens: register-actor.sh, internal/api actor rows (append-only), unix-user.sh rollback pair`: there is no deregister/retire verb: stopping a broken pi or qwen actor means stopping its unit (dispatches then fail as connection refused) and, for the account, the documented rollback pair; a registration row cannot be withdrawn — parked as a question of operator practice, not a spec change
  - seeds: `c6`

## Decisions

- pi's driving seam is its native --mode json / --mode rpc JSONL protocol, not ACP: pi 0.84.2 ships no ACP agent (docs/ list rpc.md, json.md, sdk.md — no acp), so the qwen bridge's acp/ package is not reusable for pi and `pi_cli.py` is written over pi's event stream (`message_end` carries usage + model, `agent_end` closes the turn)
- issue #228 is answered, not patched around: under an engine account the confinement IS the account (no sudo, no docker group, 750 home — docs/deviations/2026-08-29-agents-as-os-users.md), which is exactly the boundary #228 item 3 said was missing, so the qwen bridge admits yolo (wire.`ACP_MODES`) when the bridge runs as culture-qwen, and the pi bridge — which has no permission prompt at all (docs/security.md: no built-in sandbox, tools run as the process user) — is admitted on the same reasoning; both bridges still report a permission-cancelled or empty-write session as a distinct outcome, never completed (#228 items 1 and 2)
  - instruction: in adapters/qwen: add yolo to wire.`ACP_MODES`; in acp/transport.py record every cancelled session/`request_permission` on the driver result; in classifier.py map 'permission cancelled' and 'workspace-write with empty `changed_files`' to distinct non-completed outcomes; README trust-model section rewritten for the account
- q1 decided 2026-09-04: the pi bridge drives one 'pi --mode json -p' print run per invocation (cancel = terminate the process, matching codex exec); '--mode rpc' is parked as the continuation/steer seam for a later cut
- q2 decided 2026-09-04: the qwen bridge admits 'yolo' (wire.`ACP_MODES`) for the thor/orin qwen actors running as culture-qwen — the engine account is the confinement; #228 items 1 and 2 stay hard reporting rules regardless of mode
- q3 decided 2026-09-04: both hosts' pi and qwen harnesses point at thor:8000 (unsloth/Qwen3.8-27B-NVFP4, no gateway key), so every thor/orin run is a same-model comparison; orin's keyed local gateway and spark's :8001 are not targets in this cycle
- the two new adapter workflows (adapter-pi.yml, adapter-qwen.yml) use the in-adapter-directory lint form (the adapter-claude-code.yml shape, so each adapter's own black/isort config applies), lint-all.sh grows to five jobs, tests/`test_lint_all.py`'s exact-count pin and CLAUDE.md's 'exactly three jobs' sentence change in the same PR — a deliberate widening of a recorded decision (#123), not drift
- admitting yolo is a code change in adapters/qwen shared by every qwen bridge, so spark's qwen-developer gains it on its next redeploy; because the mode is a required per-dispatch input that is never defaulted (acp/gate.py), nothing changes on spark until an operator passes --mode yolo — c16's 'spark stays as it is' means its account, unit and registration, not its code revision
- q4 decided 2026-09-04: fairness means adjusting each harness to its own best shape, not stripping them to a common denominator — pi is a bare harness by design, qwen and colleague carry their vendored skills/context; each bridge runs its harness the way that harness is meant to run, and the run records the harness so the comparison is read with that in mind
- q5 decided 2026-09-04: thor and orin bridges listen on 8092 (qwen) and 8093 (pi), written into deploy/prod/actor-placement.sh

## Open parks

- [unknown_nonblocking] pi's project-trust rule: in non-interactive modes defaultProjectTrust=ask silently IGNORES .pi/ project resources; the repo has no .pi/ today, but the vendored .qwen/skills precedent suggests a .pi/skills lineage copy may be wanted — decide with the skills-vendoring owner (docs/skill-sources.md)
- [unknown_nonblocking] thor:8000's concurrent-session capacity with up to four bridge sessions plus colleague and spark load was not measured; a `capacity_exhausted`-style refusal class may be needed for local vLLM the way codex has one
- [unknown_nonblocking] LAN reachability from the engine accounts remains unmeasured (deviation record line 196); adding two no-approval harnesses per host does not change it but was not re-examined here
- [unknown_nonblocking] pi's usage reporting under streaming: `message_update` usage 'may remain zero when a provider only reports usage at completion'; thor:8000 does emit a final usage chunk, but whether pi requests `include_usage` was not verified — usage.model may be filled while token counts are zero
- [unknown_nonblocking] there is no actor retirement verb; how a misbehaving harness actor is taken out of rotation (unit stop vs registration) is operator practice today

## Resolved vagueness

- [unknown_nonblocking] orin's localhost:8000 'cortex' provider (from its login-user ~/.qwen/settings.json) was not probed for liveness; whether orin serves a model locally decides if orin runs are a same-model comparison or a same-host one — resolved: q3 (c22): orin's harnesses point at thor:8000; orin's own keyed gateway (401, serving the associate lobe) is not a target this cycle
- [unknown_nonblocking] the lobes gateway key on spark :8001 — whether a pi/qwen account on thor/orin is meant to reach spark's gateway (key distribution = a new secret in install-secrets.sh) or only thor:8000 (no key) — resolved: q3 (c22): only thor:8000 (no key) is targeted; no gateway-key secret is distributed
