# Delivery Summary — jira-driven idea-to-shipped loop

plan: `jira-driven-idea-to-shipped-loop` · run: `partial` · date: `2026-08-18`
baseline: `devague summary skeleton`

## Intent

> A Jira issue moves to the right state and, with no operator shell command in
> the transcript, a PR appears against a feature branch whose every
> constituent task was dispatched, gated, merged and decided by the system -
> the idea-to-shipped loop runs as Culture Nodes flows and the combining step
> is a node instead of the operator

One cycle: spec (scope → think → challenge, 38 claims / 28 honesty
conditions), a 20-task plan over 8 waves, executed by two codex actors
(thor/orin), five local subagents, and the operator lane — then demonstrated
live on a local full stack (real engine, real Postgres, real headspace
runner, real Jira).

## Planned Work

Quoted verbatim from the `devague summary` skeleton (t1–t20; the full list is
in that skeleton and `docs/plans/2026-08-18-jira-driven-idea-to-shipped-loop.md`):

- `t1` — Harvest node (#100) … through the internal/handover seam
- `t2` — Candidate staging + .github containment
- `t3` — Gate the combination … never the package alone
- `t4` — Merge execution with named credential custody
- `t5` — The combining-loop workflow … compiling with 0 errors
- `t6` — Wave release consults pacing
- `t7` — Self-verifying steps
- `t8` — Stage-1 demonstration … zero operator shell commands
- `t9` — Sweep evolution: transition event names + self-echo filter
- `t10` — The Jira comment actor
- `t11` — The question round trip
- `t12` — Session identity through the gap
- `t13` — Claim decisions round-trip through Jira
- `t14` — The bounded question loop
- `t15` — One active run per issue
- `t16` — Concurrency policy (#166)
- `t17` — Stage-2 live proof on the seeded backlog
- `t18` — The spec chain as a graph (#89)
- `t19` — Registration residue (#8), take-or-defer
- `t20` — PR wiring and the end-to-end demonstration

## Actual Delivery

| Plan task | Status | What actually landed |
|-----------|--------|----------------------|
| `t1` | delivered | `cmd/nodes-harvest` + `internal/handover/harvest.go` (codex-thor run `01M09R52ZS…`); duplicate test helper deduped at staging |
| `t2` | delivered | candidate staging with `merge_conflict`(1)/env(2)/`routes_to_human`(3) exit contract (codex-thor `01M09SZ638…`) |
| `t3` | delivered | gate measures the detached candidate merge commit; all-skips → `no_tests_executed` → `measurement_incomplete`; combination-red fixture (codex-thor `01M09TJXDJ…`) |
| `t4` | delivered | `cmd/nodes-merge` + `internal/handover/merge.go`: verdict/SHA fencing, atomic update-ref, push with credential-helper RESET (staging repair for the GIT_ASKPASS-outranking trap) (codex-thor `01M09VGH2P…`) |
| `t5` | delivered | `examples/combining-loop/workflow.yaml` (16th compiling example) + `scripts/combining-loop-node.py` (922 lines, 27 tests) + `scripts/combining-loop-release.py`; remote-durable ref carrier |
| `t6` | delivered | test-only: the seam measured complete — triggered and manual runs share `EnqueueWork`, so pacing/breaker/ceiling already gate both; 3 pinning tests on the real inbound path |
| `t7` | delivered | measured post-conditions in every subcommand (`ls-remote` after push; `event.id` after POST; h7's exit-0-but-failed shape demonstrated both ways) |
| `t8` | partial | the full loop ran live: event → harvest ✓ → stage ✓ → gate `gates_passed` (real unittest suite) → merge ✓ (remote feature tip == gated candidate `2abf1b37`) → claim decision parked for the human. Partial because it ran under the d6 adaptations (apt-git demo variant; #178 workaround) and the claim decision awaits the human |
| `t9` | delivered | `pr-upkeep.jira.transitioned.<slug>` vs `.comment` (never colliding), `jira_bot_account_id` filter; found+fixed #168 (watermark read GitHub field names) |
| `t10` | partial | `adapters/jira` comment-only bridge + audit + deploy lane (codex-orin `01M09R58Z4…`); partial harvest — the delivery also removed the sweep's read path and deleted a prod secret file, rejected at staging (d1) |
| `t11` | delivered | marker-stamped comments, marker-OR-author echo filter, `originating_question_id` events, fault-injection transaction test (ran green here; its sandbox could only SKIP it) (codex-orin `01M09SZ68E…`) |
| `t12` | delivered | digest `session_key`, warm-resume-no-fork, single cold retry with ForkEvent + re-brief; staging repair preserved the pinned no-identity-unchanged invariant (codex-orin `01M09TJXJ8…`) |
| `t13` | delivered | decision-comment format documented, conservative verb+id parser reusing decide-claims custody, review names the transcribed comment (26 tests) |
| `t14` | delivered | structural two-ask bound, `onExhausted` → human (compiler refusal spot-verified); resolved risk r2: a signal wait has NO timeout today (#173) |
| `t15` | delivered | `runs.subject` + advisory-locked attach-to-existing-run dedup at the trigger seam; 4 postgres-backed tests |
| `t16` | partial | subject-run ceiling (transactional deferred-trigger queue) + per-actor in-flight ceiling delivered; tag-pinned placement measured absent and deferred to #175 (d5) |
| `t17` | partial | live: the engine posted a REAL question on SCRUM-1 through the jira actor and parked on `until.signal`; one real sweep pass emitted `transitioned.to-do` for SCRUM-1 and correctly suppressed the bot's own comment. The resume half awaits the human's Jira reply (watcher armed); warm-vs-fork was not yet exercised live |
| `t18` | delivered | `examples/spec-chain/workflow.yaml` (22 nodes, compiles) + 7 lint tests + a measured 7-item gap list that drove #170's fix and deviations d2/d3 |
| `t19` | delivered | runner-services live reload (no worker restart — used live during the demo), namespace-discovery found already-closed and corrected; `docs/decisions/2026-08-18-registration-residue-take-or-defer.md` |
| `t20` | partial | the PIECES ran live (Jira-driven trigger, the merged feature branch, this PR as the cycle's artifact) but no single unbroken Jira → PR thread executed machine-only; blocked on the prod deploy (operator permission), headspace-cli#24, and #178 |

## Mid-work Decisions

- `d1` — t10 partial harvest: the sweep's Jira READ path survives — c5's
  "holds only the event-ingress token" over-compressed the #76 design
- `d2` — c13 corrected: the artifact carrier is one-way (ingest yes, read
  side no — #171)
- `d3` — c6 corrected: "human-inbox approval nodes" was a non-existent
  composition; the engine's own approval surface is the human gate
- `d4` — scope addition: #170 fixed in-cycle (NODES_INPUT_JSON into code
  operations) — every honest-gap list converged on it as the runtime blocker
- `d5` — t16 defers tag-pinned placement to #175 (no tag concept exists
  anywhere placement resolves)
- `d6` — t8's demo adaptations: local apt-git workflow variant
  (headspace-cli#24) and the #178 attempt-FK workaround
- Not covered by a record: the t9 merge pushed a test file over the
  1000-line gate (caught by the combination gate, split); openapi.json
  regenerated at t3 staging; codex version bumps rejected at staging twice
  (one bump at PR time); one operator gate-report aggregate was injected
  into demo run `01M0A0R8…`'s ledger while diagnosing #178 (stated in d6's
  record).

## Drift From Plan

| Plan item | Reason for divergence | Classification |
|-----------|-----------------------|----------------|
| `t10` (`d1`) | spec boundary c5 over-compressed the #76 design; the finding source and resume emitter would have died | needs-follow-up |
| — (`d2`) | c13 as confirmed overstated #79's closure; measured at HEAD during t18 | needs-follow-up |
| — (`d3`) | c6 named a composition with no wire; vocabulary conflated two human surfaces | acceptable |
| — (`d4`) | #170 was not a plan task; documented-around is not runnable | acceptable |
| `t16` (`d5`) | measured absence of any tag concept; forcing it through the wrong seam would fake the acceptance | acceptable |
| `t8` (`d6`) | one git-less headspace profile; ledger FK on attempts written only at completion | acceptable |
| `t17` | the warm/fork live proof needs the human's reply and a forced session loss; only the ask/park/filter halves ran live | needs-follow-up |
| `t20` | no single machine-only Jira→PR thread; blocked on prod deploy permission + headspace-cli#24 + #178 | needs-follow-up |

## Evidence

- tests: `uv run pytest -n auto` — **494 passed**; `go test ./...` green per
  package across the merges (postgres-backed suites via docker);
  `scripts/lint-all.sh` — all three CI lint jobs pass;
  `scripts/validate-examples.sh` — all 16 examples compile, 2 gate matrices
  readable
- commits: `a45f10ea..5b3c3f9` (57 commits on `spec/jira-driven-idea-to-shipped-loop`)
- live runs (local stack, ns_demo): `01M0A0YFYJ72HSX4829GCXHQTD` (the full
  green loop), `01M0A13E33F27WJ5PKKRSH4YFD` (the parked Jira round trip),
  plus the diagnostic runs named in d6
- remote proof: `refs/heads/spec/demo-feature == refs/culture-nodes/candidate/01M0A0YF… == 2abf1b37…`
  with per-gate + aggregate + acceptance `derived` records in the demo ledger
- Jira proof: one bot-authored marked comment on SCRUM-1; one live sweep pass
  emitting `pr-upkeep.jira.transitioned.to-do` and suppressing the bot's echo
- issues: #166–#179 filed this cycle (typed; all dispositioned);
  agentculture/headspace-cli#24 cross-repo
- codex runs: `01M09R52ZS… 01M09R58Z4… 01M09SZ638… 01M09SZ68E… 01M09TJXDJ… 01M09TJXJ8… 01M09VGH2P…` — all graded, claims pending human decision

## Delivery Claims

| Claim | Confidence | Evidence |
|-------|------------|----------|
| The combining loop exists as a committed compiling workflow with a tested node program | high | `examples/combining-loop/workflow.yaml` · `scripts/combining-loop-node.py` · 27 tests |
| The loop ran live: machine harvest→stage→gate→merge with the merge equal to the gated candidate | high | run `01M0A0YFYJ72HSX4829GCXHQTD` · remote SHA `2abf1b37…` · ledger gate records |
| A question posted by the engine sits on SCRUM-1 and the flow is parked awaiting the human | high | run `01M0A13E33F27WJ5PKKRSH4YFD` · SCRUM-1 comment |
| Transition-named Jira events flow live and the self-echo filter works on real data | high | signal_events row `pr-upkeep.jira.transitioned.to-do` · zero comment events on the bot's own comment |
| Subject dedup attaches a second mid-flight event to the existing run | medium | postgres-backed tests (t15); the live two-event probe hit the terminal-run path instead |
| Warm resume / fork-observable works live across the Jira gap | unverified | awaiting the human's reply (and a forced loss) — not claimed done |
| A single machine-only Jira→PR thread completes | unverified | blocked (t20) — not claimed done |
| Two engine bugs found live are fixed on this branch | high | #177 (`runnerasynccompletion_test.go`), #168 (sweep field names); #178 remains open with workaround |

## Remaining Work / Follow-up

- `t17` — CLOSED after the summary was first written: run
  `01M0A5QG2Q0EDG16BEFG9MG4TZ` completed the full round trip live (engine
  question on SCRUM-1 → human reply → filtered resume → continuing actor →
  terminal, claim `proposed` under the registered actor). Three live
  failures on the way each exposed a distinct defect: an under-declared
  `ledger.propose` (fixed in the committed example), and the silent
  origin-identity budget burn (#183). The forced-session-loss fork half
  remains test-pinned rather than live-forced.
- `t20` — prod deploy (operator permission was denied to the agent —
  `deploy/prod/deploy.sh thor` is the user's command), headspace-cli#24, and
  a #178 fix, then re-run the single-thread demonstration.
- #178 — pick fix 1 (durable attempts row at dispatch) or 2 (lenient
  attempt_ref) and land it.
- #175 — tag-pinned placement (the deferred #166 leg).
- #173 — a signal wait needs a timeout edge (silence parks forever).
- Human review queue: deviations d1–d6 (`devague deviate --confirm …`;
  Records #169 #174 #176 #179), the frame amendments c5/c6/c13, the seven
  codex completion claims + the demo claim decision, and SCRUM-1's answer.
- Operator hand-turn count for this cycle (the #118 measurement): checkout
  refreshes ×2 hosts ×3 waves, 7 harvest/stage cycles (now automatable by
  the very loop this cycle built), 1 raw namespace INSERT (#8's residue),
  local-stack bring-up, and the demo's env/digest churn — the loop that
  removes them is what this cycle shipped.
