# Measurement manifests

A *measurement manifest* declares a fixed set of comparison **rules** — one
category out of `locate`, `review`, `explain` per rule, though a manifest may
carry more than one rule per category — run against a fixed list of
**actors**. Each rule carries the instruction every actor receives, how its
answer is mechanically checked, and a 5/3/1 anchor rubric a human (or the
runner, task t11) uses to grade the actual answer against.

This directory is the **manifest** half only: schema, canonical digest,
validator, and the basic three-rule set (task t7, plan
`docs/plans/2026-09-05-harness-hardening-and-compare.md`). The **runner**
that dispatches a manifest's rules to real actors, applies each rule's
mechanical check, and posts grades is a separate module (`run.py`, task
t11) — nothing here dispatches to an actor or a bridge.

## Files

- `schema.json` — the manifest shape, as JSON Schema 2020-12.
- `manifest.py` — loads, validates, canonicalises and digests a manifest.
  Zero third-party dependencies.
- `basic.json` — the basic manifest: one rule per category, `sandbox:
  read-only`, `runs_per_actor: 2`, against `company/pi-thor`,
  `company/pi-orin`, `company/qwen-thor`, `company/qwen-orin`.
- `tests/fixtures/measurements/basic.yaml` — the same manifest, hand-authored sugar for `basic.json`.
  Present only because this interpreter happens to have PyYAML importable
  (see "JSON is canonical" below); it canonicalises to the exact same
  digest as `basic.json`.

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

## Adding or changing a rule

1. Add or edit a rule object in `basic.json` (or your own manifest file).
   Keep every category's rule count and the manifest's other rules intact
   unless you mean to change them — a manifest is a single artifact, not a
   diff against the previous one.
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
   ```

4. Re-run the runner (t11) against the new digest. **Re-running never edits
   an old run or grade** — the ledger is append-only (CLAUDE.md's ledger
   authority model: records are immutable; corrections append with
   `supersedes`). Changing a rule and re-running produces new runs and new
   grades pinned to the new digest, alongside — never instead of — whatever
   the previous digest's runs and grades already recorded. If you want to
   compare "before" and "after" a rule edit, keep both digests' runs
   around; don't delete the old ones.

## What this module does not do

- It does not dispatch to any actor or bridge, and it does not know what a
  "run" or a "grade" is in the ledger sense — that is `run.py` (task t11).
- It does not implement a general JSON Schema validator; `_validate` in
  `manifest.py` implements exactly the keywords `schema.json` uses. A
  schema change that introduces a new keyword needs a matching validator
  change.
