# Delivery Summary — upkeep-actors-jira

plan: `upkeep-actors-jira` · run: `partial` · date: `2026-08-14`
baseline: `devague summary skeleton`

## Intent

Nine upkeep and actor items, taken through the full devague chain and built by
fanning the plan out **through culture-nodes itself** rather than through local
subagents — so the product exercised itself on real work and every dispatched
run added to the comparative record of which actor is better at what. The
announcement the frame converged on: every economy-discord-graphs issue whose
delivery is evidenced is closed with that evidence cited, the pr-upkeep loop's
upkeep defects are fixed so a real PR flows sweep-to-merge without an operator
nursing it, dispatched actors get a clarify-then-commit gate, credential
rotation stops destroying keys it does not own, and every committed demo
becomes loadable by a deployment that is not this one. The Jira Cloud node-loop
the frame was named for was scoped in full and deliberately deferred to #76.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — Re-verify all nine batch items against main before any implementation starts
- `t2` — Record the issue-hygiene closures already executed this cycle
- `t3` — Add a credential and personal-identifier lint gate under tests/lint
- `t4` — Notifier renders the workflow name and omits legitimately empty fields (#66)
- `t5` — CI job compiles every workflow under examples/ (#73's recurrence half)
- `t6` — Typed literal binding restores observable visibility in the graph text (#73 option A)
- `t7` — sweep.py gains failed CI check runs as a third finding source (#61)
- `t8` — Tracker refuses to start when its bridge does not serve the actor it observes (#72)
- `t9` — Probe whether the spark bridge SERVICE can push, and record the verdict
- `t10` — Deploy the human-inbox tracker to the host serving company/human-ops (#72)
- `t11` — Credential rotation merges instead of replacing, with an explicit removal path (#69 item 1)
- `t12` — Post-deploy credential audit classifies declared vs present keys (#69 item 2)
- `t13` — Portable handle replaces the filesystem path between fix and review (#74)
- `t14` — Clarify-then-commit gate: engine-side protocol, per-actor and default-off (#67)
- `t15` — Capability surface on all four bridges (all-backends rule) (#67)
- `t16` — Committed demos become loadable by a deployment that is not this one
- `t17` — Three pr-upkeep items complete the full loop, counted from the ledger
- `t18` — Delivery summary: planned versus actual, with every scope item accounted for

## Actual Delivery

All eighteen plan tasks accounted for.

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | Re-verified nine items against `fadfa1d` before any build. Found two already delivered (#71 partly, #73's example half), which is why the task existed. Dispatched to `company/codex-thor` |
| `t2` | delivered | Ten issues closed with per-issue evidence citations (41, 43, 45, 46, 47, 49, 56, 64, 65, 68); 54, 48 and 66 deliberately left open with written reasons. PR #70 had listed them in prose GitHub does not parse |
| `t3` | delivered | Credential + personal-identifier lint gate. Merged `8bc3bd1` |
| `t4` | delivered | Notifier renders the workflow name, omits an empty actor. Merged `152684c` |
| `t5` | delivered | Offline example-compile CI gate — the part whose absence let #73 survive. Merged `95eabd5` |
| `t6` | delivered | Typed literal binding; the observable reads as a declaration in the graph again. Merged `8479786`. #73 closed on t5+t6 together |
| `t7` | delivered | Failed CI check runs as a third finding source. Merged `659f24d` |
| `t8` | delivered | Tracker refuses a foreign identity at startup. Merged `7d3a26d` |
| `t9` | delivered | A **negative** result, kept: the spark bridge *service* cannot push — HTTPS remote, no authorised key, no credential helper, no token in the service environment |
| `t10` | delivered | Pair deploys where its actor is registered. Merged `b8409de`. Also retired thor's orphan `:8087` pair, which had logged `pending=0` forever |
| `t11` | delivered | `prod.env` merges key by key through one `PROD_ENV_MERGE`; `remove-secret.sh` is the removal path merge-only requires. Merged `0c19d7f` |
| `t12` | delivered | `audit-credentials.sh` derives the declared set from compose's own `${KEY:?}` / `${KEY:-}` syntax and fails loudly on a missing required key. Merged `f4f2757`. Ran at the end of the live orin deploy and passed |
| `t13` | delivered | `handoff_unavailable` as a domain outcome. Merged `f999cee`. Proven live in run `01M00NVQH582QBARWSYFR0WTG1` |
| `t14` | delivered | Clarify-then-commit gate, engine side. Merged `9aeba35` — after independent verification, because the engine recorded the run `failed` while the session kept working and committed (#82) |
| `t15` | delivered | One byte-identical `preflight.py` across all four bridges, with a lint test that fails the build on divergence. Merged `0c9d62e`. One design call filed as #83 |
| `t16` | delivered | Example-portability guard; the sweep's script source became a granted environment value. Merged `032c01b` |
| `t17` | **partial** | The loop's blockers were diagnosed and two of three cleared; a real cycle ran and completed. **No item reached the merge gate.** See Drift and Remaining Work |
| `t18` | delivered | This artifact |

### Beyond the plan

Three items were built in the operator lane, outside the plan's eighteen tasks:

- **#63 closed out** — `codex-preflight.sh` gained a seventh check that probes
  user-namespace creation, and `deploy/prod/README.md` gained the
  "Unprivileged user namespaces" section the issue asked for. Commit `3332057`.
- **The claude actor token now reaches orin** — commit `a5438a3`, found by
  t12's audit on its first live run.
- **thor and orin deployed** from this branch, with the credential audit
  passing on both.

## Mid-work Decisions

- `d1` — spec claim c60 said the batch ships as several smaller PRs opened
  EARLY so per-PR SonarCloud and Qodo findings exist for the pr-upkeep loop to
  consume; the PR came at the END instead. Operator decision, recorded rather
  than silently followed: *"the PR is the seal on the work rather than a feed
  for it."* c60 was the mechanism behind t17's three-item target, so this
  directly shaped t17's number.
- **The Jira Cloud node-loop was deferred**, not dropped. It was scoped in full
  during `/think` and moved to #76 with all details, and nine of its claims
  were rejected in the frame so convergence measured what this cycle actually
  built. The user's call, to reduce the batch.
- **Deploy sequencing was chosen deliberately**: deploy *after* t12 merged, so
  the new post-deploy credential audit was exercised on a real deploy rather
  than only by static assertion. It immediately found a real defect.
- **Two same-wave tasks turned out to be semantically coupled.** t3's new
  credential lint failed after t7's fixtures merged, because t7 recorded an
  upstream payload verbatim carrying a vendor support address. Fixed forward by
  trimming the unused blob, not by weakening the lint. File-disjointness did not
  imply independence.
- **`preserve_on_failure` could not be relied on** for t14's recovery, because
  t9 established the bridge service cannot push. The work survived only because
  it was committed locally.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t17` (`d1`) | The PR now comes at the end rather than early, so the Qodo feed, the per-PR Sonar query and t7's new check-runs source produced nothing while the work they were meant to feed was being done | needs-follow-up |
| `t17` | Independently of `d1`: the loop **cannot** complete an item today. The sweep finds work and exits 0, but a code node's persisted output is runner metadata, and the artifact path has no ingest endpoint — so the item list never reaches the fix node. Not a supply problem; a capability gap (#79) | needs-follow-up |
| `t14` | Delivered completely but recorded `failed` — the engine's deadline expired while the session kept working and committed. Merged after independent verification; the mechanism is filed as #82 | risky |

No other plan task diverged from its contract.

## Evidence

- commits: `fadfa1d..0abf042` (38 commits, 13 task merges)
- PR: [#85](https://github.com/agentculture/culture-nodes/pull/85)
- go: `go build ./...`, `go vet ./...`, `gofmt -l` clean; `go test ./...` all packages pass
- pytest: 242 passed
- adapters: claude-code 238 · codex 222 · colleague 201 · notify 129 · human-inbox 166
- web: vitest 489 passed
- examples: 11/11 validate against the live compiler
- lint: black, isort, flake8, bandit clean; `markdownlint-cli2` clean; `uv run teken cli doctor . --strict` passes
- live run: `01M00NVQH582QBARWSYFR0WTG1` — sweep `passed`, triage `items`, fix `handoff_unavailable`, terminal `handoff-blocked`
- live deploy: thor and orin, credential audit passed on both
- issues filed: #76, #77, #78, #79, #80, #81, #82, #83, #84 · closed: #73 and ten from the prior batch

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| A credential rotation no longer destroys keys it does not own | high | commit `0c19d7f` · `tests/deploy/prodenvmerge_test.go` — verified failing against the pre-fix script before merge |
| A deploy now names any required credential that is missing | high | commit `f4f2757` · ran at the end of the live orin deploy and passed; found orin's missing claude token on its first run |
| The claude actor token reaches both workers | high | commit `a5438a3` · `tests/deploy/claudetokenplacement_test.go` · verified present on both hosts |
| All four bridges advertise one identical preflight surface | high | commit `0c9d62e` · md5-identical across four paths · guard verified to fail on injected drift, naming the diverging bridge |
| Committed examples are loadable by another deployment | high | commit `032c01b` · `tests/lint/exampleportability_test.go` · 11/11 examples validate |
| The observable authoring convention compiles and reads as a declaration | high | commits `95eabd5`, `8479786` · #73 closed |
| A host that cannot create a user namespace fails preflight | high | commit `3332057` · verified against a simulated blocked host |
| `handoff_unavailable` routes as a domain outcome, not an engine failure | high | run `01M00NVQH582QBARWSYFR0WTG1` — reached a clean terminal state through it |
| The pr-upkeep sweep works end to end again | high | same run: sweep `succeeded`, `exit_code 0`, acceptance recorded a `derived` confirm verdict |
| Codex sessions can exec shell commands on all three hosts | high | run `01M00AM5NME6TZ1PXDG4A454HE` · `bwrap` probed directly on each host |
| Three pr-upkeep items completed the full loop | **unverified — not achieved** | 0 items reached the merge gate; blocked by #79 |
| Codex `workspace-write` can land a patch (#18's write path) | unverified | the probe that settled shell exec was read-only; not claimed |

## Remaining Work / Follow-up

- **`t17` — the honest number.** `pr-upkeep` completions went **1 → 2**, but the
  second terminated at `handoff-blocked`; **items driven to a merged PR remain
  at 1**, against a target of three. Reported partial, not rounded up. The two
  metrics this cycle used are deliberately separate: **items through the
  pr-upkeep loop (1)** and **plan tasks executed through the engine this cycle
  (14 runs, 14 distinct tasks, 13 completed, against a baseline of 1)**.
- **#79 is the single remaining blocker on that loop.** No artifact ingest
  route is mounted, so the sweep's findings cannot reach the fix node even on
  one machine. Live evidence attached to the issue. Everything else in the loop
  now works.
- **#18's write path** stays unproven until a `workspace-write` dispatch lands
  a patch.
- **#84** — every deploy re-creates a duplicate human-inbox unit under a name
  spark does not run; it cannot bind and loops. Stopped and disabled by hand
  twice during this cycle.
- **#83** — the capability surface reads two sysctls rather than probing, so it
  can advertise a sandbox mode the host cannot deliver. It also contradicts the
  by-capability rule written into `deploy/prod` in the same cycle; one should win.
- **#76** — the Jira Cloud node-loop, scoped and ready for the next batch.
- **#82** — a node deadline does not stop the actor session, which is a
  concurrent-writer hazard, not only a bookkeeping error.
- **#77** — nothing records which model or effort did the work, so every actor
  grade in this cycle rates an endpoint rather than a known model.

## Actor quality recorded this cycle

Six graded runs against `company/developer` (5, 5, 5, 5, 5, 4) and two against
`company/codex-thor` (5, 3), all as `grade` ledger records. The pattern held:
route codex at finding and citing facts, route the claude-code bridge at
deciding and building. Two codex "already delivered" verdicts were wrong and
would have cancelled real work had they been taken at face value.
