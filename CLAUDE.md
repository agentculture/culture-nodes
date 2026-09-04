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
reports **four** checks — `uv run teken cli doctor . --strict` asserts
`checks=4`, so this count is gated, not prose:

1. `prompt_file_present` — the backend → prompt-file mapping (`claude` →
   `CLAUDE.md`, `colleague` → `AGENTS.colleague.md`, `acp` → `AGENTS.md`,
   `gemini` → `GEMINI.md`) *and* that the mapped file is on disk. An
   unrecognised backend fails as `backend_consistency` instead. This is the
   only `error`-severity check, so it alone decides `healthy` and the exit
   code.
2. `skills_present` — the vendored `.claude/skills/` kit exists and is
   non-empty.
3. `nodes_api_reachable` — a `GET /v1alpha1/healthz` probe against the
   resolved API URL.
4. `unprivileged_userns` — reads the AppArmor / userns sysctls to say whether
   a bwrap-backed actor sandbox can start on this host at all. That is the
   fact a codex `--sandbox workspace-write` dispatch needs *before* it
   silently loses every file write while still running shell commands (#63).

Checks 2–4 are `warning` severity: they can fail without flipping `healthy`
or the exit code. Doctor is read-only and probes sysctls directly — it never
shells out to `bwrap`, and it is not what verifies userns on a deploy host.

## Commands

```bash
uv sync                                   # install (deps + dev group)
uv run pytest -n auto                     # full test suite (xdist)
uv run pytest tests/test_cli.py::test_whoami_text   # single test
uv run pytest -n auto --cov=culture_nodes --cov-report=term  # with coverage (CI gates ≥60%)

# Lint — ONE command, and it is the one CI runs (issue #123, widened by #294):
scripts/lint-all.sh                       # all five jobs named `lint`
scripts/lint-all.sh root                  # or one job: root | adapter-codex | adapter-claude-code | adapter-pi | adapter-qwen
scripts/lint-all.sh --list
```

`scripts/lint-all.sh` is not a convenience wrapper around the commands below —
the five `lint` workflows *invoke it*, so a green local run and a red CI lint
job cannot drift apart by construction. That is the same shape as
`scripts/check-zero-runtime-deps.sh`. Before it existed, an operator had to know
that the workflows lint Python in two different styles over
overlapping-but-different paths, and that knowledge lived only here — which is
exactly how PR #122 went red on three `lint` jobs after a fully green local run.

Two root steps need context a laptop may lack — `vendored-skills` (a merge
range) and `triage` (an authenticated `gh`). Neither is skipped silently: an
unrunnable step says so and is named in the summary, and failures accumulate
rather than fail-fast, so the script can only ever report *more* red than CI,
never less.

The difference the script encodes, and the reason it must not be "simplified":
`adapter-codex.yml` runs from the **repo root** against adapter paths (so the
ROOT isort/black config applies), while `adapter-claude-code.yml`,
`adapter-pi.yml` and `adapter-qwen.yml` run **inside the adapter directory** (so
the ADAPTER's own config applies). Running only the adapter-dir form for codex
passes locally and fails in CI.

Scope is deliberately the five jobs literally named `lint`. This widened from
three to five when the sixth adapter (`pi`) and the restored `qwen` adapter each
gained a dedicated workflow (`adapter-pi.yml`, `adapter-qwen.yml`); that is the
recorded frame decision **c33** for #294 — a deliberate widening of #123, not
drift. `go vet` and `web.yml`'s `webglass` are still **not** included: #123's
defect is same-named jobs with different invocations, which those do not have,
and pulling a Go toolchain and an npm install into a script authors run casually
would change what "lint is green" means for every caller. `tests/test_lint_all.py`
pins that exactly five workflow jobs are named `lint`, so widening this further
is a decision, not a drift.

**A shared bridge module must stay byte-identical across all five adapters**
(`tests/lint/` enforces it for `preflight.py`, `dialin.py`, `deployment.py`,
and the workspace reaper). Formatting them per-adapter *breaks* that: isort is
configured in three adapters and not the other two, so the same file acquires
two different formattings. Format one, copy it to the rest, then re-run the
loop above — do not run the formatter independently in each. The claim about
`dialin.py` was aspirational until task t18: three of those four modules were
checked and the transport was not, so `tests/lint/dialintransport_test.go` now
covers it (across **all five** packages, including human-inbox, which ships no
capability surface and is therefore invisible to the guard next door).

**Every adapter is zero-runtime-dependency too, and that is now gated.**
`scripts/check-zero-runtime-deps.sh` (CI lint job) used to load exactly the
root `pyproject.toml`; it now checks the root plus every
`adapters/*/pyproject.toml`, and `tests/test_adapter_zero_dependencies.py`
both drives it (including a negative case that watches it reject a manifest
which gained a dependency) and AST-scans every adapter module for a
third-party import — the manifest is a promise, the import list is the fact.

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

- **Every piece of operator work opens or updates an issue.** If the operator
  did it by hand — a fix typed in-session, a config edit on a host, a deploy, a
  merge conflict resolved, a bridge redeployed — it gets an issue, either a new
  one or a comment on the existing one that covers it. No exceptions for
  "it was only a lint fix."

  The reason is measurement, not bureaucracy. Manual operator work is invisible
  by default: it leaves a commit, and a commit does not say *a human had to do
  this*. That invisibility is how the last cycle ended with **fourteen of
  fourteen** operator steps still manual while the delivery summary could
  otherwise have implied a loop that ran itself
  (`docs/deliveries/close-the-backlog-bootstrap-honesty.md`). An issue per
  hand-turn makes the backlog of un-automated steps countable, and countable is
  the precondition for #118 ever closing.

  Cite the issue in the commit message, so the trail runs both ways.

  **The rule is kept, and the issue type is what stops it reading as
  workload.** Filing every hand-turn is what makes un-automated steps
  countable; it is also why records pile up in the same list as defects, so a
  bare "N open issues" overstates the work outstanding. The type separates the
  two — see below.

- **Every issue declares a type.** The `agentculture` org defines four
  (`gh api graphql -f query='{ organization(login:"agentculture") {
  issueTypes(first:20) { nodes { name } } } }'`):

  | Type | What it is | Closes when |
  |---|---|---|
  | `Bug` | broken now, and the defect still reproduces | it no longer reproduces |
  | `Feature` | wanted, does not exist | it exists |
  | `Task` | scoped work that is neither — chores, migrations, follow-ups | the work is done |
  | `Record` | **complete when written** — a deviation, an audit snapshot, a counted hand-turn | on read, citing the artifact it points at |

  `Record` is the load-bearing one. A record is not work: leaving it open
  makes the backlog look like a workload, and closing it untyped makes the
  trail invisible. The type lets it be closed *and* stay countable as history.
  **A Record issue is a pointer, not a home** — the record itself lives in the
  tree (`docs/deviations/`, `docs/audits/`, `docs/decisions/`, `docs/adr/`,
  `docs/deliveries/`), and the issue names it. Close one with
  `scripts/close-issue.sh --artifact <path>`; the script refuses a path that is
  missing or untracked, so "it points at something" is enforced rather than
  trusted. #161 is the worked example.

  Open issues with `scripts/open-issue.sh` — it renders a body template and
  sets the type at creation, which is the half that keeps the taxonomy from
  decaying. It is a deliberately thin wrapper over `agtag issue post` and is
  meant to be **deleted** once `agentculture/agtag#19` lands template and type
  support upstream (`agentculture/gitculture-cli#17` covers the lifecycle and
  reporting half).

  Two things to know before reaching for the GitHub CLI directly: the installed
  `gh` (2.45.0) has **no `issueType` field at all**, so types are GraphQL-only
  here; and the search `type:` qualifier **fails open** — `type:NotARealType`
  returns `0` rather than an error, and the index lags writes. Read types
  per-issue via GraphQL and validate names against the org's list; never count
  them with a search query.

  **Scope of adoption**: culture-nodes adopts this practice. Issue types are
  org-level, so the vocabulary is *visible* in all 96 agentculture repos — but
  every other repo is offered it, not enrolled by this work. Do not cite this
  section as an org mandate.

- **pr-upkeep is the repeat process, and it has a recipe.** It is the one
  loop that runs on a clock against this repo itself, so a change to it is a
  change to a live process, not to a sample. Three documents divide it and all
  three are kept true: `examples/pr-upkeep/README.md` is the graph,
  `docs/operations/pr-upkeep-lane.md` is the operator recipe (one tick, how to
  read it, how to change the sweep), `docs/drive-from-jira.md` is what a
  person on a ticket sees. Change the loop's behaviour and you update the
  recipe in the same PR — the cadence claim ("a PR with N findings takes N
  ticks") is the kind of sentence that silently stops being true.
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
