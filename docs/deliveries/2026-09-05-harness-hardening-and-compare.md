# Delivery Summary — harness-hardening-and-compare

plan: `harness-hardening-and-compare` · run: `partial` · date: `2026-09-05`
baseline: `devague summary skeleton`

## Intent

Close the follow-ups of the #294 harness-interop cycle as one reviewed
change: harden the pi and qwen bridges (and every checkout bridge) against
an oversized body and a hung sync (#302), make deploy.sh recreate api and
worker on a token change (#300), give the next harness a one-command cutover
(#298), prove the harness swap live in CI (#297), and turn the pi-vs-qwen
comparison into a re-runnable, manifest-driven measurement with colleague
lane support. Executed by `/assign-to-workforce` on branch
`spec/harness-hardening-and-compare` under the user's standing instruction
to run all waves autonomously, deviate when needed, and open issues for
decisions.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — Port the bounded oversized-body refusal to every checkout bridge and bound it in notify/human-inbox
- `t2` — Guard pi's second sync timeout the family way and test it with a SIGTERM-ignoring fake
- `t3` — Add pi and qwen `run_conformance_kit.sh` harnesses and switch both adapter workflows to the live fake-backed run
- `t4` — Recreate api and worker in both deploy lanes and fix the README rotation runbook
- `t5` — Teach the account lanes the colleague engine and wire its unit, template and compose token keys
- `t6` — Add the colleague slot and the measurement input object to harness-compare and move the e2e slot pin
- `t7` — Define the measurement manifest: schema, digest, validator and the basic three-rule set
- `t8` — Lint guard: the oversized-body helper is byte-identical across all seven bridges
- `t9` — Enforce pi read-only at the tool level with --tools read
- `t10` — Add deploy/prod/cutover.sh: secrets -> deploy -> register for one host and engine, dry-run first
- `t11` — Build the measurement runner: revision gate, dispatch per rule, checks, grades as an agent principal
- `t12` — Register the measure-runner agent principal and the colleague actor when its account exists
- `t13` — First manifest pass on the deployed fleet: pi and qwen on thor and orin
- `t14` — Validate delivery and summarize: evidence for every success signal, hand-turn accounting, d2 closed

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | `_refuse_oversized_body` (drain bounded at `MAX_BODY_BYTES`, 413 + `Connection: close`, called before auth) in all seven bridges, byte-identical; 14 socket tests; commit `4347a37`, merged `f56518d` |
| `t2` | delivered | `pi_cli.run_sync` catches the second timeout, never SIGKILLs, maps to class `timeout`; `FAKE_PI_IGNORE_SIGTERM`; commit `d85986d`, merged `d270cd5` |
| `t3` | delivered | `adapters/pi/scripts/run_conformance_kit.sh`, `adapters/qwen/scripts/run_conformance_kit.sh`, both workflows run them; shared input carries `mode=default` for both; commit `812ce17`, merged `fc32bd1` |
| `t4` | delivered | thor lane `up -d --force-recreate api worker`, orin `--force-recreate worker` (no api service on orin); README runbook fixed; live: thor's api/worker `Created` moved on deploy; commit `5d9b7f8`, merged `8514414` |
| `t5` | delivered | engine `colleague` in `unix-user.sh` / `bootstrap-accounts.sh` (spark) / `account-bridges.sh` (opt-in spark lane), unit + template (port 8094), compose token keys, audit classification `optional`; 16 shim tests; commit `71bd54d`, merged `88f5b01` |
| `t6` | delivered | fifth slot `colleague` on `actor://company/colleague-spark`, optional input object `measurement`, e2e slot pin moved (DB-backed test ran); commit `d8b7437`, merged `3ca9b67` |
| `t7` | delivered | `measurements/schema.json`, `manifest.py` (validate / digest / canonical), `basic.json`; 36 tests; YAML twin moved to `tests/fixtures/` after the example guards caught it (`fbe7c28`); commit `67f7692`, merged `3513b45` |
| `t8` | delivered | `tests/lint/oversizedbody_test.go` with a negative drift case; commit `d7e7a85`, merged `8806ee6` |
| `t9` | delivered | read-only appends `--tools read`; capabilities prose says tool-level, not kernel; bwrap/docker grep guard; commit `45c372b`, merged `4878571` |
| `t10` | delivered | `deploy/prod/cutover.sh` (421 lines): preconditions, secrets via the fenced install-secrets lane (file stays 999 lines), deploy, register, `--dry-run`, `--yes`; 21 shim tests; commit `5ad9d36`, merged `d0e07fc` |
| `t11` | delivered | `measurements/run.py` + `fleet.py` + `checks.py`: revision gate, serial dispatch, three check kinds, grades posted `--as` the agent principal; 44 tests; plus three fix-forwards (`7fd75af` per-slot tokens, `01b9da7` User-Agent, `f8e3947` cache-bust, `fed7f70` abort on principal override) |
| `t12` | partial | `company/measure-runner` registered (`actor_register_1788590756878382256_2181324`, kind agent, loopback endpoint, never dispatched). The colleague actor is **not** registered: the spark account does not exist (root hand-turn, q8) |
| `t13` | partial | 12 runs on **thor only** from `basic-thor.json` (2 actors × 3 rules × 2), serial, at bridge revision `94415be`; audit `docs/audits/2026-09-05-first-measurement-pass-thor.md` + `.jsonl`. Orin not reached (d2); grades landed **confirmed under the operator**, not proposed under the agent (d4) |
| `t14` | delivered | `/validate-delivery` filed o1–o16, e1–e16 (14 pass, 2 fail), b1–b7; this summary; d2 of the previous plan is superseded by t3's live run |

## Mid-work Decisions

- `d1` (proposed) — codex-thor and codex-orin refused the wave-0 dispatches with a spent refresh token; t1 and t4 (and later t10, t11) ran on local subagents — codex auth needs an interactive re-login (#303).
- `d2` (proposed) — the first pass measured thor only: `harness-compare`'s slots pin thor's registry ids, so orin actors are unreachable through the graph; the per-host slot design is the owner's decision (#304).
- `d3` (proposed) — the assistant does not confirm grades under the operator's identity; confirming is the human's act.
- `d4` (proposed, risky) — the API binds the grading actor to the request principal, so the 12 pass grades landed confirmed under `company/ori`; the runner now aborts on that override (#306).
- q7/q8/q9 (frame decisions applied on "confirmed all"): read-only means `--tools read`; colleague waits for its own account; `input.measurement` + category = rule id.
- Not covered by a record: `basic-thor.json` added as the pass's actor set; the runner grew per-slot token env vars, a named User-Agent (Cloudflare 1010) and cache-busting GETs (#305); the YAML manifest twin moved out of `examples/`; the measure-runner principal registered with a loopback endpoint (risk r1 resolved that way); the orin lane recreates only the worker (compose.orin.yml has no api).

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t1`, `t4`, `t10`, `t11` (`d1`) | codex-bridge auth expiry on thor and orin; built by local subagents instead of the codex lanes | needs-follow-up |
| `t12` | colleague actor not registered: no `culture-colleague` account on spark (root hand-turn); lane support shipped | needs-follow-up |
| `t13` (`d2`) | orin actors unreachable through the comparison graph; thor-only pass | needs-follow-up |
| `t13` (`d4`) | grades landed confirmed under the operator, not proposed under the agent; 12 human-confirmed opinions produced by a machine | risky |
| `t13` (`d3`) | grades were not confirmed by the assistant — moot after d4, since the API confirmed them itself | acceptable |
| `t7` | YAML twin moved to `tests/fixtures/` (example guards); JSON remains canonical | acceptable |
| `t11` | four fix-forward commits after merge (tokens, User-Agent, cache-bust, abort-on-override), each with a test | acceptable |

Wave accounting against the split plan (issue #48): wave 0 planned 2 codex
plus 5 local sessions, actual 7 local and 2 failed codex attempts; wave 1 planned
2 codex + 2 local, actual 4 local; waves 2–3 operator lane as planned, plus
24 bridge sessions on the local model (12 measured runs, 1 cancelled orphan,
1 ungraded completed run from the aborted launches).

Hand-turns counted (CLAUDE.md): the codex re-login on two hosts (#303, still
owed); the `culture-colleague` bootstrap on spark (#298 comment owed when
done); resetting the codex/pi/qwen account checkouts to main over ssh before
dispatch (done in-session); two deploys from the branch (thor exit 3 = sweep
paused pending orin, orin exit 0, sweep resumed).

## Evidence

- tests: all seven `adapters/*/tests/test_server_oversized_body.py` — pass (14)
- tests: `adapters/pi/tests/test_pi_cli.py` + `test_surface.py` — pass (22)
- tests: `tests/test_conformance_kit_inputs_match.py` — pass; `adapters/{pi,qwen}/scripts/run_conformance_kit.sh` — exit 0, 4 PASS each
- tests: `tests/test_deploy_two_host.py` (22), `tests/test_deploy_cutover.py` (21), `tests/test_deploy_unix_user_colleague.py` (16) — pass
- tests: `tests/test_measurement_manifest.py` (36), `tests/test_measurement_runner.py` (44) — pass
- tests: `go test ./tests/lint` (incl. `OversizedBody`), `go test ./tests/e2e -run TestHarnessCompare` — ok; full `go test ./...` — ok
- suite: `uv run pytest -n auto` — 1160 passed; `scripts/lint-all.sh` — all five jobs green
- live: thor `prod-api-1`/`prod-worker-1` Created `2026-09-04T18:30` → `2026-09-05T06:48` on deploy; `/v1alpha1/version` revision `94415be…` on thor; both thor bridges `/v1/capabilities` at `94415be`, `install_mode=copy`
- live: ledger of run `01M1R60MMZ2RVP7YWXWMED5Z1Z` — grade `confirmed`, origin `company/ori` (the d4 fact)
- devague: obligations o1–o16, evidence e1–e16, deltas b1–b7 (proposed); deviations d1–d4 (proposed)
- commits: `7528101..fed7f70` (34 commits, 70 files); PRs / issues: #297 #298 #300 #302 (this cycle's scope), #303 #304 #305 #306 (opened this cycle)

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| an oversized body gets 413 + close on every bridge, drain bounded, before auth | high | e1, e2 · `tests/lint/oversizedbody_test.go` · commit `4347a37` |
| a hung pi sync reports the timeout outcome and never SIGKILLs | high | e5 · `adapters/pi/tests/test_pi_cli.py` |
| `sandbox=read-only` launches pi with `--tools read` | high | e6 · commit `45c372b` |
| CI runs the §13 kit live against fake-backed pi and qwen with one shared input | high | e7 · `.github/workflows/adapter-pi.yml`, `adapter-qwen.yml` (CI itself runs on the PR) |
| deploy.sh recreates api and worker in both lanes and the README agrees | high | e8, e9 · live Created timestamps on thor |
| `cutover.sh` takes a host+engine from secrets to registration with dry-run and named refusals | high | e10 · `deploy/prod/cutover.sh` (a real cutover has not been run yet — colleague waits) |
| colleague has lane support; its actor and slot exist in code | high | e11 (lane half), e15 · commit `71bd54d`, `d8b7437` |
| a colleague actor is registered and measured | unverified | e11 fails on the registry half — not claimed done |
| a declared manifest drives graded runs readable per category | high | e12, e13, e16 · `docs/audits/2026-09-05-first-measurement-pass-thor.md` |
| pass grades land proposed under the agent principal | unverified | e14 **fails**: confirmed under `company/ori` (#306) — not claimed done |
| pi and qwen were measured on both hosts | unverified | thor only (d2, #304) — not claimed done |
| the runner refuses stale bridges and records revisions | high | e13 · live `--gate-only` |
| hand-turns removed: the manual recreate (#300) and the four-script sequence (#298) | medium | code + tests; the cutover script has not yet run for real |

## Remaining Work / Follow-up

- **#306 (owner decision)** — the 12 confirmed grades under `company/ori`: leave or supersede; and how a runner authenticates as an agent principal (dial-in bearer vs explicit delegation). Until then no manifest pass should be run through the operator's cookie; the runner aborts on the first override.
- **#304 (owner decision)** — reach the orin actors: one slot per registered actor, or a per-host graph copy; then re-run `basic.json` (four actors).
- **#303 (hand-turn)** — interactive `codex login` as `culture-codex` on thor and orin; prove with one read-only assign each.
- **#298 (hand-turn)** — `deploy/prod/bootstrap-accounts.sh spark` after this PR lands, then `deploy/prod/cutover.sh spark colleague --dry-run` / `--yes`; that is the first real cutover run and the colleague half of t12.
- **#305** — origin sends `Cache-Control: no-store` on `/v1alpha1/`; then remove the runner's cache-bust.
- Deviations d1–d4, obligations, evidence and deltas are `proposed`: the owner confirms or rejects them (`devague deviate --confirm dN`, `devague evidence --confirm eN`, `devague delta --confirm bN`).
- Frame parks v5 (orphaned pi processes) and v6 (adapter-colleague CI job) are unchanged follow-ups.
- Deviation `d2` of the previous plan (`harness-interop-pi-qwen`, "needs-follow-up") is met by t3; mark it closed when confirming this cycle.
