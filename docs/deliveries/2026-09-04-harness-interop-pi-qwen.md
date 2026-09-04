# Delivery Summary — harness-interop-pi-qwen

plan: `harness-interop-pi-qwen` · run: `complete` · date: `2026-09-04`
baseline: `devague summary skeleton`

## Intent

Issue #294: make Culture Nodes harness-neutral again so the same bounded task
can run through different agent harnesses on one served model and be compared.
The run adds Pi as a first-class actor adapter, restores the parked Qwen Code
adapter, stands both up as registered actors on thor and orin under their own
OS accounts, and proves the two harnesses complete real dispatches through
nodes on the same local model. Executed from the converged frame + plan
`harness-interop-pi-qwen` (11 tasks, 5 waves): eight code tasks (t1–t8) fanned
to codex actors and local subagents, the operator cutover (t9), the live proof
(t10), and this closure (t11).

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — adapters/qwen: answer #228 and admit yolo under the engine account
- `t2` — adapters/pi: new pi-bridge over 'pi --mode json -p' with fake pi, transcripts and process-group cancel
- `t3` — deploy/prod/lanes/unix-user.sh + bootstrap-accounts.sh: pi engine, node tarball install, pins, host map
- `t4` — deploy/prod/pi-preflight.sh: non-billable deploy-time and ExecStartPre checks for a pi actor
- `t5` — examples/harness-compare: fan one request to a run-time actor list and join the results
- `t6` — registration + operator surfaces: compose token names, audit classification, nodes-op.sh actor rows, skill docs
- `t7` — deploy.sh thor|orin qwen + pi bridge lanes, units, templates, ports, install-secrets account steps
- `t8` — guards + CI: enrol the sixth adapter, widen lint-all.sh to five jobs, adapter-pi.yml and adapter-qwen.yml with the conformance kit
- `t9` — operator hand-turns: bootstrap culture-qwen + culture-pi on thor and orin, secrets, deploy, register four actors, verify host-level honesty conditions
- `t10` — proof dispatches: one pi run and one qwen run on thor/orin, read run + ledger, grade, record actor-quality notes
- `t11` — close the loop: validate-delivery, delivery record, memory + docs updates, version bump, PR

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | adapters/qwen answers #228 (permission_blocked / no_changes outcomes, never completed) and admits yolo; README un-parked. codex-orin build (run `01M1PJQMM5J61HWJ1DWP0PZEVX`), merged `d17851f`; 357 tests |
| `t2` | delivered | adapters/pi bridge over `pi --mode json`, fake pi, transcripts, process-group cancel; four shared modules byte-identical to qwen. codex-thor build (run `01M1PJNMJNHVW3W47JRRK3BGRW`), merged `f3663e6`; 13 tests |
| `t3` | delivered | unix-user.sh pi engine (node-22 tarball + PATH wrapper), models.json copy, thor\|orin → codex qwen pi; merged `a7582c8` (deviation d1) |
| `t4` | delivered | deploy/prod/pi-preflight.sh, six non-billable checks, 22 Go tests; merged `454b77d` |
| `t5` | delivered | examples/harness-compare fans to named actor slots and joins per-actor outcomes; e2e + lint tests; merged `b900abc` |
| `t6` | delivered | compose token names + audit classification (codex-orin run `01M1PKJ0ZDD30NPNHW5AC2GN0Y`) plus operator gap-fill (nodes-op actor rows + SKILL table); merged `885a7c2` |
| `t7` | delivered | deploy.sh qwen + pi bridge lanes, pi unit/template, ports 8092/8093, install-secrets account steps, 67 fake-host tests; merged `52aa3ba` |
| `t8` | delivered | pi enrolled in the three lint guards, lint-all.sh widened to five jobs, adapter-pi/qwen workflows; conformance is the reference check (deviation d2); merged in `f3663e6` + lint-toolchain follow-ups |
| `t9` | delivered | culture-qwen + culture-pi bootstrapped on thor (operator, NOPASSWD) and orin (user sudo); four bridge tokens minted + prod.env wired; both hosts deployed at 0.49.0; four actors registered with harness/model metadata; all four bridges healthz 200 |
| `t10` | delivered | qwen-thor run `01M1PSWJ89T4FV616WJCTXHH6T` and pi-thor run `01M1PW8D3N6ZK2V4DNXT29CV6X` both completed on unsloth/Qwen3.8-27B-NVFP4, each graded 4/5; actor-quality note remembered |
| `t11` | delivered | this record, /validate-delivery evidence (o1–o6, e1–e6), version 0.49.0 (`07fdac8`), memory updates; PR via cicd follows |

## Mid-work Decisions

- `d1` — t3 landed its pi coverage in a new sibling module `tests/test_deploy_unix_user_pi.py` (a fourth file beyond the three the acceptance criteria name) because folding it into `tests/test_deploy_unix_user.py` would exceed the 1000-line file guard; the sibling-module shape already exists for two adjacent test files. Approved (acceptable).
- `d2` — the adapter-pi/qwen conformance jobs run the in-process reference check, not the live fake-backed run with a shared `-input` that c10/h9 describe, because a live run needs a dynamic fake plus a `run_conformance_kit.sh` per adapter, both under adapters/ and outside t8's file set. Approved (needs-follow-up); tracked as #297.
- The wave-0 codex actors built t1, t2, and t6; t6 came back missing the nodes-op actor table and the SKILL doc table (codex covered the config half only), so the operator filled those two files by hand before merge. Captured here; no separate deviation record.
- Five integration bugs surfaced only by the live cutover (none by a test), fixed in-run and committed to the branch: pi-preflight's doubled `/v1/v1/models` URL, the pi config rejecting `model_endpoint`, the missing dataclass field, the provider-name mismatch, and #299 (empty summary → contract_rejected). Plus #300, the stale-worker token gap, fixed by a manual `--force-recreate`.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t3` (`d1`) | the 1000-line file guard was not in the task's acceptance text; the only literal-plan alternative was a red CI gate | acceptable |
| `t8` (`d2`) | a live fake-backed conformance run needs a dynamic fake + a `run_conformance_kit.sh` per adapter, both under adapters/ and outside t8's file set; the reference check proves the kit compiles and the protocol reference holds | needs-follow-up |
| `t6` | codex delivered the compose/audit/register half; the nodes-op actor table and SKILL doc table were added by the operator before merge — no record covers this, captured as a mid-work decision | acceptable |
| `t9`/`t10` | the plan assumed the deploy wired new actor tokens; it did not, so a manual token wiring + worker `--force-recreate` was needed (#300), and the first pi run hit #299 before the in-run fix | needs-follow-up |

## Evidence

- tests: `uv run pytest -n auto` — 1039 passed, 6 subtests (full suite on the branch)
- tests: `go test ./...` — all packages pass (guards, deploy, conformance reference, e2e)
- tests: `adapters/pi uv run pytest -q` — 13 passed (incl. the #299 regression on parse_session)
- tests: `adapters/qwen uv run pytest -q` — 357 passed (incl. the four #228/yolo tests)
- lint: `scripts/lint-all.sh` — all five lint jobs pass locally
- live: qwen-thor run `01M1PSWJ89T4FV616WJCTXHH6T` — completed, graded 4/5 (`ledger_01M1PVFG9DJEHF9WQHRCTGB0TF`)
- live: pi-thor run `01M1PW8D3N6ZK2V4DNXT29CV6X` — completed, graded 4/5 (`ledger_01M1PWED732283775FVMCGF9S4`)
- commits: `df572dc..d4185f4` (27 commits)
- PRs / issues: #294 (this cycle), #296 (0.48.2 collector fix, merged), #295/#297/#298/#299/#300 (findings), deviations d1/d2, evidence o1–o6/e1–e6

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| Pi and Qwen Code are dispatchable actors on thor and orin; a run records its harness + model | high | live runs `01M1PSWJ…` / `01M1PW8D…`; actor rows carry harness/model/model_endpoint (evidence e1) |
| The four actors are registered, healthy, and reachable by nodes-op assign | high | four bridges healthz 200 on both hosts; four registered rows; four dispatches accepted (e2) |
| One real pi run and one real qwen run each completed with a ledgered run and a grade | high | runs `01M1PW8D…` / `01M1PSWJ…`, both graded 4/5 (e3) |
| The pi bridge drives `pi --mode json`; shared core byte-identical to qwen; offline tests green | high | adapters/pi 13 passed; tests/lint byte-identity + reaper guards green (e4) |
| examples/harness-compare fans one instruction to named actor slots and joins outcomes | high | `go test ./tests/e2e -run TestHarnessCompare` + examplescompile/portability green (e5) |
| #228 is answered: permission_blocked / no_changes, never completed; yolo admitted | high | adapters/qwen 357 passed incl. the four new tests (e6) |
| The adapter conformance jobs run the live fake-backed swap with a shared input | unverified | deferred to #297 (deviation d2) — reference check only today |

## Remaining Work / Follow-up

- **#299** — fixed in-run (commit `d4185f4`) and proven live; closes on merge of this branch.
- **#297** — the live fake-backed conformance run (dynamic fake + `run_conformance_kit.sh` per adapter); deferred from t8 (d2).
- **#300** — automate the api/worker `--force-recreate` (or token-change detection) in deploy.sh so a new actor authenticates without a manual step; fixed live this cycle, code fix pending.
- **#298** — a first-class cutover command (bootstrap → secrets → deploy → register) so the next harness is a reviewed one-liner rather than four hand-run scripts.
- **pi async result envelope** — the two remaining live-cutover fixes (pi config `model_endpoint`, pi-preflight `/models`) are committed; watch for further envelope mismatches when a write-path (workspace-write) pi dispatch is first attempted.
- **orin harness parity** — pi-orin and qwen-orin are registered and healthy but were not live-dispatched this cycle (thor covered the t10 proof); a first orin dispatch is the obvious next data point.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

<https://claude.ai/code/session_017zi8Kzf8KyDJmivPtLEwQS>
