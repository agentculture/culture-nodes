# Build Plan — harness-hardening-and-compare

slug: `harness-hardening-and-compare` · status: `exported` · from frame: `harness-hardening-and-compare`

> Harness hardening and measurement shipped: the pi and qwen bridges (and every checkout bridge) refuse oversized bodies with a 413 instead of desynchronizing the keep-alive connection, a hung pi sync reports the timeout outcome instead of a 500, pi's read-only sandbox mode is enforced at the tool level or refused, CI proves the harness swap by running the §13 conformance kit live against fake-backed pi and qwen bridges with one shared input, deploy.sh recreates api and worker whenever the actor-token set changes, one reviewed command brings a new harness actor online after the root bootstrap, and the harness-compare workflow grades pi, qwen and colleague on the same served model across a declared task set.

## Tasks

### t1 — Port the bounded oversized-body refusal to every checkout bridge and bound it in notify/human-inbox

- covers: c2, h1, c18, h15, c28, h22, c13, h14
- acceptance:
  - adapters/{pi,qwen,claude-code,codex,colleague}/src/\*/server.py gain `_refuse_oversized_body`, called first in `do_POST`/`do_DELETE` before any auth or body read; the helper drains at most `MAX_BODY_BYTES` then answers 413 with class `actor_rejected_input` and Connection: close
  - notify and human-inbox helpers gain the same drain bound; diff of the helper across all seven server.py files is empty (same bytes, copied not reformatted)
  - each of the five checkout bridges has a unit test that on one keep-alive connection sends a body declaring Content-Length above `MAX_BODY_BYTES`, gets 413 + Connection: close, and a following request on a fresh connection parses cleanly; a second test declares 10x the cap, sends only the cap, and gets the 413 within the socket timeout
  - every server.py stays under 1000 lines (go test ./tests/lint -run FileLength passes); all adapter unit suites and scripts/lint-all.sh are green

### t2 — Guard pi's second sync timeout the family way and test it with a SIGTERM-ignoring fake

- covers: c3, h2
- acceptance:
  - `pi_cli`.`run_sync` catches the second TimeoutExpired after `terminate_group`, sets `timed_out`=True with empty stdout and an explanatory stderr, never sends SIGKILL, and returns a SyncRunResult the handler maps to mapping.`CLASS_TIMEOUT`
  - tests/`fake_pi.py` gains `FAKE_PI_IGNORE_SIGTERM`=1; a test with `sync_timeout_seconds`=0.15 and that flag asserts `timed_out`=True, that the child pid (from `FAKE_PI_CHILD_PID_FILE`) is still alive after the call, and that the handler's response class is timeout; the test cleans the child up itself
  - the module docstring records the never-SIGKILL stance with a pointer to `claude_cli.py` and `codex_cli.py`

### t3 — Add pi and qwen `run_conformance_kit.sh` harnesses and switch both adapter workflows to the live fake-backed run

- covers: c5, h4
- acceptance:
  - adapters/pi/scripts/`run_conformance_kit.sh` and adapters/qwen/scripts/`run_conformance_kit.sh` exist, are executable, and follow adapters/claude-code/scripts/`run_conformance_kit.sh`: scratch repo, bridge config with the fake as the binary, `start_background` on port 0, healthz wait, then go test ./tests/conformance with -endpoint, -auth-token, -input, -async-input, -bad-input, -callback-wait, -timeout, -expect-callback-retry and -require-cancellation
  - the -input, -async-input and -bad-input JSON strings are identical between the two scripts modulo the scratch repo path (a test in tests/ or a shell diff in the script's own self-check asserts it)
  - adapter-pi.yml and adapter-qwen.yml conformance jobs run the scripts (Go toolchain + uv set up as adapter-claude-code.yml does) and pass in CI; the in-process reference check remains as a separate always-on step
  - no real pi or qwen binary is installed in CI; if a fake needed a per-input answer to pass, that is recorded as a deviation, not silently added

### t4 — Recreate api and worker in both deploy lanes and fix the README rotation runbook

- covers: c7, h6, c8, h7
- acceptance:
  - deploy/prod/lanes/two-host.sh's thor sequence recreates api and worker (the 'up -d --scale scheduler=0' and 'up -d worker' calls carry --force-recreate, or an equivalent explicit recreate step) and deploy.sh's orin lane recreates api and worker too; the compose config-hash label and the `NODES_BUILD_REVISION` parity check still pass
  - tests/`test_deploy_two_host.py` asserts the recreate argv in both lanes and the existing ordering assertions (stop, migrate, up, worker) still hold
  - deploy/prod/README.md contains no 'docker compose ... restart worker' step for a token change; the rotation runbook shows the recreate form and says why restart is not enough
  - live check recorded in the PR: a deploy.sh thor with no code change moves prod-worker-1's Created timestamp (docker inspect before/after)

### t5 — Teach the account lanes the colleague engine and wire its unit, template and compose token keys

- covers: c10
- acceptance:
  - deploy/prod/lanes/unix-user.sh accepts engine 'colleague': role colleague-developer, credential file ~/.colleague/config.json copied from the login user, installer 'uv tool install colleague== followed by the pinned version' inside the account, version pin `UNIX_USER_COLLEAGUE_VERSION`; bootstrap-accounts.sh spark's engine set becomes 'claude qwen colleague'
  - deploy/prod/culture-nodes-colleague-developer.service and colleague-developer.json.template exist, modelled on the pi pair; lanes/account-bridges.sh's spark lane can deploy the colleague bridge as culture-colleague; compose.thor.yml and compose.orin.yml declare `NODES_ACTOR_COLLEAGUE_SPARK_TOKEN` in both api and worker blocks
  - tests/`test_deploy_unix_user`\*.py and tests/`test_deploy_account_bridges.py` cover the colleague engine under the fake-ssh shims, including the refusal when ~/.colleague/config.json is absent; a bootstrap is NOT run — the spark account remains a hand-turn recorded on #298

### t6 — Add the colleague slot and the measurement input object to harness-compare and move the e2e slot pin

- covers: c32, h26
- acceptance:
  - examples/harness-compare/workflow.yaml gains a fifth guarded agent slot 'colleague' on actor://company/colleague-spark with the same edges as the pi slot, and an optional top-level input object 'measurement' {`manifest_digest`, `rule_id`}; README.md documents both
  - tests/e2e/`harnesscompare_test.go`'s harnessCompareSlots and slot->actor map include colleague; the compile test asserts five guarded agent nodes; the two-actor run asserts the unset colleague slot never ran; go test ./tests/e2e -run TestHarnessCompare and ./tests/lint pass

### t7 — Define the measurement manifest: schema, digest, validator and the basic three-rule set

- covers: c21, h10
- acceptance:
  - examples/harness-compare/measurements/schema.json (JSON Schema 2020-12) defines a manifest of rules {id, category, instruction, sandbox, check {kind: grep-cites-file-line|seeded-defect-named|tests-named, expect}, anchors {5,3,1}, `runs_per_actor`} and an actors list; basic.yaml declares exactly one rule per category (locate, review, explain), sandbox read-only, `runs_per_actor` 2
  - a zero-dependency python3 module examples/harness-compare/measurements/manifest.py validates a manifest against the schema, canonicalises it to JSON and prints its SHA-256 digest; editing any rule field changes the digest; an invalid manifest exits non-zero naming the field
  - tests cover validation, canonical digest stability across YAML key order, and the refusal; a test pins that basic.yaml has one rule per category

### t8 — Lint guard: the oversized-body helper is byte-identical across all seven bridges

- depends on: t1
- covers: c13, h14
- acceptance:
  - tests/lint/`oversizedbody_test.go` extracts `_refuse_oversized_body` from every adapters/\*/src/\*/server.py that defines `do_POST` and fails if any two differ or if a checkout bridge lacks it; jira is exempted by name with the reason in a comment
  - go test ./tests/lint passes on the tip that carries t1 and fails on a copy with one bridge's helper altered (negative case in the test itself)

### t9 — Enforce pi read-only at the tool level with --tools read

- depends on: t2
- covers: c4, h3, c12, h13
- acceptance:
  - `pi_cli`.`build_argv` takes sandbox; read-only appends '--tools read', workspace-write appends no tool flag; spawn forwards sandbox instead of discarding it; a handover dispatch still requires workspace-write (server.py:574 unchanged)
  - capabilities.py's confinement prose states read-only is tool-level (--tools read), not a kernel boundary, and keeps 'pi has no sandbox'; `test_surface.py` and `test_pi_cli.py` cover both modes and the prose
  - no bwrap, docker or micro-VM appears in the pi lane (grep in adapters/pi/src is empty)

### t10 — Add deploy/prod/cutover.sh: secrets -> deploy -> register for one host and engine, dry-run first

- depends on: t4, t5
- covers: c9, h21, c35, h27
- acceptance:
  - cutover.sh HOST ENGINE \[--dry-run\] \[--yes\] checks preconditions first — the engine account exists on the host and both compose files declare the engine/host token key in api and worker blocks — and refuses by name otherwise; then runs install-secrets' `install_bridge_account_env` for the engine, `deploy_account_engine_bridge`, and register-actor.sh with --os-user and harness/model/`model_endpoint` metadata; stops at the first failure, names the failed step on stderr, exits non-zero
  - --dry-run prints every step with would-run / would-skip and exits 0 with an empty shim log; a second real run against the same fake host logs every step as skipped and exits 0
  - it never invokes bootstrap-accounts.sh or sudo; it stays under 1000 lines and install-secrets.sh does not grow; tests/`test_deploy_cutover.py` drives all of the above under the fake-ssh shims; deploy/prod/README.md and .claude/skills/nodes-operator/SKILL.md document it

### t11 — Build the measurement runner: revision gate, dispatch per rule, checks, grades as an agent principal

- depends on: t6, t7
- covers: c30, h24, c29, h28
- acceptance:
  - examples/harness-compare/measurements/run.py (zero-dependency python3, curl-free urllib) reads a manifest, computes its digest, resolves each declared actor's row and bridge endpoint from GET /v1alpha1/actors, reads the deployment block from each bridge's /v1/capabilities, and exits non-zero naming any actor whose revision differs from --expect-revision
  - for each rule x actor x `runs_per_actor` it creates a harness-compare run with category = rule id, input.sandbox = the rule's sandbox, input.measurement = {`manifest_digest`, `rule_id`}, and per-actor bridge revisions in the input; it watches to terminal, applies the rule's check to the actor's summary, and posts a grade with the anchored rating and the check result in the notes, --as the agent actor id given by `MEASURE_RUNNER_ACTOR_ID` (never the human default)
  - a test against a fake API (stdlib http server) proves: stale revision refuses, category and measurement fields are set, each of the three check kinds yields the anchored rating, and every posted grade names the agent principal; ledger records from a fake-API run are authority proposed
  - README documents how to add or change a rule and re-run, and that re-runs append (never edit) runs and grades

### t12 — Register the measure-runner agent principal and the colleague actor when its account exists

- depends on: t8, t10, t5
- covers: h9
- acceptance:
  - company/measure-runner is registered kind=agent (register-actor.sh or the API) and GET /v1alpha1/actors lists it; if an agent row needs an endpoint, the row points at a bridge that only serves capabilities and this is recorded
  - after the spark bootstrap hand-turn (recorded on #298), cutover.sh spark colleague registers company/colleague-spark with harness=colleague and model=unsloth/Qwen3.8-27B-NVFP4, the bridge answers /v1/capabilities, and GET /v1alpha1/actors shows it; until then this half of the task is explicitly deferred in the delivery summary

### t13 — First manifest pass on the deployed fleet: pi and qwen on thor and orin

- depends on: t11, t9, t3, t8, t4
- covers: c25, h18
- acceptance:
  - the fixed bridges are deployed to thor and orin (deploy.sh, revisions checked via /v1/capabilities) and the CI conformance jobs are green on the merged revision
  - run.py basic.yaml completes 3 rules x 4 actors x 2 runs; every grade lands proposed; the operator confirms them through the review surface; GET /v1alpha1/actors/{id}/stats for the four actors shows grades in categories locate, review and explain
  - the pass's stats snapshot (per actor, per category: completion, `attempts_per_completion`, duration p50/p95, tokens, mean grade) is written to docs/audits/ with the manifest digest and bridge revisions

### t14 — Validate delivery and summarize: evidence for every success signal, hand-turn accounting, d2 closed

- depends on: t12, t13
- covers: c1, h12, c23, h16, c24, h17, c26, h19, c27, h20
- acceptance:
  - validate-delivery files evidence for each c27 signal from named tests, workflow runs or API reads; any signal that cannot be shown is filed as a behavioral delta, not asserted
  - the delivery summary maps every announcement clause to its requirement and signal, counts the hand-turns removed (#300 recreate, #298 sequence) against the one remaining (root bootstrap on spark), links the first-pass stats audit, and names what each shipped piece served
  - before-state reproductions are cited: the failing desync and timeout tests before t1/t2, deviation d2, the #300/#298 bodies, the pre-pass stats; d2 is marked closed and #302, #300, #298, #297 are closed or commented with what remains

## Risks

- [unknown_nonblocking] a grading-only agent principal (company/measure-runner) may need an `endpoint_url` to register as kind=agent; if so it points at a capabilities-only stub and the plan records it (task t12)
- [unknown_nonblocking] the colleague half of t12 waits on the spark root bootstrap hand-turn (q8): culture-colleague does not exist and sudo on spark needs a typed password; until then colleague is lane-supported (t5) but not registered or measured (task t12)
- [unknown_nonblocking] the control plane's handling of a bridge 413 (internal/actors/client.go) was not read during /challenge (c33): t1's e2e-style check should send one oversized dispatch through the fake-backed bridge and confirm it classifies as a contract failure rather than retrying (task t1)
- [unknown_nonblocking] why 'up -d' did not recreate the worker on thor is unproven (frame park v1); t4's unconditional recreate does not depend on it, but the live before/after check in t4 is what shows the fix works (task t4)
- [unknown_nonblocking] server.py line budget: qwen 947 and claude-code 941 leave ~50 lines under the hard limit for the helper and its call sites; if it does not fit, a byte-identical sibling module is the escape and t8's guard must follow it (task t1)
- [unknown_nonblocking] the first pass (t13) runs 24 sessions against one served model on thor:8000 with pi/qwen sessions on thor and orin; lobe concurrency is the binding limit — run one actor per host at a time; the pass is cheap in subscription terms but its wall-clock is unmeasured (task t13)
- [follow_up] orphaned pi processes after a second timeout are not reaped (frame park v5); a follow-up reaper, not this plan
- [follow_up] no adapter-colleague.yml CI conformance job this cycle (frame park v6)
