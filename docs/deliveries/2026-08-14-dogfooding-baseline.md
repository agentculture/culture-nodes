# Dogfooding baseline — how much of this batch went *through* culture-nodes

Requested by the operator during the economy-discord-graphs cycle:

> How many tasks were handled by culture-nodes/bridges so we can see
> improvement over time. (Count and % of overall tasks, also type of tasks and
> models + effort that took the jobs)

This is that baseline. It is deliberately unflattering, because a baseline
that flatters is not a baseline.

## The headline

**0 of 35 plan tasks (0%) were executed through culture-nodes.**

Every plan task was built *beside* the product — by local Claude subagents, by
`codex exec` invoked directly, by the `colleague` CLI invoked directly, or by
the operator. The control plane orchestrated none of the work that built it.

The product was used in this cycle, but only to **prove itself**, never to
**build itself**.

## What did run on the control plane

Empirical, from the deployed Postgres on thor (all runs ever recorded, not
just this batch):

| workflow | completed | failed | cancelled | running | what it is |
|---|---|---|---|---|---|
| `pr-upkeep` | 1 | 8 | 1 | 1 | the closest thing to real work |
| `codex-smoke-pair` | 2 | 2 | 4 | — | smoke test |
| `placement-proof` | 3 | 3 | — | — | proof |
| `cross-machine-proof` | 1 | 3 | — | — | proof (this batch) |
| `notify-message` | 1 | 1 | — | — | proof (this batch) |
| `parallel-live-proof` | 1 | 1 | — | — | proof (this batch) |
| `signal4-check` | 1 | 1 | — | — | proof (this batch) |
| `self-hosting-loop` | 1 | 1 | — | — | proof |
| `delivery-loop` | — | 1 | — | — | proof |

**61 runs, 108 attempts, across the system's entire history.** Every workflow
in that list is a proof, a smoke test, or a demo. `pr-upkeep` is the single
exception and it is a maintenance sweep, not a plan task.

Attempts by actor during this batch (from 2026-08-13):

| actor | attempts | machine |
|---|---|---|
| `company/developer` (claude-code) | 16 | spark |
| `headspace/docker` (runner) | 15 | thor |
| `company/codex-thor` | 12 | thor |
| `company/intake` (claude-code) | 5 | spark |
| `company/planner` (claude-code) | 3 | spark |
| `company/verifier` (claude-code) | 3 | spark |
| `company/human-ops` (human inbox) | 3 | thor |
| `company/notify-discord` | 1 | thor |

## How the 35 plan tasks were actually executed

| lane | tasks | through the product? |
|---|---|---|
| Local Claude subagent (Sonnet) | t5, t6, t7, t22, t23, t24, t25, t26, notify actor | no |
| `codex exec` run directly (gpt-5.6-sol, effort high) | t35, and earlier-wave tasks | no |
| `colleague` CLI run directly (Qwen, self-hosted) | t15, t18 (partial), t5 (abandoned) | no |
| Operator, in-session | deploy fixes, doctor check, colleague resume, all merges and gating | no |

**Effort settings**: codex lanes ran `model_reasoning_effort=high`. Subagents
inherited session defaults. Colleague runs used its own pinned Qwen model.

### The attribution gap, which is itself a finding

Most of this table is reconstructed from the operator's session memory and
from commit-message prose, **not** from anything the system recorded. Only 8
of 35 task commits carry an attribution line at all
(`git log` grep for "built by"). There is no query that answers "which actor
built task N" because nothing writes that down.

That is precisely what issue #28 (per-actor analytics and first-class grading
records) exists to fix, and this table is the argument for it: a
dogfooding-improvement metric cannot be tracked over time while the numbers
have to be remembered rather than queried.

## Why it came out this way

Worth recording honestly rather than as excuses, because each of these is a
fixable cause:

1. **The bridge hosts could not run codex sandboxed.** All three machines
   carry `kernel.apparmor_restrict_unprivileged_userns=1`, so codex's
   bubblewrap sandbox cannot start (#63). Two dispatched codex lanes produced
   zero files before this was understood.
2. **The claude bridges were running four-month-old code** — no resume
   support — until they were restarted mid-cycle. Anything dispatched to them
   earlier would have cold-started every turn.
3. **A bridge could not commit a ledger record at all.** Both the human-inbox
   and notify bridges reported an actor *key* where the ledger's foreign key
   wanted an actor *row id*, so every terminal commit rolled back. Found only
   by running a real workflow end to end.
4. **The merge tracker was crash-looping for nine hours** on a stale checkout
   (6272 restarts), so the one human-in-the-loop lane was dead.
5. **Dispatching through the product is slower for the operator** than
   spawning a subagent, and under a deadline the fast path wins unless the
   slow path is genuinely better. Today it is not, for the reasons above.

Causes 1–4 were all fixed during this cycle. Cause 5 is the real one, and it
is not fixed.

## What "better" would look like next cycle

A concrete, checkable target rather than an aspiration:

- **≥ 1 plan task executed end to end through culture-nodes**, with its
  ledger records as the evidence of completion. Anything above zero is a
  qualitative change; the current state is not "low", it is "none".
- **Attribution queryable, not remembered** — #28's per-actor records, so
  this table can be regenerated by a query instead of rebuilt by hand.
- **The comparative record the repo's own conventions ask for**: which actor
  is better at what. Impossible to build today, because the sample of
  through-the-product task executions is empty.

## Method

- Run and attempt counts: direct SQL against the deployed Postgres on thor.
- Lane attribution: `git log` on `feat/economy-discord-graphs` plus the
  operator's session record, with the gap above stated rather than papered
  over.
- "Through culture-nodes" means the control plane dispatched the work to an
  actor and recorded the result. Driving a bridge's HTTP surface directly
  (as task t7's A/B harness does, 27 invocations) is **not** counted — it
  exercises the actor protocol but bypasses the engine, the ledger and the
  dashboard.
