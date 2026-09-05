# Measurement manifests

A *measurement manifest* declares a fixed set of comparison **rules** — one
category out of `locate`, `review`, `explain` per rule, though a manifest may
carry more than one rule per category — run against a fixed list of
**actors**. Each rule carries the instruction every actor receives, how its
answer is mechanically checked, and a 5/3/1 anchor rubric a human (or the
runner, task t11) uses to grade the actual answer against.

This directory holds both halves (plan
`docs/plans/2026-09-05-harness-hardening-and-compare.md`): the **manifest**
— schema, canonical digest, validator, and the basic three-rule set (task
t7) — and the **runner** that dispatches a manifest's rules to real actors,
applies each rule's mechanical check, and posts grades (task t11). They stay
separate modules: `manifest.py` never dispatches to an actor or a bridge,
and `run.py` never re-implements validation or the digest.

## Files

- `schema.json` — the manifest shape, as JSON Schema 2020-12.
- `manifest.py` — loads, validates, canonicalises and digests a manifest.
  Zero third-party dependencies.
- `basic-thor.json` — **the runner's default manifest**: one rule per
  category, `sandbox: read-only`, `runs_per_actor: 2`, against
  `company/pi-thor` and `company/qwen-thor` — the two actors the shipped
  graph can actually reach.
- `basic.json` — the same three rules against all four actors
  (`company/pi-thor`, `company/pi-orin`, `company/qwen-thor`,
  `company/qwen-orin`). It is **not runnable on today's graph** and is not
  the default: the orin actors collapse onto the thor slots and the runner
  refuses the pass (see "One limitation, stated plainly" below, and #304).
  It is kept as the four-actor set to run once the graph has a slot per
  registered actor.
- `tests/fixtures/measurements/basic.yaml` — the same manifest, hand-authored sugar for `basic.json`.
  Present only because this interpreter happens to have PyYAML importable
  (see "JSON is canonical" below); it canonicalises to the exact same
  digest as `basic.json`.
- `run.py` — the runner: the revision gate, serial dispatch, the report and
  the grades. Importable as a module, runnable as a CLI.
- `fleet.py` — the runner's fleet-facing half: the control-plane API client
  (with the operator lane's auth convention), the bridge `/v1/capabilities`
  probe, and actor/slot resolution. Split from `run.py` only to stay inside
  the repo's 1000-line-per-file contract; it is not a separate tool.
- `checks.py` — the three mechanical checks and the 5/3/1 anchor rating.

## JSON is canonical, YAML is authoring sugar

CLAUDE.md's contract rule/decision applies here: JSON is the canonical,
always-supported format. The `culture-nodes` runtime package ships **zero**
third-party dependencies (`pyproject.toml`'s `dependencies = []`), so PyYAML
is not guaranteed to be importable wherever `manifest.py` runs. `manifest.py`
therefore:

- always loads and validates `.json` manifests;
- loads a `.yaml`/`.yml` manifest only if `import yaml` succeeds in the
  running interpreter; if it does not, it exits `2` (environment error) with
  a hint to author the manifest as JSON instead, rather than pretending YAML
  is always available.

If you don't know whether YAML is available in your environment, check
before relying on it:

```bash
uv run python -c "import yaml" && echo "YAML available" || echo "YAML NOT available"
```

## The check kinds

Every rule's `check.kind` says how its actors' answers are mechanically
checked (the runner, t11, implements the actual checking logic — this is
just the contract each kind promises):

- `grep-cites-file-line` — the answer must cite a `path:line`-shaped
  location, and the path must contain `check.expect`. Used for `locate`
  rules: "where in the code is X handled".
- `seeded-defect-named` — the answer must name the specific defect seeded
  into an embedded diff; `check.expect` is a short phrase or token the
  answer must contain (e.g. a value or operator uniquely tied to the seeded
  bug). Used for `review` rules.
- `tests-named` — the answer must name a specific test file; `check.expect`
  is the file the check considers correct. Used for `explain` rules: "what
  proves this code works".

`checks.py` implements exactly these three and refuses any other kind.

A rule's `anchors` (`"5"`, `"3"`, `"1"`) are the human-readable rubric a
grader uses on top of the mechanical check — the mechanical check says
whether the expected fact appears at all; the anchors say how *well* the
answer demonstrates understanding (cited and precise vs. right-but-uncited
vs. wrong/fabricated/absent).

## Command reference

```bash
uv run python examples/harness-compare/measurements/manifest.py validate <file>
uv run python examples/harness-compare/measurements/manifest.py digest   <file>
uv run python examples/harness-compare/measurements/manifest.py canonical <file>
```

- `validate` exits `0` and prints an `ok:` line when the manifest matches
  `schema.json`; otherwise it exits `1`, prints `error: <message>` on
  stderr, and (for a field-specific failure) a `path: <field path>` line
  naming exactly which field failed (e.g. `path: $.rules[2].check.kind`).
  Rule `id` uniqueness across `rules` is enforced too — it is a business
  rule JSON Schema 2020-12 has no keyword for, so `manifest.py` checks it
  by hand alongside the schema.
- `digest` validates, then prints `sha256:<hex>` of the manifest's
  **canonical** JSON form (sorted keys, compact separators, ASCII-safe) —
  stable across source key order and incidental whitespace, and across a
  YAML manifest vs. its JSON twin, but it changes the instant any rule
  field's *value* changes.
- `canonical` validates, then prints that canonical JSON form.
- A `.yaml`/`.yml` manifest with no importable YAML parser exits `2` with a
  hint, from any of the three subcommands, before validation is attempted.

## The runner

`run.py` takes a manifest and turns it into runs, checks and grades. It is
zero-dependency python3 (stdlib `urllib`/`json`/`argparse`/`time`), so it
runs anywhere python3 does — including on a deploy host with nothing
installed.

```bash
export NODES_API_URL=https://nodes.culture.dev
export NODES_OP_COOKIE=...          # Cloudflare Access cookie; never echoed
export MEASURE_RUNNER_ACTOR_ID=...  # the runner's own AGENT actor id

uv run python examples/harness-compare/measurements/run.py \
  --manifest examples/harness-compare/measurements/basic-thor.json \
  --repo-map pi=/home/culture-pi/git/culture-nodes-agent \
  --repo-map qwen=/home/culture-qwen/git/culture-nodes-agent \
  --expect-revision "$(git rev-parse HEAD)" \
  --report docs/audits/measurements.jsonl \
  --yes
```

`--manifest` is spelled out above so the command reads as one artifact, but
that path *is* the default — a bare `run.py` runs the same manifest.

### Environment and flags

| Name | What it is |
| --- | --- |
| `NODES_API_URL` | control plane base URL (`--api-url` overrides). LAN writes have been 401 since 0.47.0 — use the tunnel. |
| `NODES_OP_COOKIE` | Cloudflare Access cookie, sent as `Cookie: CF_Authorization=...` exactly as `nodes-op.sh` sends it. Injected via `grant run --inject`, never pasted on a command line, and never printed by the runner. |
| `NODES_OP_BEARER` | optional bearer, the same hook `nodes-op.sh` carries. |
| `MEASURE_RUNNER_ACTOR_ID` | the grading principal (`--as` overrides). **Required** — see below. |
| `NODES_BRIDGE_TOKEN` | default bearer for a bridge's authenticated `/v1/capabilities` (`--bridge-token slot-or-key=TOKEN` per actor). |
| `NODES_BRIDGE_TOKEN_<SLOT>` | per-slot bearer (`NODES_BRIDGE_TOKEN_PI`, `NODES_BRIDGE_TOKEN_QWEN`, …); wins over the default and keeps the secret off argv. |
| `--repo-map SLOT_OR_KEY=PATH` | each actor's checkout **on its own host**. Required per actor; the runner refuses rather than guessing, because a path is meaningful on exactly one machine. |
| `--expect-revision SHA` | the revision gate below. |
| `--qwen-mode MODE` | ACP session mode for the qwen slot (default `default`); the qwen bridge refuses a dispatch that names none. |
| `--slot ACTOR_KEY=SLOT` | override the actor-key → workflow-slot mapping. |
| `--report PATH` | JSON Lines report, appended to. |
| `--timeout SECONDS` | per-run watch timeout (default 1800); on expiry the run is cancelled, not just abandoned. |
| `--cancel-grace SECONDS` | how long a cancelled run gets to reach a terminal state before the pass stops (default 120). |
| `--gate-only` | read every bridge revision and stop, dispatching nothing. |
| `--yes` | required: the pass dispatches real, billable agent sessions. |

### The revision gate

Before any dispatch the runner reads each actor's bridge
`GET <endpoint_ref>/v1/capabilities` and pulls out the `deployment` block
(`revision`, `install_mode`, `revision_is_dirty`).

- With `--expect-revision <sha>` it **refuses**, naming every actor whose
  revision differs and what that actor is actually running, before a single
  session is spent.
- Without the flag it refuses nothing and simply **records** what it saw —
  into every run's input (`measurement_context.bridge_revisions`) and into
  every grade's notes. A measurement whose build is unknown is still a
  measurement; a measurement that *silently* mixes builds is not.

`--gate-only` runs just this half. Read `install_mode` first: a `copy`
install (the `uv tool install`ed bridges on thor and orin) goes stale
silently until redeployed, while an `editable` one cannot go stale but can
be serving uncommitted code (`revision_is_dirty`).

### It is serial, deliberately

pi, qwen and colleague are served by ONE model on one host. Two concurrent
runs would not measure two actors, they would measure a contended queue —
so the runner creates one run at a time across **all** actors and rules and
waits for it to reach a terminal state before creating the next. There is no
`--parallel` flag and adding one would silently change what every recorded
duration means. `tests/test_measurement_runner.py` pins this: the fake API
records concurrency and asserts the high-water mark is 1.

A watch timeout (`--timeout`, 1800 s) is **not** an exception to that. A run
the runner merely stopped watching is still a run holding the model, so on
timeout it cancels the run (`POST /v1alpha1/runs/{id}/cancel`, the same call
as `nodes-op.sh cancel`) and polls until the control plane reports it
terminal — only then is the next measurement dispatched. The report row
carries both words for what happened: `run_state` is `timed_out`, the
runner's reason, and `settled_state` is what the ledger settled on, normally
`cancelled`. A run that finished in the gap between the last poll and the
cancel keeps its real `completed` state and its answer; rating that 1 would
be a claim about the actor that is not true.

If the control plane will not report the run terminal within
`--cancel-grace` (120 s), the pass **stops** with an environment error
naming the run, rather than dispatching the next measurement over the top of
it. Everything already measured is already in the report — it is appended
per run — so the honest move is to cancel that run by hand and re-run.

The limit of that guarantee, stated plainly: the cancellation is durable in
the ledger before the call answers, and the actor is *asked* to stop its
session, but propagating a cancel to an actor is best-effort by design (PRD
§13.6, `internal/api/cancelpropagate.go`). An actor that ignores the ask is
a protocol limitation, not something this runner can wait out.

### Grades are posted as an agent, never as a human

`nodes-op.sh grade` defaults `--as` to the first registered `kind=human`
actor, and a human grader's grade lands **confirmed** on arrival
(`internal/api/grades.go`). A runner inheriting that default would mint
confirmed grades in bulk for work no human read. So this runner:

- refuses to start with no `MEASURE_RUNNER_ACTOR_ID` / `--as`;
- refuses a principal whose registered `kind` is `human`;
- files each grade against the actor that **actually served** the run
  (`node_runs[].attempts[].actor_id`), flagging a routing mismatch in the
  notes and the report if that is not the actor it addressed.

Every grade it posts therefore lands `proposed` and waits for the ordinary
review surface. Confirming them is a human's move, and the grade notes carry
what that human needs: rule id, manifest digest, check kind, expected fact,
verdict, fabrication flag and bridge revision.

### How an answer is rated

The mechanical check reads the actor's summary text — not its
understanding — and `checks.py` says exactly what each rating means:

- **5** — the expected fact is present **and** the answer points at
  something specific: a `path:line` citation of the expected path
  (the only thing that counts for `grep-cites-file-line`), a `line N` /
  `` `quoted span` `` reference (`seeded-defect-named`), or a named
  `test_*` function (`tests-named`).
- **3** — the expected fact is present but the answer is **uncited** (no
  specific reference at all) or **padded**: it names three or more distinct
  file paths other than the expected one.
- **1** — the expected fact is absent, or the run produced no answer
  (failed, cancelled, timed out).

The **fabrication flag** is a boolean in the notes: true when the answer
cites a file path that does not exist in the checkout the runner can read.
It is best-effort by construction — the actor read its own checkout on its
own host, which the runner cannot see — so it is a signal for the human
deciding the grade, never an input to the rating.

### One limitation, stated plainly

`examples/harness-compare/workflow.yaml` has five **fixed** slots and each
slot's `uses:` is a static registry id — slot `pi` resolves to
`actor://company/pi-thor` and nothing in the run input redirects it. So two
manifest actors that map to the same slot (`company/pi-thor` and
`company/pi-orin`) cannot both be dispatched through this graph, and the
runner **refuses** such a manifest rather than running one host twice and
labelling one of the results the other. Measuring both hosts needs the graph
to gain a slot per host (or a per-host copy of the graph); `--slot` is the
escape only for a deployment that genuinely registered the second host under
another slot's id.

That is why the runner's default manifest is `basic-thor.json` and not
`basic.json`: the four-actor manifest hits this refusal before a single
dispatch, so shipping it as the default would ship a runner whose no-flag
invocation always aborts. `tests/test_measurement_default_manifest.py`
asserts the default's actors resolve to distinct slots that the graph
actually declares, so this cannot silently regress.

## Adding or changing a rule

1. Add or edit a rule object in `basic.json` **and `basic-thor.json`** (or
   in your own manifest file). The two shipped manifests carry the *same*
   rule list and differ only in `actors`, so editing one and not the other
   would silently make the default pass measure a different rule set from
   the four-actor one; `tests/test_measurement_default_manifest.py` holds
   the two rule lists equal. Keep every category's rule count and the
   manifest's other rules intact unless you mean to change them — a
   manifest is a single artifact, not a diff against the previous one.
2. If you added or edited `tests/fixtures/measurements/basic.yaml` too, regenerate it from the JSON so
   the two stay byte-equivalent after canonicalisation (they are not meant
   to be maintained independently by hand):

   ```bash
   uv run python - <<'EOF'
   import json, yaml
   data = json.load(open("examples/harness-compare/measurements/basic.json"))
   with open("examples/harness-compare/measurements/tests/fixtures/measurements/basic.yaml", "w") as f:
       yaml.safe_dump(data, f, sort_keys=False, allow_unicode=True, width=100)
   EOF
   ```

3. Validate, then note the new digest:

   ```bash
   uv run python examples/harness-compare/measurements/manifest.py validate examples/harness-compare/measurements/basic.json
   uv run python examples/harness-compare/measurements/manifest.py digest   examples/harness-compare/measurements/basic.json
   uv run python examples/harness-compare/measurements/manifest.py validate examples/harness-compare/measurements/basic-thor.json
   uv run python examples/harness-compare/measurements/manifest.py digest   examples/harness-compare/measurements/basic-thor.json
   ```

4. Re-run the runner against the new digest:

   ```bash
   uv run python examples/harness-compare/measurements/run.py \
     --manifest examples/harness-compare/measurements/basic-thor.json \
     --repo-map pi=/home/culture-pi/git/culture-nodes-agent \
     --repo-map qwen=/home/culture-qwen/git/culture-nodes-agent \
     --report docs/audits/measurements.jsonl --yes
   ```

   **Re-running never edits an old run, grade or report line** — the ledger
   is append-only (CLAUDE.md's ledger authority model: records are
   immutable; corrections append with `supersedes`), and the report is JSON
   Lines appended to for exactly the same reason. Changing a rule and
   re-running produces new runs and new grades pinned to the new digest,
   alongside — never instead of — whatever the previous digest's runs and
   grades already recorded. If you want to compare "before" and "after" a
   rule edit, keep both digests' runs around; don't delete the old ones, and
   don't rewrite the report: `manifest_digest` on each row is what tells
   the two passes apart.

5. Check the new rule's expected fact is actually reachable before spending
   a pass on it. `--gate-only` costs nothing and tells you which build every
   bridge is on; a rule whose `expect` names a file that moved in the
   meantime will rate every actor 1 and prove nothing about the actors.

## What these modules do not do

- `manifest.py` does not dispatch to any actor or bridge, and does not know
  what a "run" or a "grade" is in the ledger sense — that is `run.py`.
- `run.py` does not confirm anything. Its grades are `proposed` claims from
  an automated reader; deciding them is a human's move through the review
  surface, and a 5 from the mechanical check is not evidence that the actor
  understood the code.
- The fabrication flag does not prove fabrication; see above.
- It does not implement a general JSON Schema validator; `_validate` in
  `manifest.py` implements exactly the keywords `schema.json` uses. A
  schema change that introduces a new keyword needs a matching validator
  change.

## `basic-thor.json` — the default, and the first pass's actor set

`basic.json` names all four thor/orin actors, but `workflow.yaml` pins each
slot to one registry id (`pi` → `company/pi-thor`, `qwen` → `company/qwen-thor`),
so the orin actors cannot be reached through the graph today and the runner
refuses a manifest whose actors collide on one slot. `basic-thor.json` is the
same three rules restricted to the two thor actors; it is what the first
measurement pass ran, and it is what `run.py` runs when no `--manifest` is
given. It carries its own digest, so its runs and grades are
distinguishable from a later four-actor pass once the graph gains a slot per
registered actor.

When #304 lands a slot per registered actor, the move is to point
`DEFAULT_MANIFEST` back at `basic.json` — not to widen `basic-thor.json`,
whose digest is what the first pass's audit rows are pinned to.
