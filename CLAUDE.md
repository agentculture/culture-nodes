# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

**culture-nodes** is a workflow front and framework for Culture: it composes
mesh agents and their verbs into node-based workflows — define a graph of
nodes, run it across the Culture mesh, and inspect what each node did.

The repo currently has two layers, and knowing which one you are touching
matters:

1. **The product design** lives in `docs/initial-design/` and is not yet
   implemented:
   - `culture-nodes-prd-spec.md` — the full PRD for **Culture Nodes**, a
     durable, ledger-native workflow orchestrator for agents, code, services,
     and people. Brand line: *"Every node has a contract. Every result has
     evidence."*
   - `culture-nodes-implementation-issue.md` — the Phase 0/1 implementation
     issue (contracts + compiler, then a durable vertical slice).
2. **The mesh-agent scaffold** is the code that exists today — a Python 3.12
   package (`culture_nodes/`) scaffolded from `culture-agent-template`,
   carrying an agent-first CLI, a mesh identity, and the vendored skill kit.
   The self-description strings this note used to flag (`learn`, the
   argparse description, `explain`'s root catalog entry) were updated to
   describe the shipped product rather than the template it was scaffolded
   from — first partly during the Phase-0/1 build, and the remaining
   drift (`overview`'s command/catalog text, `whoami`'s module docstring)
   during task t24 of the phase-2 cycle. If a self-description string
   anywhere in `culture_nodes/` still reads like generic clonable-template
   prose, that is new drift worth fixing, not this note's old one.

## Design ground rules (from docs/initial-design/)

When implementing product features, the PRD is the blueprint. The
load-bearing decisions to honor (see the PRD for full detail):

- **Vocabulary**: workflow (immutable versioned graph), node, edge, actor,
  run, token, node run, attempt, contract, work ledger, evidence. Actor
  identity, graph token, node run, and attempt are *separate* records.
- **Ledger authority model**: agents may only create `proposed` records;
  humans `confirm`/`reject`; trusted runners create `observed` evidence only
  for facts they directly measured; deterministic validators create `derived`.
  No actor promotes its own proposal. An agent saying "done" is a completion
  claim, not verified evidence. Records are immutable; corrections append
  with `supersedes`.
- **Domain outcome ≠ technical status**: `changes_required` is a domain
  outcome that follows a graph edge, never an engine failure.
- **Contracts**: JSON Schema Draft 2020-12; conditions in CEL; JSON is
  canonical (YAML is authoring sugar); definitions are content-addressed by
  SHA-256 digest and runs pin an immutable digest.
- **Runtime**: PostgreSQL is authoritative; SQS (later) is a disposable work
  signal only; leases + fencing tokens; transactional outbox. Code executes
  only through the headspace-cli runner boundary — never a shell, script, or
  Docker socket in the control-plane process.
- The PRD targets a **Go control plane + React/Vite UI**; today's package is
  the Python mesh-agent surface. The PRD's §26 lists open Phase-0 questions
  (e.g. whether the CLI stays standalone `nodes`). Record deviations from the
  PRD explicitly (ADR / devague deviation record), don't drift silently.

The devague skill chain vendored here (`/scope` → `/think` → `/challenge` →
`/spec-to-plan` → `/assign-to-workforce`, with `/deviate` and
`/summarize-delivery` around execution) is the intended lane for turning this
design into confirmed, planned work.

## Mesh identity

`culture.yaml` declares the agent: `suffix: culture-nodes`,
`backend: colleague`, pinned Qwen model. The colleague resident's prompt file
is `AGENTS.colleague.md` — **not** this file; a colleague-backend agent never
loads CLAUDE.md, so mesh-agent behavior rules belong there. `nodes doctor`
enforces the backend → prompt-file mapping (`claude` → `CLAUDE.md`,
`colleague` → `AGENTS.colleague.md`, `acp` → `AGENTS.md`, `gemini` →
`GEMINI.md`) plus prompt-file-present and skills-present.

## Commands

```bash
uv sync                                   # install (deps + dev group)
uv run pytest -n auto                     # full test suite (xdist)
uv run pytest tests/test_cli.py::test_whoami_text   # single test
uv run pytest -n auto --cov=culture_nodes --cov-report=term  # with coverage (CI gates ≥60%)

# Lint — mirrors the ROOT CI lint job exactly:
uv run black --check culture_nodes tests
uv run isort --check-only culture_nodes tests
uv run flake8 culture_nodes tests
uv run bandit -c pyproject.toml -r culture_nodes
markdownlint-cli2 "**/*.md" "#node_modules" "#.local" "#.claude/skills" "#.teken"
uv run teken cli doctor . --strict        # agent-first CLI rubric gate

# …and the FIVE adapter lint jobs, which the root scope does NOT cover.
# Each bridge lints `src tests` in its own workflow. Skipping this is how
# PR #122 went red on three `lint` jobs after a fully green local run:
for a in claude-code codex colleague human-inbox notify; do
  (cd adapters/$a && uv run black --check src tests && uv run isort --check-only src tests)
done
```

**A shared bridge module must stay byte-identical across all five adapters**
(`tests/lint/` enforces it for `preflight.py`, `dialin.py`, `deployment.py`,
and the workspace reaper). Formatting them per-adapter *breaks* that: isort is
configured in three adapters and not the other two, so the same file acquires
two different formattings. Format one, copy it to the rest, then re-run the
loop above — do not run the formatter independently in each.

The installed CLI command is **`nodes`** (`[project.scripts]` in
pyproject.toml), even though help text renders the prog as `culture-nodes`:

```bash
uv run nodes whoami        # identity from culture.yaml
uv run nodes learn         # self-teaching prompt
uv run nodes explain <path> # markdown docs for any noun/verb
uv run nodes overview      # descriptive snapshot
uv run nodes doctor        # agent-identity invariants
uv run nodes cli overview  # describe the CLI surface
```

## CLI architecture

The runtime package has **zero third-party dependencies** (`dependencies = []`);
`teken` and the test/lint stack are dev-group only. Keep it that way — the
CLI is cited from teken's `python-cli` reference and must keep passing
`teken cli doctor . --strict` (the seven-bundle agent-first rubric, gated in CI).

- `culture_nodes/cli/__init__.py` — entry point. `_CliArgumentParser`
  overrides argparse's `.error()` so even parse-time errors emit the
  structured `error:` / `hint:` format; because parse errors happen before
  `args.json` exists, `main()` pre-scans raw argv for `--json` and stashes it
  on the class (`_json_hint`). `_dispatch()` wraps any non-`CliError`
  exception so no traceback ever reaches stderr.
- `culture_nodes/cli/_commands/*.py` — one module per verb/noun group, each
  exposing `register(subparsers)`. New noun groups follow the same pattern
  and are registered in `_build_parser()`.
- `culture_nodes/cli/_output.py` — the stable contract: **results to stdout,
  errors/diagnostics to stderr, never mixed**; `--json` on every command.
  Text errors render as `error: <msg>` + `hint: <remediation>` (the `hint:`
  line is rubric-required).
- `culture_nodes/cli/_errors.py` — `CliError` with the exit-code policy:
  `0` success, `1` user error, `2` environment error, `3+` reserved.
- `culture_nodes/explain/catalog.py` — verbatim-markdown catalog keyed by
  command-path tuples; `explain` and `overview` read from it. A new verb also
  needs a catalog entry (tests introspect `known_paths()`).

## CI and PR workflow

- **Every PR bumps the version** — even docs/config-only changes. Use
  `/version-bump patch|minor|major` (updates `pyproject.toml` + prepends a
  CHANGELOG entry); the `version-check` job blocks merge otherwise.
- `tests.yml`: pytest + coverage → SonarCloud scan with
  `sonar.qualitygate.wait=true` (a red gate fails CI; skipped when
  `SONAR_TOKEN` is absent, e.g. fork PRs); lint job (black, isort, flake8,
  bandit, markdownlint, teken rubric); version-check.
- `publish.yml`: same-repo PRs touching `pyproject.toml` or `culture_nodes/**`
  publish a `.devN` build to TestPyPI; **a push to main that touches those
  paths publishes to PyPI** (Trusted Publishing, no tokens).
- Use the `cicd` skill for the PR lane (delegates to `devex pr`, adds
  SonarCloud `status`/`await`); `sonarclaude` for ad-hoc quality-gate queries.
  PR replies via the cicd scripts auto-sign as `- culture-nodes (Claude)`.

## Skills are vendored — cite, don't import

`.claude/skills/` is vendored from guildmaster (and, as tracked divergences,
directly from devague, colleague, and eidetic-cli). **Never edit vendored
script bodies.** Provenance, the allowed identifier-only adaptations, and the
re-sync procedure live in `docs/skill-sources.md`. The `type: command`
frontmatter on every `SKILL.md` is load-bearing (the culture skill loader
skips files without it). Tooling prerequisites: `devex` (>=0.21) and `agtag`
(>=0.1) on PATH; `colleague` optional (only `ask-colleague` needs it).

The first-party `nodes-operator` skill (authored here, not vendored) carries
culture-nodes-specific split-plan guidance on work-package node sizing,
per-wave session declaration, and codex-first routing for large packages
(see the "Split-plan lane guidance" section in `.claude/skills/nodes-operator/SKILL.md`).
When using the vendored `assign-to-workforce` skill, apply the split-plan
template from `nodes-operator` to declare expected model-session counts per
wave against the remaining subscription window.

## Conventions and workflow

- **Nodes dogfooding reflex**: when a scoped task is delegable, assign it
  through the system instead of doing it in-session — invoke the
  `/nodes-operator` skill and run its `assign <actor> "instruction" --yes`
  verb with exactly one registered actor (today: `codex-thor` or
  `codex-orin`; billable — confirm intent per the skill's guard). Two goals, both deliberate: the product
  exercises itself on real work, and every assigned run grows the
  comparative record of **which actor is better at what**. Codex sessions on
  thor/orin **can exec shell commands** as of the userns fix (#63:
  `kernel.apparmor_restrict_unprivileged_userns=0`, applied and persisted on
  all three hosts) — verified live by run `01M00AM5NME6TZ1PXDG4A454HE`, which
  ran `git log`, `git status`, `pwd` and `ls` and got real output. The **write
  path is not yet proven**: that probe was read-only, and #18 stays open until
  a `workspace-write` dispatch actually lands a patch. Until then, route
  analysis, reviews and investigations to codex freely, and treat write work
  there as unproven rather than impossible.
  The assessment half is not optional: after every assigned run, read
  `run <id>` + `ledger <id>`, weigh the proposed claims (completion claims,
  not evidence — §10.4), decide them through the approval surface where one
  is reviewable, and `/remember` a short actor-quality note (actor, task
  kind, verdict, why) so the comparison survives the session. First-class
  grading records and per-actor analytics are tracked in issue #28 — prefer
  those surfaces over ad-hoc notes once they exist.
- **A failing merge gate is routed, not carried** (task t32, issue #102): a
  rejecting suite verdict makes the control plane compose a `derived` routing
  record — a bounded repair attempt on a lane whose advertised capability
  surface shows it can actually run the failing suite, or a human node. The
  bound is **2 repair attempts per run over a 24-hour window from the run's
  first gate rejection**, and both ceilings reach a human (`internal/repair`).
  Nothing is dispatched: the control plane decides and records, and executing
  the routed dispatch stays a deliberate step, because the bridge write path is
  unproven (#18) and an advertised surface cannot show a database-backed suite
  is runnable on a lane (#119). Read a red gate's routing with
  `scripts/collect-handover.py <run-id> --gate ...`; declare what a repair
  would need with `--requires-grant` / `--implicates`. A failure implicating
  `.github/` always goes to a person — a repair is a dispatch, and a dispatch
  may not modify CI configuration.
- **Ask what revision is running before trusting a probe** (task t32, issues
  #104 / #120): `curl -s $NODES_API_URL/v1alpha1/version` for the control
  plane (unauthenticated), and the `deployment` block on a bridge's
  `/v1/capabilities` for a bridge. Read `install_mode` first — the codex and
  notify bridges are `uv tool install`ed **copies** that go stale silently
  until redeployed, while spark's claude bridges are **editable** installs that
  cannot go stale but can be serving uncommitted code (`revision_is_dirty`).
  `deploy/prod/deploy.sh` stamps the copies before installing them; a bridge
  reporting no revision was deployed by something that does not stamp, and its
  age is unknown.
- **Memory discipline** (eidetic): `/recall` before non-trivial work to build
  on prior decisions; `/remember` when a non-obvious decision, constraint, or
  hard-won fix surfaces. This repo's memory is **in-repo and public** — a
  plain `/remember` resolves scope from `culture.yaml` and lands the record
  in `<repo-root>/.eidetic/memory` (committed, mesh-shared); pass
  `--visibility private` to keep a record in `$HOME` instead.
- **Ask-colleague reflex**: for a non-trivial committed diff, run
  `ask-colleague review` before opening the PR; for a fresh read of an
  unfamiliar area, `ask-colleague explore`. Both are read-only in a throwaway
  worktree and always safe; side-effecting `write --apply` / `--pr` needs the
  user's go-ahead.
- **Worktrees**: hand-created worktrees (workforce lanes, scratch checkouts)
  live in `../.worktrees.culture-nodes/<name>/` — one repo-named directory
  beside the checkout, never a shared `../worktrees/`. Scope branch prefixes
  to the work (plain `agent/*` collides with leftovers from earlier
  fan-outs). Tear down with `git worktree remove <path>` (`prune` only clears
  metadata for already-deleted dirs). The vendored `assign-to-workforce`
  example uses the shared path and `agent/<task-id>` branches — it is cited
  verbatim and must not be edited; override both when following it.
- **Split-plan lane and session accounting** (economy lane, issue #48): The
  operator's interactive Claude session, local subagents, and all bridge
  sessions (headless claude -p, codex exec, colleague work) on the same
  account share ONE subscription session window — not independent capacity
  pools. Before any fan-out, the split plan declares expected model-session
  count per wave against the remaining window (windows reset on a fixed
  clock). Work-package node sizing: model nodes amortize bootstrap into one
  persistent warm session with many ledgered sub-actions, never one cold
  session per small plan task; deterministic/code nodes stay microscopic.
  Routing: big analysis/build packages default to codex actors (full cold-session
  tax justified only by ledger attribution, isolation, or cross-machine
  execution); the operator's Claude window is reserved for operator-lane work
  and merge gates. Ground these declarations in the lane-cost ladder
  observations: operator main loop (prompt-cache warm, lowest marginal cost),
  local subagents (cold-ish start but same-process context), bridge sessions
  (full cold tax plus engine overhead). See issue #48 comment items 2–3 and
  the attempted-evidence-humans-loops deviation record (issue #47/#48) for the
  grounding economics.
