# internal/devague testdata

`show.json`, `waves.json`, and `deliverables.json` are genuine `devague`
CLI output (devague 0.22.0), not hand-written. They were captured by running
the exact command sequence below in a scratch directory **outside any git
repository** — devague persists frame/plan state cwd-relative, under
`.devague/`, so it must never be run from inside a repo whose own
`.devague/` you do not want touched.

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
