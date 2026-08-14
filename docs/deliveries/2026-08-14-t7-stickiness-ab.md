# Delivery Artifact — t7: the stickiness A/B gate

task: `t7` (economy-discord-graphs) · covers: `c42` · honesty: `h35`, `h2` ·
date: `2026-08-14`

## What t7 asked for

> Stickiness A/B gate: ten representative tasks through fresh sessions vs
> one resumed thread; comparison artifact (uncached input, cached input,
> sessions, failures, wall time) recorded; stickiness stays opt-in until
> the artifact shows uncached-input reduction.

The plan's own honesty condition governs everything below: *"the A/B
comparison exists as a recorded artifact before stickiness defaults on; if
resumed sessions do not measurably reduce uncached input, stickiness stays
opt-in and the spec's economics claims are re-examined"* (spec honesty h2).
t7 does not have to prove stickiness works. It has to produce an honest
artifact and leave the gate exactly where the evidence puts it.

## Summary of the result

**A real, live measurement was collected** against the actual deployed
claude-code fleet — not a fixture-only stand-in. Two independent six-task
runs (12 tasks per run: 6 cold, 6 warm) were driven through the real
`developer` claude-code bridge on `spark`. **Session resume itself works
correctly** — the warm arm collapsed to one provider session across 5 of 6
resumed turns, confirmed independently by claude's own reported `session_id`
staying identical across the resumed turns (not merely by this harness's own
request construction). **On the formally required metric — uncached input
tokens — stickiness showed a 0.0% reduction in both live runs.** A
secondary, not-formally-required cost signal (`usage.cost`, already on the
wire) showed a large apparent reduction in the first run and a small
*increase* in the second, which turned out to trace to a genuine gap in
what the wire protocol reports (see "The `cache_creation_input_tokens` gap"
below), not to stickiness itself.

**Verdict: `no_reduction_or_regression`. Stickiness stays opt-in.** The one
metric the honesty condition names did not move. See "Where the gate was
left, and why" for the full reasoning, including why the tempting cost
number in run 1 is not treated as counter-evidence.

## What was built

`scripts/stickiness_ab.py` — a stdlib-only (no third-party dependencies,
matching this repo's `culture_nodes` convention), operator-runnable harness
that speaks the actor protocol's HTTP wire surface (`POST /v1/invocations`,
PRD §13.1/§13.2) directly against a *running* bridge process, from outside
it. It never imports, patches, or touches `adapters/*/src` — task t25 is
editing all three bridges concurrently, and the harness's whole point is to
be an ordinary client of whatever they expose on the wire, unchanged.

- **Two arms, measured identically.** `run_cold_arm` sends one fresh
  session per task (no `session_key`, no `continuation_ref`).
  `run_warm_arm` sends one persistent thread across every task, chaining
  each reply's own `continuation_ref` into the next request via a shared
  `session_key` (the transport field task t5/t6 exclude from every
  bridge's Bound-inputs block). Both arms are reduced to an `ArmSummary` by
  the exact same `summarize` function — there is no per-arm branch in it,
  so a bug that favored one arm's arithmetic over the other's is
  structurally not possible.
- **Sync or async, whichever the bridge picks.** Every request sets
  `input.async: false` as a hint and carries a real callback block; a
  `CallbackReceiver` (a tiny loopback `http.server` opened once per run)
  catches the terminal `completed`/`failed` §13.4 event when a bridge
  ignores the hint and answers 202 — which every real deployed claude-code
  bridge on this fleet does (`always_async: true` in
  `~/.config/culture-nodes-bridges/*.json`). This was not anticipated when
  the harness was first drafted against the fixture-only plan; the live
  fleet forced it, and it is now exercised by the live runs below, not just
  asserted.
- **Honest cache-telemetry handling.** `cached_input_tokens` is `None`, not
  0, whenever a bridge doesn't report it (ADR 0009's own rule). A task with
  unmeasurable cache telemetry is excluded from the uncached average, not
  folded in as if fully cached or fully uncached. Session count is the
  number of *distinct* `usage.thread_id` values an arm's replies actually
  reported, never assumed from task count. A warm-arm request only counts
  as `resumed` when the prior reply actually returned a `continuation_ref`
  to carry forward.
- **`compare`'s decision table** (`scripts/stickiness_ab.py`) never
  produces a verdict stronger than `insufficient_data` when either arm ran
  zero tasks, reported no cache telemetry at all, or fell under `min_n`
  measured tasks; it only calls a reduction `reduction_demonstrated` (the
  one verdict that flips `recommend_default_on` to `True`) at or above a
  configurable threshold (default 10%) and at or above `min_n` (default
  10, matching the plan's own "ten representative tasks" language) per arm.

## The `cache_convention` bug the live run caught

The harness's first draft computed "uncached input tokens" as
`input_tokens - cached_input_tokens`, on the unstated assumption that
`cached_input_tokens` is a *subset* of `input_tokens` (the OpenAI/Codex
family's documented convention: `prompt_tokens_details.cached_tokens` is
counted *within* `prompt_tokens`). The first live call against the real
`developer` bridge produced a warm task with `cached_input_tokens` (36,739)
*larger* than `input_tokens` — impossible under that assumption, and a
concrete, reproducible signal that Anthropic's real convention is the
opposite: `input_tokens` in Claude's own `usage` block already **excludes**
both `cache_read_input_tokens` and `cache_creation_input_tokens` — they are
disjoint categories, not overlapping ones. Subtracting again double-counted
the cache and could even go negative.

This was fixed before any number in this artifact was trusted:
`uncached_input_tokens(record, cache_convention=...)` now takes an explicit,
required `cache_convention` (`"additive"` — Anthropic's confirmed behavior,
`uncached = input_tokens` directly — or `"subset"` — the OpenAI-style
formula, offered for a future codex run this artifact does **not** claim to
have verified). `summarize`'s own `cache_convention` parameter has no
default at all — a caller must say which backend it is measuring, because
guessing wrong produces a plausible-looking wrong number rather than an
obviously broken one. 6 new fixture tests
(`tests/test_stickiness_ab.py::test_additive_convention_*`,
`test_subset_convention_*`) pin both formulas, including the exact
cache-exceeds-input shape the live run produced, so this regression cannot
silently return.

**This is itself the honest headline finding t7 exists to surface**: the
comparison logic's first version was measuring the wrong thing, and only a
real live call against a real backend caught it. A fixture-only run would
not have — the fixtures were written by the same (wrong) assumption.

## What was measured live

Two independent runs, `developer` claude-code bridge (`spark`,
`http://127.0.0.1:8088`), same 6-task instruction set both times
(`scripts/stickiness_ab_tasks.example.json` — six trivial,
file-untouching, single-word-reply prompts, deliberately identical in
shape between cold and warm so the comparison measures caching, not prompt
length). `--cache-convention additive` throughout (the only convention this
artifact confirms live).

### Run 1 — `run_id 2a01e9dd-eb24-4a80-9829-d2492156c9a1`

| column | cold | warm |
| --- | --- | --- |
| tasks run (n) | 6 | 6 |
| tasks with cache telemetry (measured_n) | 6 | 6 |
| failures | 0 | 0 |
| avg uncached input tokens/task | 2.00 | 2.00 |
| total cached input tokens | 114,582 | 221,496 |
| distinct provider sessions (thread_id) | 6 | 1 |
| tasks actually resumed | 0 | 5 |
| total wall time (s) | 22.42 | 17.61 |
| avg cost/task (USD, bonus) | 0.1871 | 0.0198 |

Reduction (uncached input): **0.0%** — verdict `no_reduction_or_regression`.
Cost dropped ~89% (bonus metric, not the gate's own signal — see below).

### Run 2 — `run_id 6742a2f6-d106-4574-ab83-48b1f98354e9` (harness now cost-aware)

| column | cold | warm |
| --- | --- | --- |
| tasks run (n) | 6 | 6 |
| tasks with cache telemetry (measured_n) | 6 | 6 |
| failures | 0 | 0 |
| avg uncached input tokens/task | 2.00 | 2.00 |
| total cached input tokens | 220,506 | 224,314 |
| distinct provider sessions (thread_id) | 6 | 1 |
| tasks actually resumed | 0 | 5 |
| total wall time (s) | 22.24 | 16.85 |
| avg cost/task (USD, bonus) | 0.0194 | 0.0223 |

Reduction (uncached input): **0.0%** — verdict `no_reduction_or_regression`.
Cost went *up* ~15% for the resumed arm (bonus metric).

**N is 6 per arm, not the plan's imagined 10** — a deliberate, stated
deviation: the operator brief redirected this task mid-flight once live
bridges became reachable, explicitly on cost grounds ("the two arms are NOT
equally expensive... the affordable design is small tasks and a modest
N... six tiny prompts per arm is a real measurement; ten substantial ones
is not"). `--min-n 6` was passed explicitly for both live runs to match the
N actually used; the harness's own *default* `min_n` stays 10, matching the
plan's original language, for a future larger run.

**Total real spend across every live call made for this task** (two smoke
tests plus both 12-task runs, 27 real invocations counted from the
bridge's own flight-feed usage records): **$1.91**.

### Independent confirmation that resume genuinely happened

Per the operator brief's caution, resume was not inferred from the token
numbers being measured. Two independent, harness-external checks:

1. **`usage.thread_id` collapsed to one value across the warm arm** (1
   distinct session vs. 6 for cold) — this comes from claude's own reported
   `session_id`, not from anything this harness's request construction
   claims about itself.
2. **Direct inspection of the bridge's flight-feed files**
   (`~/.local/state/culture-nodes-bridges/developer/flight/*.jsonl`, a
   read-only, out-of-band spot check against the bridge's own runtime
   state — not something the harness relies on generically, since it is
   claude-code-specific) confirmed the SAME `session_id`
   (`af8788f3-dfb1-4f05-89c2-d56c1f133431`) on 5 consecutive result records
   in run 1, with `cache_creation_input_tokens` dropping from 17,654 (that
   session's first turn) to 99 (every turn after) while
   `cache_read_input_tokens` climbed turn over turn (36,751 → 36,850 →
   36,949 → 37,048 → 37,147) — the exact fingerprint of a conversation
   genuinely growing on one warm, resumed thread.

## The `cache_creation_input_tokens` gap

The flight-feed spot check above surfaces a real, previously invisible
finding: Anthropic's `usage` block reports **three** input-token
categories — `input_tokens` (genuinely fresh), `cache_read_input_tokens`
(cheap, served from cache), and `cache_creation_input_tokens` (expensive,
*writing* a new cache entry). ADR 0009 (task t1) added `cached_input_tokens`
to the wire protocol, mapped from claude's `cache_read_input_tokens` —
`claude_code_bridge/mapping.py::usage_from_result` explicitly documents
choosing not to add `cache_creation_input_tokens`, reasoning that doing so
"would overstate the cache ratio this field exists to measure." That
reasoning holds for the *ratio* field, but it means `cache_creation_input_
tokens` is **dropped on the floor entirely** — never reported anywhere in
the wire protocol, the database, or this harness.

Run 1's flight-feed data shows why that matters: a cold session paid
17,654 `cache_creation_input_tokens` (a real, billed cost — cache writes
carry a premium) every single time, while the resumed session paid only 99
after its first turn. That gap is almost certainly the dominant real
economic effect of session resume for the fleet's own long-CLAUDE.md,
large-repo-context workloads — and it is **invisible to `uncached_input_
tokens`**, because `input_tokens` never included it in the first place.
Run 2, run minutes later against a fleet still warm from run 1, shows the
same gap essentially closed (cold sessions had nothing new to write either,
having piggybacked on the still-live ~1-hour ephemeral cache run 1 left
behind) — which is exactly why the bonus cost column swung from a ~89%
drop to a ~15% rise between the two runs on the identical task set and
identical code path. **This is provider cache-TTL sensitivity made
concrete** — precisely the failure mode spec risk s26 named ("a resumed
long-history session can cost more than a cold start") — except here it
runs the other way: it is the *cold* arm's cost that is unstable, entirely
dependent on ambient cache state left over from unrelated recent traffic on
the same fleet.

This is recorded here as a finding for a follow-up task, not fixed by t7:
closing it means adding a wire-protocol field
(`internal/actors/protocol.go`, `engine.Usage`, a new migration column) and
a bridge-side mapping change (`adapters/claude-code/src/claude_code_bridge/
mapping.py`) — both out of t7's scope (the engine/protocol change belongs
with a future ADR in the ADR-0009 lineage; the bridge change collides with
task t25's concurrent edits to all three bridges' `src/`). Recorded for
`t33`'s delivery verification and as a candidate follow-up task.

## Fixture-driven validation

`tests/test_stickiness_ab.py` — 31 tests, all passing, none requiring
network access:

- Pure arithmetic pins for `uncached_input_tokens` under both
  `cache_convention` values, including the exact cache-exceeds-input shape
  the live run produced (`test_additive_convention_handles_cached_
  exceeding_input`) and a regression pin showing what the old (wrong)
  formula would have produced on that same data
  (`test_subset_convention_can_go_negative_which_is_exactly_the_original_
  bug`).
- `summarize`'s failure/measured-count bookkeeping, distinct-session
  counting, and resumed-count logic, each pinned independently.
- `test_summarize_applies_the_identical_formula_to_both_arm_labels` —
  relabels one record set from `cold` to `warm` and asserts every summary
  field but the label matches, proving there is no per-arm branch to bias.
- `compare`'s full decision table: insufficient-data (empty arm, no cache
  telemetry, zero baseline), no-reduction/regression, underpowered-positive
  (below `min_n` and below the reduction threshold, as two separate cases),
  and reduction-demonstrated — under both `cache_convention` values.
- One integration test drives `BridgeClient` → `run_cold_arm`/
  `run_warm_arm` → `summarize` → `compare` over real loopback HTTP against
  a fake bridge (reusing `tests/fake_api.FakeNodesAPI`, the pattern this
  repo's CLI test suite already uses) that speaks the exact wire shape the
  real bridges promise, including asserting `continuation_ref` rides
  top-level per ADR 0010 §1 and never leaks into `input`. A second
  integration test confirms a failed task does not abort the rest of an
  arm and is excluded from the average without corrupting the failure
  count.

None of this substitutes for the live run above — it proves the arithmetic
that turns real numbers into a verdict is correct, which is a precondition
for trusting the live numbers, not a replacement for collecting them.

## Where the gate was left, and why

**Stickiness stays opt-in.** Two independent live runs against the real
deployed fleet, on the exact metric the plan's honesty condition h2 names
(uncached input tokens), showed **0.0% reduction, twice**. That is not
"insufficient data" — it is a real, repeated, live measurement that the
gate's own stated criterion was not met.

The bonus cost signal is genuinely interesting (an ~89% drop in run 1) but
is explicitly **not** treated as counter-evidence for two reasons: (1) it
is not the metric h2 names, and (2) it did not replicate — run 2, same
code, same tasks, minutes later, showed a *rise*. A signal that inverts
between two runs of an N=6 sample is exactly what "underpowered" looks
like, and the mechanism turned out to be explainable by ambient cache-TTL
state rather than by resume itself (see "The `cache_creation_input_tokens`
gap"). Recommending a default flip on that basis would be exactly the
"overstating a thin measurement" failure mode this task was warned against.

**No code was changed to keep the default off**, because none needed to
be: a repo-wide search (`grep -rn "session_key" internal/ examples/
schemas/`) turns up zero occurrences outside the bridges' own test suites —
nothing in the control plane populates `input.session_key` for a
cross-run/cross-workstream resume today. The within-run, same-actor
continuation reuse task t4 shipped (`internal/worker/dispatch.go`'s
`priorContinuationRef`) is unconditional but deliberately narrow-scoped
(ADR 0010 §4: "not `session_key`... a cross-run lookup would resume a
conversation nothing declared it wanted resumed") and is explicitly *not*
what ADR 0010's own consequences section calls "stickiness" ("Stickiness
itself remains gated on c42's A/B artifact (task t7). This ADR makes the
ref *carriable*; it does not make resuming a default."). The broader,
workstream-level stickiness this gate is actually about has no default-on
code path to begin with — this artifact's job was to confirm that absence
should not change, and it should not. `internal/worker/dispatch.go` and
`adapters/*/src` were not touched, per this task's repo rules (t25 is
concurrently editing all three bridges) and because there was nothing to
flip.

A future task that wires cross-run `session_key` population must ship it
config-gated, defaulting off, and clear this artifact's bar — or a re-run
of it at real N — before flipping that default. Given the
`cache_creation_input_tokens` gap above, the more useful next A/B may
target the *dollar-cost* metric once that field exists on the wire, rather
than re-running this exact `uncached_input_tokens` comparison at higher N:
the live evidence here suggests `uncached_input_tokens` may simply be the
wrong proxy for what stickiness actually saves.

## How to re-run this

```bash
# Against any bridge speaking the actor protocol (fixture or real):
python3 scripts/stickiness_ab.py \
    --base-url http://127.0.0.1:8088 \
    --auth-token "$CLAUDE_CODE_BRIDGE_AUTH_TOKEN" \
    --repo /path/to/allowlisted/repo \
    --tasks scripts/stickiness_ab_tasks.example.json \
    --min-n 6 \
    --cache-convention additive \
    --out docs/deliveries/DATE-t7-stickiness-ab-rerun.md
```

`--cache-convention` has no silently-safe default choice baked into the
gate's own reasoning above — pass `additive` for claude-code (confirmed
live by this artifact) or `subset` for a first live codex run (unconfirmed
by this artifact; codex's own convention is a stated open question, not an
assumption). See `scripts/stickiness_ab.py`'s module docstring for the full
mechanics (sync-or-async following, the honesty rules the comparison
enforces, and why raw `AttemptRecord`s never bake a convention in).

## Gates run

```text
uv run pytest -n auto -q                                           → 199 passed (baseline 168 + 31 new,
                                                                        tests/test_stickiness_ab.py)
uv run black --check culture_nodes tests                           → pass (38 files unchanged)
uv run isort --check-only culture_nodes tests                      → pass
uv run flake8 culture_nodes tests                                  → pass
go vet ./...                                                        → clean
go test -p 1 ./...                                                  → ok, every package (no Go source
                                                                        touched by this task; re-run
                                                                        anyway per the task's gate list)
gofmt -l .                                                          → no files listed (clean)
markdownlint-cli2 "docs/deliveries/2026-08-14-t7-stickiness-ab.md" → 0 errors
```

(Bonus, not in the task's mandated gate list but run anyway on the new
file: `uv run bandit -c pyproject.toml -r scripts/stickiness_ab.py` → 0
issues, one `# nosec B310` on the harness's own `urlopen` call — the same
false-positive pattern `adapters/*/callbacks.py` already comments
identically, since the URL is an operator-supplied `--base-url`, not
untrusted input.)

No files under `adapters/*/src`, `internal/worker/dispatch.go`, `web/`, or
`deploy/` were touched. No SQL migration was added. No version bump.
`scripts/stickiness_ab.py` and `scripts/stickiness_ab_tasks.example.json`
are new, reusable operator tooling; `tests/test_stickiness_ab.py` and this
artifact are the rest of the diff.
