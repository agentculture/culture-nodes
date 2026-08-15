# own-the-work-end-to-end — delivery summary

**Batch:** eleven issues behind [#87] — 76, 77, 78, 79, 80, 81, 82, 83, 84, 86.
**Spec:** `docs/specs/2026-08-14-own-the-work-end-to-end.md` (75 claims, 54
honesty conditions, 49 scope entries, three challenge passes).
**Plan:** `docs/plans/2026-08-15-own-the-work-end-to-end.md` (26 tasks, 8 waves,
100 coverage targets).
**Branch:** `owe/batch`. **Deployed to thor** at `c407fa8` before the live test.

---

## 1. The six success signals, each with a verdict

The spec (claim c35) declared six falsifiable signals. Three are met, one is
partly met, two are **not met**. Nothing here is marked met because the code
shipped.

### (1) A node killed mid-write has its session stopped and its work recovered from the run alone — **MET, by test; NOT exercised in production**

t9 makes a fired deadline cancel the actor session after the timer transaction
commits, and t13 proves the preserve path fires on a cancelled session — with
the assertion being **a ref in the repository**, not the bridge's payload,
because a payload is the bridge's claim (§10.4) and `preserve_branch: None` is
exactly the case where believing it was the mistake. Mutation-checked: stubbing
`if ev.kind == "failed":` to `if False:` fails both tests.

Not exercised on the live fleet. The live test did not kill a node mid-write.

### (2) The pr-upkeep fix→review handoff completes across two machines with no path passed and no operator copy step — **NOT MET**

t6 landed the **contract** — one declaration in `schemas/workflow/handoff.schema.json`,
both surfaces constrained, and a guard proving a bare filesystem path is still
refused at all three surfaces (eleven mutation checks). What it did not land is
the **transport**, and the run said so rather than glossing it. Three blockers,
all named:

- t5 mounted artifact **publish** only; there is no fetch route;
- nothing publishes the handover ref to a remote, because the producing agent
  is forbidden to push (correctly) and the operator/control-plane move is
  unbuilt;
- `handover_ref` has no dispatch call site.

The live test moved a work-item list from spark to orin **inside the dispatch
instruction** — an operator copy step. That is the honest reading: cross-host
review happened, the two-carrier handoff did not.

### (3) Cancelling a run ends the actor session, verified by observing the session end — **NOT MET**

t9 built the path and a `tests/lint` guard keeps the cancel after commit and off
the tick loop. But it was never observed live. No run in this batch was
cancelled and watched for its session ending.

### (4) `usage_model` on every fresh attempt across all four backends — **MET, and it took the live test to make it true**

t14 surfaced the field, t15 gave colleague and notify explicit sentinels — and
the live test then read three fresh attempts back through the API and found
`usage_model` **NULL on every one**. Five for five across four claude bridges
and a codex run, after the batch believed the issue closed.

The cause was omission, not a write failure: both bridges set `model` only when
it is unambiguous, and a `claude -p` session that ran subagents reports several
entries in `modelUsage`, so the bridge named none. t15 had skipped these two
because the spec measured that they "already report model" — true when a model
is determinable, false otherwise, with nothing distinguishing the cases.

Fixed, deployed, and re-verified live:

```text
usage_model  : 'unknown:claude-code-session-did-not-report'
tokens in/out: 4 / 167
cost/currency: 0.24366200000000002 USD
```

Deliberately a different sentinel from colleague/notify's: `cannot-report` is a
permanent property of a backend, `did-not-report` is a gap in one attempt.
Collapsing them would let a bridge that silently stopped reporting models read
exactly like one that never could.

### (5) This cycle's `-STATE.md` would not have needed to exist — **NOT MET**

It exists, it is 260 lines, and the work could not have continued through a
context compaction without it. Section 11 inventories why: fourteen operator
steps per package, none of them expressible in the system today.

The batch's own artifacts answer this signal in the negative and say so.

### (6) The human acts only at the three devague gates and at ledger promotion — **NOT MET, and this is the sharpest failure**

Measured: nine work-package runs produced nine ledger records. **All nine are
still `proposed`.** Zero confirmed, zero rejected, zero derived, zero observed.

Every one *was* decided — read, gated, repaired, merged. None of those decisions
is anywhere a reader of the run can find. The PRD's authority model (§10.4) is
half-implemented: the refusal half works perfectly, nothing self-promoted; the
affirmative half does not exist. Issue [#99].

Beyond ledger promotion, the human also harvested every diff, ran every gate,
repaired every gate failure, wrote every commit message, performed every merge,
and ran the deploy.

---

## 2. Correcting the previous delivery

`docs/deliveries/2026-08-14-economy-discord-graphs.md:194` records:

> | A preserve branch is visible on the run detail page | medium | t26
> store/API/web tests; not live-exercised |

That row was honest about its evidence and is now **superseded rather than
contradicted**: t13 of this batch exercised the preserve path on a cancelled
session and asserted on a real git ref. The production sample size for that
claim at the time it was written was **zero**, and the row should be read that
way — "verified by test, never exercised in production" — not as either high
confidence or as false.

---

## 3. What was delivered

23 of 26 tasks merged; t24 (self-test) passed; t25 (live test) ran partially;
t26 is this document.

| Issue | Task(s) | Outcome |
|---|---|---|
| #72 | t1 | `EnvironmentFile` seam on every bridge unit; host derived from the actor registration |
| #84 | t2 | human-inbox lane renamed; the deploy now removes legacy unit files, not just disables them |
| #83 | t3 | bwrap is authoritative; sysctls demoted to explaining a failed probe; `not-probed` is a third state |
| — | t4 | four over-limit files split along real seams; a 1000-line gate, with a test for the gate |
| #79 | t5 | `internal/artifacts` has its first production caller: an authenticated attempt-scoped write route |
| #74 | t6 | the two-carrier handoff **contract**, with the bare-path refusal proven to survive the widening |
| — | t7 | artifact tombstones, so reaping cannot make an immutable record lie |
| #80 | t8 | `continue.while` / `bounds` / `onExhausted` compile and the engine evaluates them |
| #82 | t9 | a deadline cancels the session after commit — and a pause now **re-arms** |
| #78 | t10 | a deadline-origin timeout is no longer retried; unknown origin fails closed |
| — | t11 | a late callback appends a superseding attempt row instead of vanishing |
| — | t12 | a third cancel origin on the existing ORIGIN axis |
| — | t13 | preserve-on-cancel proven and pinned against a real git ref |
| #77 | t14, t15, + the live fix | attempt attribution end to end, sentinels on all four backends |
| — | t16 | bridge-side worktree minting; scoped-prefix allowlists; nested worktrees refused |
| — | t17 | a reaper that refuses dirty worktrees unconditionally and preserves unreferenced work first |
| — | t18 | the handover-gate design: numbers per gate, explicit `not_applicable` |
| — | t19 | the sweep source derived from the revision the deploy ships |
| #86 | t20 | pr-upkeep is a configured repo set, not one pinned repo |
| — | t21 | the development loop as a compiling graph that names seven of its own gaps |
| #81 | t22 | generate a workflow from text, in front of the existing publish door, with a model-egress guard |
| #76 | t23 | Jira as a fixture-backed source; live proof separately gated and still blocked |

**t24 self-test, all green:** Go build/vet/test; 1123 adapter tests across five
bridges; 262 root pytest at **95.5%** coverage; 497 web tests; web build;
black, isort, flake8; bandit 0 findings at every severity; markdownlint; the
teken rubric; 12 of 12 example workflows compiling.

---

## 4. Drift from plan

Nine deviations, all recorded at the time (`devague deviate --list`):

| id | what |
|---|---|
| d1 | one package per codex actor — each bridge allowlists exactly one checkout |
| d2 | refresh the agent checkouts; they were two commits behind |
| d3 | commit and push spec + plan so briefs cite them |
| d4 | reconcile decision c26 against the committed unit tests |
| d4* | sandbox posture — superseded by d6 |
| d5 | self-contained briefs, to unblock wave 0 without answering #91 |
| d6 | #91 resolved **by measurement**: keep `workspace-write`, widen `.git` per dispatch |
| d7 | split t4 — the lint lands with the file splits, not before |
| d8 | q5 settled on per-artifact read capabilities; the spec's TTL premise falsified |
| d9 | rebalance routing to 70% claude / 30% codex |

Two are worth reading as findings rather than bookkeeping. **d6** replaced a
four-way guess with a measurement: `codex --sandbox workspace-write` leaves
`.git` read-only, and one scoped `writable_roots` entry lifts it, so the
`git_ref` carrier needed neither `danger-full-access` nor to be dropped.
**d8** falsified a premise in our own spec: q5 assumed a 30-minute upload
outlives a 15-minute token, but dispatch mints with `MintUntil(deadline+grace)`,
so it does not.

---

## 5. Which actor is better at what

The comparative record CLAUDE.md asks for, from nine work packages.

**codex (thor/orin) — strong at grounded analysis, cannot verify itself.** The
q5 authorization study cited ~25 exact locations and falsified a premise in the
spec. But those hosts have no Go, no npm and no working `uv` ([#96]), so every
codex build package arrived with tests that had never been executed, and every
one needed operator repair.

**claude (spark bridges) — verified its own work, and did the discipline
unprompted.** All four claude packages ran their full suites before reporting.
Two mutation-checked every property they added (t6 checked eleven separately);
one caught a test of its own that passed *vacuously*; one wrote an ADR before
its migration; one measured two things that broke its first version and would
never have surfaced off-machine (reflog rewrites by `gc`, and `git status`
refreshing the index).

**The routing consequence**, recorded as d9: build work belongs where the
toolchain is; analysis and cross-machine attribution are what the codex lane is
worth paying the cold-session tax for.

---

## 6. What the live test actually proved

Three legs ran against the deployed tree.

- **Leg A (sweep, spark)** produced five cited work items — and found a real
  defect in this batch's own work: the 1000-line gate admits only code
  extensions, so `openapi.json` at 4357 lines is invisible to it.
- **Leg B (review, orin — a different machine)** verified all five
  independently and **refuted one** with evidence: t17's reaper does have a
  production caller, so the sweep's claim was stale. Two nodes on two machines
  disagreeing, with the second winning on evidence, is the review step working.
- **Leg C (attribution)** is what turned signal 4 from false to true.

What it did not prove: the two-carrier handoff (signal 2), and cancellation
observed at the session (signal 3). Both are recorded as not met above.

**The companion-document test**, honestly: a reader given only these runs could
reconstruct what each agent claimed. They could not reconstruct what was
believed, what was broken, what was repaired at the gate, or why three packages
merged while partial. That is signal 5, answered in the negative.

---

## 7. Issues opened

Eleven, of which six are the operator-lane inventory in `-STATE.md` §11.

| # | what |
|---|---|
| [#88] | widen SonarCloud, measure a baseline, ratchet |
| [#89] | run scope → think → challenge through Culture Nodes |
| [#90] | worker push credential (carries a correction — its first analysis was wrong) |
| [#91] | **closed** — resolved by measurement, see d6 |
| [#92] | the operator surface truncates ledger claims |
| [#93] | every dispatch needs the operator to hand-prepare the checkout |
| [#94] | the capability surface omits `.git` writability |
| [#95] | `continue.while` evaluates a hardcoded `node.state` |
| [#96] | the capability surface omits toolchains |
| [#97] | model subscription budgets as first-class records |
| [#98] | the workflow-scope guard matches the instruction text |
| [#99] | **nothing ever decides a proposed claim** |
| [#100] | harvest is an operator ssh command |
| [#101] | the merge gate runs in the operator's session |
| [#102] | gate failures are repaired by hand |
| [#103] | briefs are hand-written heredocs |
| [#104] | nothing records what revision is deployed |
| [#105] | an erroring `continue.while` is indistinguishable from a false one |

---

## 8. The honest headline

The batch built most of what #87 asked for and **did not remove the human from
the loop**. It made the loop legible enough to say exactly where the human still
is — fourteen steps per package, inventoried, each with an issue — and it
closed #77 only because a live test caught the batch's own claim being false.

Two things it did better than the previous cycle: every deviation was recorded
when it happened rather than reconstructed, and every gate failure was caught
before merge rather than after.

One thing it did worse than it should have: nine runs produced nine undecided
claims, and nobody noticed until the count was computed by hand.

[#87]: https://github.com/agentculture/culture-nodes/issues/87
[#88]: https://github.com/agentculture/culture-nodes/issues/88
[#89]: https://github.com/agentculture/culture-nodes/issues/89
[#90]: https://github.com/agentculture/culture-nodes/issues/90
[#91]: https://github.com/agentculture/culture-nodes/issues/91
[#92]: https://github.com/agentculture/culture-nodes/issues/92
[#93]: https://github.com/agentculture/culture-nodes/issues/93
[#94]: https://github.com/agentculture/culture-nodes/issues/94
[#95]: https://github.com/agentculture/culture-nodes/issues/95
[#96]: https://github.com/agentculture/culture-nodes/issues/96
[#97]: https://github.com/agentculture/culture-nodes/issues/97
[#98]: https://github.com/agentculture/culture-nodes/issues/98
[#99]: https://github.com/agentculture/culture-nodes/issues/99
[#100]: https://github.com/agentculture/culture-nodes/issues/100
[#101]: https://github.com/agentculture/culture-nodes/issues/101
[#102]: https://github.com/agentculture/culture-nodes/issues/102
[#103]: https://github.com/agentculture/culture-nodes/issues/103
[#104]: https://github.com/agentculture/culture-nodes/issues/104
[#105]: https://github.com/agentculture/culture-nodes/issues/105
