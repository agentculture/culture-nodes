# Delivery Summary — close-the-backlog

plan: `close-the-backlog` · run: `complete` · date: `2026-08-15`
baseline: `devague summary skeleton`

## Intent

Clear Culture Nodes' own tracker — every open issue reaching a recorded
disposition rather than a guess — and do it *through Culture Nodes*, so the
product exercised itself on real work and the cycle produced a comparative
record of which actor is better at what. The plan carried 33 tasks in five
waves against 150 coverage targets, dispatched across ten rounds to two codex
hosts and three Claude bridges, capped at one to two model sessions per
account.

## Planned Work

Quoted verbatim from the `devague summary` skeleton:

- `t1` — Add a Go job to CI so the Go tree is tested somewhere that is not an operator's laptop (#115 step 1)
- `t2` — Commit a regenerable triage table: every open issue, its bucket, its disposition, its evidence pointer
- `t3` — Template the closing comment so every closure is re-checkable by a stranger
- `t4` — Account for the cycle honestly: issues opened versus closed, and every self-opened issue dispositioned
- `t5` — Write the stage-1 lane split down before any dispatch: which issue's evidence comes from spark, which from an agent host
- `t6` — Template the verification dispatch: one brief shape, read-only posture, evidence-or-cannot-verify contract
- `t7` — Run the bucket-A sweep through Culture Nodes and decide every claim it produces
- `t8` — Make each stage's exit gate a query, not an assertion
- `t9` — Git becomes the handover medium: the agent pushes to refs/culture-nodes/RUN-ID, fenced server-side (#90)
- `t10` — Record a pushed ref as OBSERVED evidence the control plane measured, not as the agent's claim (#13)
- `t11` — The merge gate consumes CI's verdict as derived evidence instead of an operator reading a green tick (#101)
- `t12` — Fix the bug tail: #95 and #105 (one fix, two tests), #98, #21, #17
- `t13` — Deploy an OTLP collector and close #5 on a live trace
- `t14` — Ship the database's backups off its own host, and prove the restore (#30)
- `t15` — Make the database a deployment input in every profile, not just in the Helm chart (#112)
- `t16` — Close out the AWS lane honestly: opt-in RDS, the measurements that decided it, and what binds only if #112 chooses rds
- `t17` — Split sweep into a pure emitter with durable watermarks, and let conditioned event handlers react
- `t18` — Disposition #50 as cross-repo work, not an external wall
- `t19` — Make the capability surface report toolchains, using the three probes as its test cases (#96)
- `t20` — Hold the two standing repo boundaries through the cycle: vendored skills and the zero-dependency runtime
- `t21` — Land the simple credential form with its expiry wired in, and file what replaces it (#111)
- `t22` — Give the dial-in path rate limiting, lockout and revocation before it accepts a connection
- `t23` — Invert the transport: bridges dial in, with mixed mode decided before the first bridge changes
- `t24` — Retire `endpoint_ref` under the approved ADR 0002 bypass
- `t25` — Audit the codebase against the connection principle and record what still violates it
- `t26` — Record that the bootstrap was built the old way, rather than claiming the loop ran itself
- `t27` — Put the remaining owner decisions in front of the owner as costed briefs
- `t28` — Close the loop on why this mattered, and on the signal that judges the next cycle
- `t29` — Make a run explain itself: untruncated ledger claims and a real run-status surface (#92, #108)
- `t30` — Build the affirmative half of the authority model: a surface that decides a proposed claim (#99)
- `t31` — Compose the work-package brief from the plan and the capability surface instead of a heredoc (#103, #93)
- `t32` — Close the gate-repair-gate loop and make the deployed revision a recorded fact (#102, #104)
- `t33` — Make upkeep run itself: a schedule that starts runs, and findings routed by actor affinity (#107)

## Actual Delivery

All 33 tasks accounted for. 32 delivered, 1 partial.

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | Go job in `tests.yml` with a Postgres service and a guard that fails when DB tests skip; proved by injecting a real defect, not by deleting an assertion (`58b1827`, `3fa2caf`) |
| `t2` | delivered | `scripts/triage-report.py` + `docs/triage/dispositions.csv`; CI fails when the table drifts from the tracker (`acff4b8`) |
| `t3` | delivered | `scripts/close-issue.sh`, signature resolved from `culture.yaml` (`acff4b8`) |
| `t4` | delivered | `scripts/cycle-accounting.py` — four numbers, four re-runnable commands, boundary derived from git (`1b19e48`) |
| `t5` | delivered | Stage-1 lane split committed before any dispatch (`7dee034`) |
| `t6` | delivered | `scripts/render-verification-brief.py`; one shape, read-only, evidence-or-cannot-verify (`7dee034`) |
| `t7` | delivered | Two lane-shaped sweep packages (d10); two closures, three narrowed, one new defect (`a764560`) |
| `t8` | delivered | `scripts/ledger-gate.py` — a query over the ledger, not an assertion (`cd47642`) |
| `t9` | delivered | Handover ref created by all three bridges; ssh transport replaced the ruleset design (d1, d13) (`dfce0f2`, `82c1e3e`) |
| `t10` | delivered | `internal/handover` fetches from the control plane's own remote and records sha + changed paths as `observed`; no fetchable ref → no record (`62b9b52`) |
| `t11` | delivered | `scripts/collect-handover.py` (run id → diff) + `POST /v1alpha1/runs/{id}/suite-verdicts` writing `derived` verdicts (`13b8543`) |
| `t12` | delivered | #95/#105, #98, #21, #17 all fixed (`4d41bd5`, `90914fc`, `975ed8a`) |
| `t13` | delivered | OTLP collector deployed, #5 closed on a live trace (`5469c20`) |
| `t14` | delivered | Off-host S3 backups, opt-in, remote retention by bucket lifecycle not by the backup loop (`a7c27c5`) |
| `t15` | delivered | Database a deployment input in every profile, proved against an external Postgres (`2ac7e35`) |
| `t16` | delivered | AWS lane closed on the record; RDS reverted to opt-in (`673ea87`) |
| `t17` | delivered | Pure emitter with durable watermarks, then workflow-level CEL `triggers` that CREATE runs (`a7a1b16`, `32c2816`) |
| `t18` | delivered | #50 filed upstream as cross-repo work (`acff4b8`) |
| `t19` | delivered | Capability surface reports what can EXECUTE, not what is on disk (`8b442e3`) |
| `t20` | delivered | Both boundaries CI-enforced: `check-vendored-skill-diff.py`, `check-zero-runtime-deps.sh` (`673ea87`) |
| `t21` | delivered | Migration 0031 + constant-time verifier; CHECK rejects IP-shaped keys; dump guard passes against a real dump (`1627bfd`) |
| `t22` | delivered | Separable rate limiting and lockout, positive `revoked_at` revocation, `SELECT FOR UPDATE` (`15ae877`) |
| `t23` | delivered | Mixed mode decided and costed, executable rollback, dial-in in all five bridges; criterion 2 demonstrated on a scratch deployment (`e2d11ca`, `a91cf5e`) |
| `t24` | **partial** | Migration written with the bypass recorded in the file; **held unapplied** in `migrations/pending/` because its own precondition (fleet cutover) is unmet (`9425bc2`) |
| `t25` | delivered | Connection-principle audit; records that the principle is not yet satisfied (`7dee034`) |
| `t26` | delivered | Fourteen operator steps classified; the count is **14 of 14 still manual** (`1b19e48`) |
| `t27` | delivered | Four costed owner-decision briefs, each with a "no" branch that closes something (`153f7b6`) |
| `t28` | delivered | Last cycle's three NOT MET signals each mapped to a named issue, with one mapping refused and the refusal explained (`1b19e48`) |
| `t29` | delivered | Untruncated claims + `running` verb (`6f97441`) |
| `t30` | delivered | Decision surface; `checkReviewerIsHuman` closes the hole the refusal half was hiding (`1af7993`) |
| `t31` | delivered | `scripts/render-work-package.py` generates briefs from plan + capability surface (`b5f7d5c`) |
| `t32` | delivered | Bounded repair routing (2 attempts / 24h / route-to-human), deployment revision on `/v1/capabilities` and `GET /v1alpha1/version` (`6a20bd4`) |
| `t33` | delivered | Schedules that emit events, affinity routing with the rule name recorded on the run (`9d4b54c`) |

## Mid-work Decisions

Twenty-one deviation records, all `proposed` and awaiting the owner's
`devague deviate --confirm`. The load-bearing ones:

- `d1` — the intra-fleet handover medium is plain ssh git transport, not a GitHub push credential fenced by an org ruleset
- `d2` — codex work packages hand back working-tree changes because a dispatched codex session cannot write `.git`
- `d5` — t1 deferred behind the #98 fix; package K1 re-cut as t12-first, after K1's first dispatch was refused **because my brief text contained `.github/workflows/`** — a live reproduction of #98
- `d6` — the codex sandbox has no network egress, re-routing every network-dependent verification off the agent hosts
- `d8` — t8's gate asks whether every proposed record has been **reviewed**, not whether zero proposed records exist; the latter can never pass against an immutable ledger
- `d9` — the codex lane exhausts by **capability**, not budget
- `d11` — t1 becomes operator-lane work, because K1's own fix correctly forbids every bridge from touching CI configuration
- `d12` — t1's gate proof breaks production code rather than deleting an assertion, because a deleted assertion makes tests pass
- `d13` — t9's remote-side fence is replaced by the sandbox's absence of egress, and the weaker guarantee is **stated rather than glossed**
- `d14` — Go installed on both codex hosts to test whether the lane could reopen for Go work
- `d17` — Go work in the codex lane is capped at build/vet/non-DB tests; seven tasks move to the operator lane
- `d18` — the deployed bridges were stale, so t9/t10's handover shipped working and produced **nothing** in production for three dispatches
- `d19` — narrows d17: codex keeps the **authoring**, the operator lane keeps the **gate**
- `d20` — t21's dispatch was refused for touching `.github/`, and **the brief caused it**; the generator now emits the boundary
- `d21` — t23 splits: codex authors the decision and code, the live cross-fleet demonstration stays in the operator lane

Not covered by any record, captured here directly:

- Migration collisions were resolved at merge three times (t22 vs t33 on `0032`; t23 on `0033`; t24 on `0036`), because parallel packages branch from the same base and cannot see each other's numbering.
- `migrations/pending/` was created at merge, because a correct migration whose own text says "do not apply" is nevertheless applied by every `nodes migrate` if it sits in the applied sequence.

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t9` (`d1`, `d13`) | ssh git transport replaced the ruleset fence; the sandbox's absent egress is what actually fences it, and that is weaker — a sandbox that regains egress removes the fence silently | needs-follow-up |
| `t9` (`d2`) | a dispatched codex session could not write `.git`; five packages handed back working trees before `writable_git` was wired | needs-follow-up |
| `t1` (`d5`, `d11`, `d12`) | deferred behind #98, then moved to the operator lane because no bridge may touch CI; its gate is proved by injecting a defect rather than deleting an assertion | needs-follow-up |
| `t5` (`d6`, `d9`) | the codex sandbox has no egress, and the lane exhausts by capability rather than budget | needs-follow-up |
| `t8` (`d8`) | the gate asks whether each proposed record was reviewed; "zero proposed records" can never pass against an immutable ledger | acceptable |
| `t10` (`d15`, `d16`) | the control plane now runs `git` as a subprocess, and `internal/handover` joins the authority-observed allowlist | needs-follow-up |
| `t10` (`d18`) | the handover shipped working and did nothing in production for three dispatches; a stale bridge and an honest refusal are byte-identical | needs-follow-up |
| `t19` (`d14`, `d17`, `d19`) | Go installed on both codex hosts, then capped at authoring: `socket(2)` returns `EPERM` for loopback and egress alike | needs-follow-up |
| `t21` (`d20`) | the dispatch was refused for touching `.github/`; the brief caused it, and the generator now emits the boundary | needs-follow-up |
| `t23` (`d21`) | split — codex authored the decision and code; the live demonstration ran in the operator lane | acceptable |
| `t24` | the migration is correct and **must not be applied**: t23 chose mixed mode, which keeps `endpoint_ref` as the fallback the rollback depends on. Held in `migrations/pending/`. Merging it into the applied sequence dropped the column and failed fourteen tests | needs-follow-up |
| `t14` | needed an account-admin action (`bootstrap-operator.sh update-policy`) the agent may not run | acceptable |

## Evidence

- go: `go build ./...`, `go vet ./...`, `go test ./...` — all pass
- pytest (root): 301 passed
- pytest (bridges): claude-code 326, codex 340, colleague 286, human-inbox 169, notify 148 — 1269 total
- lint: `black` / `isort` / `flake8` / `bandit` clean; `gofmt -l` → 0
- markdownlint: 124 files, 0 errors
- rubric: `teken cli doctor . --strict` — 26/26 PASS
- openapi: `regen-openapi-json` produces no diff (parity)
- commits: `1e6a532..9425bc2` — 92 commits, 24 merges
- runs: 33 in `docs/triage/cycle-runs.txt`; ledger gate reports **nothing awaiting a decision**
- grades: 30 records — 26 rated 5, 4 rated 4
- issues: 11 opened, 14 closed, delta 3, **0 opened-by-cycle undispositioned**
- AWS: `deploy/aws/preflight.py` — 4 ready, 0 blocked (policy at v6)

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| All 33 plan tasks are accounted for; 32 delivered, 1 partial | high | table above · `1e6a532..9425bc2` |
| Every claim from every dispatched run has a recorded human decision | high | `scripts/ledger-gate.py` → "33 run(s) checked, nothing awaiting a decision" |
| An agent can no longer decide its own claim | high | `internal/ledger/reviewer_test.go` · ablation: stubbing `checkReviewerIsHuman` fails both refusal tests |
| A run id alone collects a handover, with no hand-typed host or branch | high | `scripts/collect-handover.py` · run `01M04FX4B93JKP936ZM800838C` collected end to end |
| A dispatch reaches a bridge the control plane holds no address for | high | run `01M04K4TZVYFQX3W9M9SGTTWK3` completed; `endpoint_ref IS NULL`; mailbox row `claimed t, completed t, 200` |
| Upkeep starts itself on a cadence and survives restart without double-starting | high | 9 events / 9 runs / 9 distinct; one late fire on re-enable, one recovery fire after kill |
| A suite's exit code is recorded as `derived` evidence naming the commit it tested | high | `POST /v1alpha1/runs/{id}/suite-verdicts` · ablation: removing `validateFullSHA` records `verdict=confirm, commit_sha=""` |
| A red gate routes to a bounded repair rather than the operator's session | high | `internal/repair`, 2 attempts / 24h / route-to-human · ablation: emptying `GuardedPathPrefixes` misroutes CI failures |
| The codex sandbox cannot reach any database or listener | high | run `01M04C39ZQK8NHJPQFSG8Z3QNV` — `socket(2)` → `EPERM`, loopback and external alike |
| A `pg_dump` of the credential tables carries nothing presentable | high | `scripts/check-inbound-auth-dump.py` PASS against PostgreSQL 17 · ablation FAILs on a `credential_plaintext` column |
| No bridge could ever have claimed dial-in work before this cycle's last fix | high | `dialin.py` 204 handling · regression test in all five bridges · ablation reproduces `dial-in reconnecting` |
| The operator loop is **not** automated — 14 of 14 steps remain manual | high | `docs/deliveries/close-the-backlog-bootstrap-honesty.md` |
| Mixed mode works with one converted and one unconverted bridge simultaneously | **unverified** | needs two real bridges on two hosts; gated on #111 |
| The revision stamp lands on thor/orin and `/v1alpha1/version` answers from a deployed image | **unverified** | `deploy.sh` was never executed; static assertions + a real wheel build only |
| The repair loop behaves correctly against a real rejecting gate | **unverified** | never exercised on the fleet |
| Migration 0036 applies cleanly | **unverified** | deliberately unapplied; precondition unmet |

## Remaining Work / Follow-up

- **`t24`** — apply `migrations/pending/0036_...sql` only after every bridge is converted to dial-in and the outbound fallback is disabled. `git mv` it up one directory, renumber if 0036 is taken, then `nodes migrate`.
- **21 deviations await `devague deviate --confirm`** — user-only; an agent must never confirm its own proposal.
- **#119** — pick one of four options for database-backed Go in the codex lane; option 1 (ephemeral Postgres on a unix socket) needs an `AF_UNIX` probe first, and is the only one that costs the no-egress fence nothing.
- **#120** — a bridge that cannot hand over must *refuse* the dispatch asking for it; attempted-but-empty must be distinguishable from not-attempted.
- **#117** — audit every origin-stamping site for the class behind t30's finding.
- **#118** — make the combining step a node. Fifteen hand-merges this cycle, and the last cycle's STATE predicted the recurrence.
- **Bridge redeploy is part of landing a bridge change.** The codex bridges are copied installs and go stale; the claude bridges are editable and never do. The lane that would have caught this is the one that structurally cannot.
- **Suspected flake** — `TestSchedulerDeadlineFailsAParkedRunnerOperation` failed once under the full parallel sweep and passed on every subsequent run. Not reproduced, not fixed; recorded as an absence of evidence.
