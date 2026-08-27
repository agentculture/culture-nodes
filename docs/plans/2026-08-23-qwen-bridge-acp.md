# Build Plan — qwen-bridge-acp

slug: `qwen-bridge-acp` · status: `exported` · from frame: `qwen-bridge-acp`

> culture-nodes officially supports Qwen Code as a fourth actor backend: adapters/qwen ships qwen-bridge, a PRD-13 actor-protocol implementation that drives Qwen Code over ACP (qwen --acp, stdio JSON-RPC), with the container image as the support contract (#113) and the configured repository as the agent's identity (#114), running on spark/thor/orin where qwen 0.22.0 already lives

## Tasks

### t1 — Port the 19-module shared core into the adapters/qwen package

- instruction: Copy the module set from adapters/codex/src/`codex_bridge` (the 19-module core) into adapters/qwen/src/`qwen_bridge`, renaming the package only; do NOT copy `codex_cli.py` (t2 writes `qwen_cli.py` in its place). Port the codex core tests, re-aimed at the qwen package. The design source of truth is docs/specs/2026-08-23-qwen-bridge-acp.md (c2/c12/h9).
- covers: c2, c12, h9
- acceptance:
  - the package imports cleanly and the ported core test suite runs green (0 failures) under uv run --project adapters/qwen pytest -q
  - preflight.py is byte-identical to the codex sibling's (a committed diff test asserts it; if tests/lint/`preflightsurface_test.go` enumerates bridges explicitly, the list gains qwen and the Go lint passes with all four surfaces equal)
  - the c12 absence check is recorded: ls adapters shows no qwen entry and .github/workflows contains no adapter-qwen.yml before t6 (the thor actor-list leg is dispositioned to t8 per c22)

### t2 — Build `qwen_cli.py`: the ACP client seam and the terminal-event classifier

- instruction: Build test-first against a fake ACP agent: a stdio JSON-RPC script under tests/fixtures/acp/ that replays the MEASURED shapes from the 2026-08-23 probes (scripts /tmp/acp-probe/probe.py and /tmp/acp-probe2/probe2.py on spark - re-run them if absent and regenerate the fixtures). The measured facts: initialize returns protocolVersion 1 + agentInfo qwen-code 0.22.0 + authMethods + agentCapabilities{loadSession:true}; session/new returns sessionId + modes{plan,default,auto-edit,auto}; failed tool calls end `end_turn`; cancel is a notification answered by stopReason cancelled; the terminal response carries `_meta`.qwen.branchPoint. The spec's c16/c19/c21 are the classifier contract; the v1 frame decision is the client-obligation contract (no fs/terminal handlers in the first cut). h14 (the resume probe) is a claim-side verification, not a plan target - the committed probe script satisfies it without covering a target.
- depends on: t1
- covers: c4, c16, c18, c19, c21, h3, h13, h15, h16, h18
- acceptance:
  - the classifier maps the measured terminal shapes with one passing test each: stopReason `end_turn` -> ok (including the tool-failure fixture where a failed cat still ends `end_turn` with error null), stopReason cancelled -> the 13 cancellation outcome, a JSON-RPC error on the session/prompt response -> error, no terminal response (fake agent dies) -> incomplete
  - exit-0-without-terminal-event is asserted incomplete, never ok - the crash-case test mirrors codex's `test_codex_cli.py` rule
  - the handshake gate refuses an unsupported protocolVersion or an agentInfo.version mismatch with a distinct message before serving (unit test feeds the measured initialize response with protocolVersion 2)
  - the result payload for the 72-notification turn fixture is <= 64 KB while the full session/update stream is retained in the bridge's local transcript file
  - the session mode is set from the input/preflight policy at session creation and never falls back to the measured default auto; the effective mode is exposed to the capability surface; the h14 named probe (kill process, session/load the recorded sessionId) is a committed script whose result is recorded, and `continuation_ref` is NOT implemented in the first cut
  - unknown ext methods (the craft/drainMidTurnQueue fixture, id 0) are answered leniently and the turn completes

### t3 — qwen capabilities + deployment: the measured host-facts surface

- instruction: Parse, never hard-code: the 2026-08-23 probe values (node v22.23.2 vs v18.19.1, unsloth/Qwen3.8-27B-NVFP4 vs cortex) are DIFF fixtures for the capability document, not constants in the code. The host-local model fact (c8, a confirmed assumption) is what this surface reports; c20: the measured authMethods (`OPENAI_API_KEY` env path) + per-host settings.json endpoints are the config source the surface names without leaking. Spec c10/c20 are the contract.
- depends on: t1
- covers: c6, c10, c20, h5, h7, h17
- acceptance:
  - the capability document reports qwen version, bundled node version, model identity, config source (env or settings path, WITHOUT values), and the effective session mode - parsed from the measured initialize response plus qwen --version and node --version fixtures, 0 failures
  - the context budget is reported only from a measured source; where qwen 0.22.0 does not expose it, the field is null - asserted on the orin/cortex fixture
  - the deployment leg refuses to serve invokes when a contract leg is missing: the negative boot test boots the bridge without the qwen binary and asserts the distinct message (h5)

### t4 — config.py: the #114 configured-repo load, fail-closed per leg

- instruction: The qwen prompt-file leg is AGENTS.md (the measured qwen context file on this repo). Per issue #114 (spec c7): the image carries no identity; the repo is the config; missing legs refuse with distinct messages (fail closed). The .qwen/skills surface vendored 2026-08-23 (9 lineage-marked skills) is the skills leg this repo would load.
- depends on: t1
- covers: c7, h6
- acceptance:
  - the bridge loads the culture.yaml + AGENTS.md + .qwen/skills triple from the configured repository at startup, and three negative tests (one per missing leg) assert three DISTINCT refusal messages with no invoke served
  - the capability document carries the loaded config repo + revision (the clone HEAD), asserted against a fixture clone at a known revision

### t5 — Bridge entrypoint + 13 integration: invoke/cancel/replay against the fake ACP agent

- instruction: Wire the ported core (dialin/`async_runner`/callbacks/idempotency/`scope_guard`/`session_registry` - already in the package from t1) to `qwen_cli` (t2) behind the config (t4) and capabilities (t3). Boot the server on a localhost port in tests; the 13 shapes are api/actor-protocol/README.md - do NOT extend them (c5). The entrypoint is src/`qwen_bridge`/`__main__.py` so t1's pyproject stays untouched by this task.
- depends on: t2, t3, t4
- covers: c1, c13, h10
- acceptance:
  - POST /v1/invocations against the fake ACP agent returns 200 with the classified ok result for a completed turn, and a cancelled invoke returns the 13 cancellation outcome - integration tests, 0 failures
  - an idempotent replay of a finished invoke returns the same result without re-executing the model turn (the fake agent asserts exactly one session/prompt call for the replayed invocation id)
  - the 202 async path emits the callback sequence (stable invocation id, increasing sequence) against the fake agent, and the every-`after_state`-leg check runs as commands: conformance-style invoke, boot + capability fetch, synthetic invoke

### t6 — Dockerfile + CI workflow + conformance kit script (the #113 image contract)

- instruction: Base the Dockerfile on the adapters/codex/Dockerfile pattern (python:slim + the qwen install layout). The entrypoint must NOT rely on the ssh PATH: qwen is absent from the non-interactive PATH on thor/orin (measured) - invoke the install layout directly (the image controls its own layout). Copy scripts/`run_conformance_kit.sh` from a sibling UNMODIFIED. The success-signal commands (c15/h12) are named in the workflow job names so the 0-failures / 3-of-3-hosts tokens map to checks.
- depends on: t5
- covers: c2, c5, c6, c15, h2, h4, h12
- acceptance:
  - the image builds on aarch64 (the measured uniform fleet architecture) and contains node + pinned qwen 0.22.0 + the stdlib-only python bridge; a grep of the built image filesystem finds zero credential material
  - `run_conformance_kit.sh` passes against the built image with the kit UNMODIFIED (0 failures), and the workflow mirrors adapter-codex.yml (test + lint + conformance jobs, fake-driven so no real qwen in CI)
  - the diff check passes: nothing lands outside adapters/qwen, .github/workflows/adapter-qwen.yml, and the docs this task names - api/actor-protocol/ and tests/conformance are byte-identical (git diff assertion in the workflow, enforcing the c9 non-goal)

### t7 — README: trust model, the ACP seam, build+run runbook

- instruction: Model the structure on adapters/codex/README.md (trust model, crash mapping, the invocation-input table, the capability surface, configuration). The ACP seam section is where the challenge-pass measurements live (the s10-s18 frame scope entries are the source). The why-it-matters section states the support-contract framing (#113/#114) the audience claims (c11/c14) are about.
- depends on: t5
- covers: c11, c14, c15, h1, h8, h11
- acceptance:
  - a fresh agent session, given ONLY adapters/qwen/README.md + docs/specs/2026-08-23-qwen-bridge-acp.md, states the audience, the ACP seam (client-side methods, no fs/terminal handlers), and the three fleet constraints (aarch64 uniform, host-local models, PATH absence) correctly - the h11 check
  - the build+run section is executable as written: the commands boot the bridge on a scratch checkout and the runbook names the three continuation issues from t8 (per c22, nothing is silently dropped)
  - the measured ACP facts are documented: the modes (plan/default/auto-edit/auto), the craft/drainMidTurnQueue ext method, the `_meta`.qwen.branchPoint handle, the tool-call-in-process behavior

### t8 — Continuation issues: disposition every off-checkout leg (the c22 routing, the c23 dogfood)

- instruction: Directive task carrying the operator decisions c22 (disposition rule) and c23 (workforce constraint) - decisions are descriptive, so this task covers no frame target; its contract IS the decisions. Use the agtag-backed communicate skill vendored into .qwen/skills/communicate (scripts/post-issue.sh, the culture-nodes signature resolved from culture.yaml). Post (a)(b)(c) as one issue each, tagged for the qwen-bridge work; the dogfood issue body quotes c23 verbatim (codex + claude-code on culture-nodes, qwen joins when live, NO colleague). Record the three issue numbers in the plan state and in the README runbook (t7's second acceptance). This task owns no repo files - its artifacts are the posted issues + the plan record.
- depends on: t6
- acceptance:
  - three continuation issues are posted on agentculture/culture-nodes via the agtag-backed communicate skill, each tracing back to the plan task and the spec claim: (a) live dogfood - culture-nodes work items through the codex and claude-code bridges (which already work on culture-nodes), then through the qwen bridge once it exists; the colleague bridge is OUT of use for this work (c23); (b) control-plane actor registration on thor + the h9 actor-list leg (c12); (c) fleet 3-of-3 boot + capability-doc diff against the 2026-08-23 probes (c15/h7)
  - each posted issue body states the disposition of the leg it carries (implement-here vs continuation-issue) and the plan's task list records the three posted issue numbers

## Risks

- [unknown_nonblocking] resume/`continuation_ref` is OUT of the first cut (frame park v2): the session/load cross-process measurement (h14's named probe) has not been run - any dispatch needing resume stays on the codex/claude-code bridges until it is, and the t2 committed probe script is the named verification (task t2)
- [unknown_nonblocking] the model/API JSON-RPC error shape is SYNTHESIZED in the t2 suite, not measured live (frame park v4): the only measured failure was a tool failure ending `end_turn` - the first live occurrence must be captured and diffed against the synthesized fixture before the error leg is trusted (task t2)
- [unknown_nonblocking] craft/drainMidTurnQueue semantics are unmeasured (frame park v5): the lenient empty-answer is the first-cut behavior (measured to let turns complete) - do not rely on mid-turn queueing until the real handler contract is validated (task t2)
- [unknown_nonblocking] the mode mapping (c18) is a candidate from the measured session/new modes: h15's per-mode live verification (plan-mode refusing a shell command, etc.) has not been run - the bridge must never fall back to the measured default auto, and the t2 selection logic is tested until h15 passes live (task t2)
- [follow_up] h9's thor control-plane actor-list leg is unmeasured (it lives off-checkout on thor): continuation issue (b) from t8 must run it before the qwen image is declared supported on the fleet (task t8)
