# internal/devague testdata

`show.json`, `waves.json`, `deliverables.json`, `plan-show.json`, and
`deviations.json` are genuine `devague` CLI output (devague 0.22.0), not
hand-written. They were captured by running the exact command sequences
below in a scratch directory **outside any git repository** — devague
persists frame/plan state cwd-relative, under `.devague/`, so it must never
be run from inside a repo whose own `.devague/` you do not want touched.

`plan-show.json` and `deviations.json` (task t22) are a SEPARATE fixture
pair from a separate frame/plan (`t22fixture`, not `fixture`) — see
"Regeneration: plan-show.json and deviations.json" below. They exist
because `plan-show.json` needs a dependency/status shape `waves.json` cannot
exercise: a task depending on only ONE of two same-wave siblings, which is
exactly what distinguishes MapPlanShow's real per-task edges from
MapPlanWaves' "depends on the whole previous wave" approximation (see
plan_show.go's doc comment and plan_show_test.go's
`TestMapPlanShowDoesNotDegradeToTheWavesApproximation`).

## Regeneration

```bash
mkdir -p /tmp/devague-fixture && cd /tmp/devague-fixture

# --- Frame: announcement + the required convergence claims ---------------
devague new "Fixture feature ships a working demo" --title fixture --json
devague capture "The people who will use the fixture" --kind audience --origin user --json
devague capture "Fixture reaches a stable demo state" --kind after_state --origin user --json
devague capture "Fixture demo boundary: no production data is touched" --kind boundary --origin llm --json
devague capture "Fixture converges in under 3 seconds" --kind success_signal --origin user --json
devague capture "We will keep the fixture frame tiny on purpose" --kind decision --origin user --json
devague capture "The devague binary on PATH matches the tested version" --kind assumption --origin llm --json
devague capture "Without this fixture the mapping code has nothing real to round-trip against" --kind why_it_matters --origin user --json

# Honesty conditions. NOTE: devague's `interrogate` flips a CONFIRMED claim
# back to "proposed" when it changes the claim's own --instruction in the
# same call — a user capture auto-confirms, so adding --honesty *and*
# --instruction together on an already-auto-confirmed claim (c1/c2/c3/c5)
# demotes it. Re-confirm after interrogating, or split the two flags across
# separate calls. This fixture takes the "re-confirm after" path below.
devague interrogate c1 --honesty "The announcement text matches what was captured" --instruction "Read c1's text back and compare to the capture command" --origin user --json
devague interrogate c2 --honesty "The audience text names a real reader" --instruction "Read c2's text" --origin user --json
devague interrogate c3 --honesty "The after_state text is a checkable end condition" --instruction "Read c3's text" --origin user --json
devague interrogate c4 --honesty "The boundary is testable by inspection" --instruction "Confirm no production data appears in fixture files" --origin llm --json
devague interrogate c5 --honesty "3 seconds is measured from devague new to devague converge" --instruction "Time the fixture generation script" --origin user --json
devague interrogate c8 --honesty "The reason names the fixture's actual purpose" --origin user --json

devague confirm c1 c2 c3 c5 --json   # re-confirm after the instruction flip above
devague confirm h4 c4 --json         # c4 is llm-origin: both the honesty condition and the claim need an explicit user confirm

devague converge --json              # ready_for_spec: true

# --- Plan -------------------------------------------------------------
devague plan new --frame fixture --title "Fixture plan" --json

devague plan task "Build the frame fixture (announcement/audience/after_state)" \
  --covers c1 --covers h1 --covers c2 --covers h2 --covers c3 --covers h3 \
  --accept "Frame converges via devague converge --json" \
  --origin llm --instruction "Run the capture+interrogate+confirm sequence recorded in the fixtures README" --json

devague plan task "Round out convergence (boundary/success_signal/why_it_matters)" \
  --covers c4 --covers h4 --covers c5 --covers h5 --covers c8 --covers h6 \
  --dep t1 \
  --accept "Frame converges via devague converge --json" \
  --accept "devague plan new succeeds against the converged frame" \
  --origin llm --instruction "Add the remaining claims and honesty conditions, then confirm them" --json

devague plan confirm t1 t2 --json
devague plan converge --json         # ready_for_plan: true

# --- Fixture files -------------------------------------------------------
devague show --json > show.json
devague plan waves --json > waves.json
devague plan deliverables --json > deliverables.json
```

Copy the three `.json` files into this directory (overwriting the committed
ones) if you regenerate them.

## Regeneration: plan-show.json and deviations.json

```bash
mkdir -p /tmp/devague-t22-fixture && cd /tmp/devague-t22-fixture

# --- Frame: the same minimum convergence shape as the "fixture" frame above,
# under a different slug so the two fixture pairs never collide -------------
devague new "Fixture t22 plan-show ships real dependency edges" --title t22fixture --json
devague capture "The people who will use the fixture" --kind audience --origin user --json
devague capture "Fixture reaches a stable demo state" --kind after_state --origin user --json
devague capture "Fixture demo boundary: no production data is touched" --kind boundary --origin llm --json
devague capture "Fixture converges in under 3 seconds" --kind success_signal --origin user --json
devague capture "We will keep the fixture frame tiny on purpose" --kind decision --origin user --json
devague capture "The devague binary on PATH matches the tested version" --kind assumption --origin llm --json
devague capture "Without this fixture the mapping code has nothing real to round-trip against" --kind why_it_matters --origin user --json

devague interrogate c1 --honesty "The announcement text matches what was captured" --instruction "Read c1's text back and compare to the capture command" --origin user --json
devague interrogate c2 --honesty "The audience text names a real reader" --instruction "Read c2's text" --origin user --json
devague interrogate c3 --honesty "The after_state text is a checkable end condition" --instruction "Read c3's text" --origin user --json
devague interrogate c4 --honesty "The boundary is testable by inspection" --instruction "Confirm no production data appears in fixture files" --origin llm --json
devague interrogate c5 --honesty "3 seconds is measured from devague new to devague converge" --instruction "Time the fixture generation script" --origin user --json
devague interrogate c8 --honesty "The reason names the fixture's actual purpose" --origin user --json

devague confirm c1 c2 c3 c5 --json   # re-confirm after the instruction flip
devague confirm h4 c4 --json
devague confirm h6 --json
devague converge --json              # ready_for_spec: true

# --- Plan: five tasks, deliberately shaped so a wave-level approximation of
# depends_on would be WRONG for t3 (its only real dep is t1, but t2 is also
# in t1's wave) -------------------------------------------------------------
devague plan new --frame t22fixture --title "t22 fixture plan" --json

devague plan task "No-dependency setup task" --covers c1 --covers h1 \
  --accept "t1 has no prerequisites" \
  --origin llm --instruction "Bootstrap the fixture" --json

devague plan task "Another independent setup task" --covers c2 --covers h2 \
  --accept "t2 has no prerequisites either" \
  --origin llm --instruction "Bootstrap something else" --json

devague plan task "Depends on t1 only, not on t2" --covers c3 --covers h3 \
  --dep t1 \
  --accept "t3 depends only on t1" \
  --origin llm --instruction "Build on t1's output" --json

devague plan task "Depends on both t1 and t2" --covers c4 --covers h4 \
  --dep t1 --dep t2 \
  --accept "t4 depends on t1 and t2" \
  --origin user --instruction "Combine t1 and t3's output" --json

devague plan task "Scoped out during planning" --covers c5 --covers h5 \
  --accept "t5 never ships" \
  --origin llm --instruction "This task gets rejected" --json

devague plan confirm t1 t3 --json    # t2 stays proposed, t4 auto-confirms (user origin)
devague plan reject t5 --json        # t5 exercises the rejected-status path

devague plan show --json > plan-show.json

# --- Deliveries: one approved (user), one still-proposed (llm), one
# rejected (llm) -- exercising every reviewForDeviationStatus branch --------
devague deviate "Swapped t3's approach after a live capability check" --task t3 \
  --reason "verified against the installed toolchain; the original approach was infeasible" \
  --affects t3 --affects c4 \
  --origin user --classification acceptable --json

devague deviate "Propose folding t4 into t1's scope" --task t4 \
  --reason "found while scoping t4; t1 already produces most of what t4 needs" \
  --affects t4 --affects t1 \
  --origin llm --classification needs-follow-up --json

devague deviate "Considered dropping t2 entirely" --task t2 \
  --reason "explored during planning; ultimately t2's output is still needed" \
  --origin llm --json
devague deviate --reject d3 --json

cp .devague/deliveries/t22fixture.json deviations.json
```

Copy `plan-show.json` and `deviations.json` into this directory (overwriting
the committed ones) if you regenerate them. Unlike the single-line CLI
`--json` stdout captures above, `deviations.json` is copied straight from
the on-disk delivery file (`.devague/deliveries/t22fixture.json`) rather
than from `devague deviate --list --json` — see `deliveryFile`'s doc
comment in `types.go` for why: the CLI list view renames `task_ref` to
`task` and drops `schema_version`/`created`/`updated`, which is not the
shape `.devague/deliveries/*.json` actually has on disk (compare this
repo's own `.devague/deliveries/economy-discord-graphs.json`).

## Why the frame has this exact shape

The convergence gate (`devague converge`) requires confirmed
`announcement`/`audience`/`after_state` claims, a `boundary` claim, a
`success_signal` claim, one of `before_state`/`why_it_matters`, and a
confirmed honesty condition on every confirmed spec-affecting claim. That is
the minimum needed for `devague plan new` to accept the frame at all — the
fixture adds nothing beyond it except `decision` (c6) and `assumption` (c7),
which exist purely so `MapFrameClaims` has a real `decision`/`assumption`
example to map, and which are *not* required for convergence (decision
auto-confirms on a user capture; the assumption is deliberately left
`proposed`, since an unconfirmed assumption is a warning, never a blocker).

Origins are deliberately mixed: c1/c2/c3/c5/c6/c8 are `--origin user`
(devague auto-confirms a user capture); c4 (`boundary`) and c7
(`assumption`) are `--origin llm`. c4 is then explicitly confirmed by the
user — this is the fixture's one llm-origin **confirmed** claim, the case
`internal/devague`'s authority-honesty tests are built around (an
agent-origin claim's own record must never carry confirmed authority
directly). c7 is left `proposed`, giving a llm-origin claim with no
review record at all.

## Known gaps in what devague's `--json` views expose

Verified empirically against this fixture, not assumed from source reading:

- **`devague plan waves --json`** exposes each active task's `summary`,
  `instruction`, `acceptance_criteria`, and `covers` — but **not** devague's
  own per-task `proposed`/`confirmed` status, and **not** explicit
  dependency edges (only the `waves` layering). `devague plan show --json`
  does carry per-task `status` and `deps`, but `MapPlanWaves` is pinned to
  the `plan waves` view specifically; see `plan.go`'s doc comment for how it
  derives `status: "ready"` and `depends_on` from what this view actually
  has.
- **`devague plan deliverables --json`**'s `success_signal` field is a bare
  list of confirmed claim *text*, with no claim id — so `MapDeliverables`
  cannot correlate a signal back to the `MapFrameClaims` record it came
  from, and does not try to (see `deliverables.go`).

Regenerating the fixture with a different frame/plan shape is fine as long
as it still exercises: an announcement, at least one `--origin llm`
confirmed claim, at least one `--origin llm` still-`proposed` claim, a
`decision`, an `assumption`, two plan tasks in two different waves (so
`depends_on` is non-trivial), and at least one confirmed `success_signal`.
