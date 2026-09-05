# First measurement pass: pi vs qwen on thor (2026-09-05)

Plan `harness-hardening-and-compare` task t13. Manifest
`examples/harness-compare/measurements/basic-thor.json`
(digest `sha256:3846ab95bb817734c1273f982f1376051d04d911cfebc5afe768d34a611ab027`),
three read-only rules, two runs per actor, dispatched strictly one at a time
by `examples/harness-compare/measurements/run.py` through
`https://nodes.culture.dev`, graded as the agent principal
`company/measure-runner` (`actor_register_1788590756878382256_2181324`).
The grades were meant to land **proposed** under that agent principal
(deviation d3). They did not: the API resolves the grading actor from the
request's bound principal, so all 12 landed **confirmed** under `company/ori`
(the operator's Access identity). Deviation d4 and issue #306 record this;
the numbers below are therefore human-confirmed opinions produced by a
machine, and the reader should weigh them as the runner's check results,
not as the operator's judgement.

Bridge revisions at dispatch (read from each bridge's `/v1/capabilities`
deployment block, gate `--expect-revision`):
`94415be1d82450d58f0da027c16193d82e96f882`, `install_mode=copy`, both
actors. Model held constant: `unsloth/Qwen3.8-27B-NVFP4` at `http://thor:8000/v1`.

## Result per actor and rule

| actor | rule | runs | ratings | verdicts | mean seconds |
|---|---|---|---|---|---|
| company/pi-thor | explain-output-tests | 2 | [3, 3] | names 'tests/test_cli.py' but pads the a; names 'tests/test_cli.py' but pads the a | - |
| company/pi-thor | locate-exit-code-policy | 2 | [3, 3] | names 'culture_nodes/cli/_errors.py' but; names 'culture_nodes/cli/_errors.py' but | - |
| company/pi-thor | review-seeded-defect | 2 | [5, 5] | names '500' and points at Line 179; names '500' and points at line 179 | - |
| company/qwen-thor | explain-output-tests | 2 | [3, 3] | names 'tests/test_cli.py' but pads the a; names 'tests/test_cli.py' but pads the a | - |
| company/qwen-thor | locate-exit-code-policy | 2 | [3, 3] | names 'culture_nodes/cli/_errors.py' but; names 'culture_nodes/cli/_errors.py' but | - |
| company/qwen-thor | review-seeded-defect | 2 | [5, 5] | names '500' and points at line 179; names '500' and points at line 169 | - |

Both harnesses produced the same profile (by the runner's checks): they name the seeded defect with a
citation (5), and they name the right file for the locate and explain rules
but pad or fail to cite (3). Mean rating 3.67 for each actor over six runs.
This is one data point per cell, not a verdict; the manifest exists so the
next pass can widen it.

## Per-actor stats from `GET /v1alpha1/actors/{id}/stats` after the pass

| actor | runs by outcome | attempts/completion | duration p50 / p95 | grades proposed (mean) | grades confirmed (mean) | categories |
|---|---|---|---|---|---|---|
| company/pi-thor | cancelled=1, completed/completed=8, failed=1 | 1.25 | - / - | 0 (-) | 7 (3.7142857142857144) | explain-output-tests, harness-compare, locate-exit-code-policy, review-seeded-defect |
| company/qwen-thor | completed/completed=7 | 1 | - / - | 0 (-) | 7 (3.7142857142857144) | explain-output-tests, harness-compare, locate-exit-code-policy, review-seeded-defect |
| company/pi-orin | - | - | - / - | 0 (-) | 0 (-) |  |
| company/qwen-orin | - | - | - / - | 0 (-) | 0 (-) |  |

The orin actors show zero runs: the comparison graph's slots pin thor's
registry ids (deviation d2, issue #304). `company/pi-thor` also carries one
`cancelled` run and one ungraded `completed` run from two aborted launches
of this pass (a harness memory kill and the edge-cache freeze, issue #305);
both are visible in the counts above and belong to no rule.

## Evidence

- Report: `docs/audits/2026-09-05-first-measurement-pass-thor.jsonl` (one
  line per run: run id, actor, rule, check verdict, rating, grade id,
  bridge revision, timing).
- Runs and grades are in the ledger under category = rule id
  (`locate-exit-code-policy`, `review-seeded-defect`, `explain-output-tests`).
