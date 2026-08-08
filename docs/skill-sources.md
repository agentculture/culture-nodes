# Skill upstream sources

culture-agent-template vendors its `.claude/skills/` from **guildmaster** — the
AgentCulture **skills supplier** after the steward → guildmaster cutover
(guildmaster 0.5.0, 2026-05-24). `steward` retains the **alignment** role
(`steward doctor`, the sibling-pattern baseline); only the skills-supplier role
moved. This file tracks provenance so re-syncs stay deterministic.

Seven skills — `think`, `spec-to-plan`, `assign-to-workforce`, `scope`,
`challenge`, `deviate`, and `summarize-delivery` — originate in
[`agentculture/devague`](https://github.com/agentculture/devague). The first
three are **re-broadcast** through guildmaster — cite guildmaster's copy; track
devague as the true origin. The remaining four (`scope`, `challenge`, `deviate`,
`summarize-delivery`) are vendored **directly from devague**: guildmaster's
current re-broadcast copies carry an added `scripts/*.sh` wrapper (guildmaster
`292feac`, "vendor scripts/ wrappers for 4 script-less devague skills") that the
devague originals — and these vendored copies — do not have, so citing
guildmaster's copy would pull in content this repo never asked for. This is a
tracked local divergence, parallel to `ask-colleague`'s below (see
[below](#local-divergence--scope--challenge--deviate--summarize-delivery-vendored-directly-from-devague-2026-07-15)).
One skill, `ask-colleague` (formerly `outsource`), originates in
[`agentculture/colleague`](https://github.com/agentculture/colleague) — the
renamed `convertible`. guildmaster's re-broadcast still carries the old
`outsource` name, so `ask-colleague` is vendored **directly from colleague** as a
tracked local divergence (see [below](#local-divergence--outsource--ask-colleague-2026-06-06)).

Every vendored `SKILL.md` carries `type: command`. culture-agent-template
declares a culture agent (`culture.yaml`, `backend: colleague`), and
`core.skill_loader` silently skips any `SKILL.md` lacking `type:` — so the field
is load-bearing, even where guildmaster's upstream copy omits it.

| Skill | Upstream | Origin | Notes | Last synced |
|-------|----------|--------|-------|-------------|
| `cicd` | `../guildmaster/.claude/skills/cicd/` | guildmaster | CI/CD lane layered on `devex pr`: the 5 thin scripts (`workflow.sh`, `pr-status.sh`, `pr-reply.sh`, `_resolve-nick.sh`, `portability-lint.sh`) delegate lint/open/read/reply/delta to `devex` and add the `status` / `await` SonarCloud-gating extensions. Consumer-identifying prose (`guildmaster` → `culture-agent-template`) adapted in the description + heading; upstream history (`Renamed from pr-review in steward 0.7.0; rebased on devex in 0.12.0`) and env-var literals (`STEWARD_*`) kept verbatim. The PR signature resolves at runtime from `culture.yaml` via `_resolve-nick.sh` (→ `culture-agent-template`). Requires `devex` on PATH. | 2026-05-26 (guildmaster 0.6.0) |
| `communicate` | `../guildmaster/.claude/skills/communicate/` | guildmaster | Cross-repo + mesh communication. Consumer-identifying prose adapted in the description (incl. the `- culture-agent-template (Claude)` signature line). **No hard-coded signature literal in the scripts** — `post-issue.sh` is `agtag`-backed and resolves the signing nick from `culture.yaml`; requires `agtag` (>=0.1) on PATH. The supplier `scripts/templates/` (`skill-update-brief.md`, `skill-new-brief.md`) are kept verbatim — inert for a consumer (they cite guildmaster as upstream). Renamed from `coordinate` in steward 0.8.0; absorbed `gh-issues` in 0.9.1. | 2026-05-26 (guildmaster 0.6.0) |
| `version-bump` | `../guildmaster/.claude/skills/version-bump/` | guildmaster | Pure-Python, CWD-aware (`scripts/bump.py`). Verbatim except added `type: command`. | 2026-05-26 (guildmaster 0.6.0) |
| `agent-config` | `../guildmaster/.claude/skills/agent-config/` | guildmaster (origin steward) | Shows a Culture agent's full config; run `scripts/show.sh` directly (no `guild` binary required). `scripts/show.sh` + `data/backend-fingerprints.yaml` verbatim. Verbatim except added `type: command`. | 2026-05-26 (guildmaster 0.6.0) |
| `doc-test-alignment` | `../guildmaster/.claude/skills/doc-test-alignment/` | guildmaster | **STUB** — `scripts/check.sh` exits not-yet-implemented; the contract lives in SKILL.md. Verbatim except added `type: command`. | 2026-05-26 (guildmaster 0.6.0) |
| `pypi-maintainer` | `../guildmaster/.claude/skills/pypi-maintainer/` | guildmaster | Switch a package install between PyPI / TestPyPI / local editable (`scripts/switch-source.sh`). Verbatim except added `type: command`. | 2026-05-26 (guildmaster 0.6.0) |
| `run-tests` | `../guildmaster/.claude/skills/run-tests/` | guildmaster | pytest + xdist + coverage (`scripts/test.sh`). Verbatim except added `type: command`. | 2026-05-26 (guildmaster 0.6.0) |
| `sonarclaude` | `../guildmaster/.claude/skills/sonarclaude/` | guildmaster | SonarCloud API queries (`scripts/sonar.sh`). Verbatim except added `type: command`. | 2026-05-26 (guildmaster 0.6.0) |
| `think` | `../guildmaster/.claude/skills/think/` | **devague** (re-broadcast via guildmaster) | idea→spec leg of the devague workflow chain. Verbatim (already carried `type: command` at guildmaster). Origin/broadcast prose left verbatim. | 2026-05-26 (guildmaster 0.6.0) |
| `spec-to-plan` | `../guildmaster/.claude/skills/spec-to-plan/` | **devague** (re-broadcast via guildmaster) | spec→plan leg of the devague workflow chain. Verbatim (already carried `type: command`). | 2026-05-26 (guildmaster 0.6.0) |
| `assign-to-workforce` | `../guildmaster/.claude/skills/assign-to-workforce/` | **devague** (re-broadcast via guildmaster) | plan→parallel-implementation leg of the devague workflow chain. Verbatim (already carried `type: command`). | 2026-05-26 (guildmaster 0.6.0) |
| `scope` | `../devague/.claude/skills/scope/` | **devague** (vendored directly — guildmaster's copy now carries an added `scripts/` wrapper; see [local divergence](#local-divergence--scope--challenge--deviate--summarize-delivery-vendored-directly-from-devague-2026-07-15)) | Explores the scope of a vague idea BEFORE framing it into a spec — the idea→scope leg, the optional opening move ahead of `/think`; surveys the surfaces the idea touches (code, docs, skills, CI, sibling repos) and seeds the coming Announcement Frame with boundary/non-goal/assumption claims that cite what was actually explored. Verbatim (carries `type: command`). | 2026-07-15 (devague#74/#75/#76) |
| `challenge` | `../devague/.claude/skills/challenge/` | **devague** (vendored directly — guildmaster's copy now carries an added `scripts/` wrapper; see [local divergence](#local-divergence--scope--challenge--deviate--summarize-delivery-vendored-directly-from-devague-2026-07-15)) | Runs a risk-scaled blind-spot discovery pass over a converged, exported frame BETWEEN `/think` and `/spec-to-plan` (the seventh origin skill, third leg in flow order): pressure-tests the spec through structured lenses, routes every finding back through the existing deterministic moves as proposed-only content the human adjudicates, and on a clean pass records the examined lenses/surfaces and residual uncertainty — never a claim that there are no unknown unknowns. Verbatim (carries `type: command`). | 2026-07-15 (devague#74/#75/#76) |
| `deviate` | `../devague/.claude/skills/deviate/` | **devague** (vendored directly — guildmaster's copy now carries an added `scripts/` wrapper; see [local divergence](#local-divergence--scope--challenge--deviate--summarize-delivery-vendored-directly-from-devague-2026-07-15)) | Stops an in-flight assign-to-workforce run the moment execution must diverge from the confirmed plan, gets explicit human approval for the divergence, and records it as a first-class, append-only deviation record via `devague deviate` before resuming — never folds a deviation silently into drift after the fact. Verbatim (carries `type: command`). | 2026-07-15 (devague#74/#75/#76) |
| `summarize-delivery` | `../devague/.claude/skills/summarize-delivery/` | **devague** (vendored directly — guildmaster's copy now carries an added `scripts/` wrapper; see [local divergence](#local-divergence--scope--challenge--deviate--summarize-delivery-vendored-directly-from-devague-2026-07-15)) | Closes the loop after an assign-to-workforce run by turning what actually happened into an accountability artifact — planned versus actual delivery, mid-work decisions, plan drift, evidence-backed delivery claims, and remaining work; runs on complete, partial, AND failed runs, reporting failure faithfully rather than smoothing it over. Verbatim (carries `type: command`). | 2026-07-15 (devague#74/#75/#76) |
| `ask-colleague` | `../colleague/.claude/skills/ask-colleague/` | **colleague** (renamed from convertible; vendored directly — guildmaster re-broadcast pending) | The first-party front door to the `colleague` CLI: hand a scoped task to a *different* engine/mind via `explore` / `review` / `write`, grade a finished work item via `feedback` (the ROI loop), and reap stale/corrupt `colleague/*` branches a crashed run left behind via `clean`. Every verb takes `--json` (result JSON on stdout, diagnostics on stderr). `explore`/`review` run isolated in a throwaway `git worktree`; `write` **previews by default** (throwaway worktree, no side effects) and refuses a dirty tree only when applying (`--apply` / `--pr`). Verbatim except one consumer-identifying clause in the Provenance paragraph (`colleague vendors from guildmaster` → `culture-agent-template vendors from guildmaster`); already carried `type: command`. Optional runtime dep: **`colleague`** on PATH. | 2026-06-12 (colleague 1.7.0, direct) |

## Re-sync procedure

```bash
# Diff against upstream before pulling (example: cicd / communicate):
for s in cicd communicate; do
  diff -ru ../guildmaster/.claude/skills/$s .claude/skills/$s
done

# Pull a skill fresh (remove first so dropped scripts don't linger):
rm -rf .claude/skills/<skill>
cp -R ../guildmaster/.claude/skills/<skill> .claude/skills/

# Re-apply the identifier-only adaptations in SKILL.md:
#   - consumer-identifying prose: `guildmaster` → `culture-agent-template` (NOT
#     where it cites guildmaster/steward/devague as the upstream/origin).
#   - add `type: command` to the frontmatter if guildmaster's copy omits it
#     (load-bearing for the culture/claude backend's core.skill_loader).
# No script bodies are edited (cite-don't-import). The communicate signature
# resolves from culture.yaml via agtag — no literal to patch.
```

If a re-sync would lose a culture-agent-template adaptation, lift the change
upstream into guildmaster first (per guildmaster's `docs/skill-sources.md`) and
re-vendor.

### Local divergence — `agex` → `devex` rename (2026-05-30)

The PR-lifecycle CLI was renamed `agex` → `devex` (same tool, new name). The
vendored `cicd` (`SKILL.md`, `workflow.sh`, `pr-status.sh`),
`assign-to-workforce`, and `communicate` (`skill-new-brief.md` template) copies
were **patched in place** for this rename rather than re-vendored — a deliberate
exception to cite-don't-import, made so the `cicd` scripts invoke the real
`devex pr` binary now. The matching canonical rename is tracked upstream for
guildmaster in [agentculture/guildmaster#48](https://github.com/agentculture/guildmaster/issues/48),
so the next clean re-sync from guildmaster reconciles without losing this
change. (Re-sync once guildmaster's renamed copies are broadcast.)

The same in-place patch also bumped the documented `devex` version floor from
`>=0.1` to `>=0.21` in the vendored `cicd` `SKILL.md` + `workflow.sh` (to match
this doc's tooling-prerequisites and the `await`-era feature set) — likewise
flagged for guildmaster on #48.

### Local divergence — outsource → ask-colleague (2026-06-06)

`convertible` was renamed **`colleague`**, and its skill `outsource` →
**`ask-colleague`** (colleague#148; the `wheels` verb also became `backends`, and
`drive` → `work`). `ask-colleague` adds a fourth verb, `feedback` (the ROI loop),
and `write` now **previews by default** (a throwaway worktree, no side effects)
instead of committing to a branch unless you pass `--apply` / `--pr`.

guildmaster has **not** re-broadcast the rename yet — its kit still ships the old
`outsource`. So this template's `outsource/` was removed and `ask-colleague/`
vendored **directly from the sibling `colleague` checkout**
(`../colleague/.claude/skills/ask-colleague/`), not from guildmaster. This is a
tracked exception to "cite guildmaster's copy", parallel to the `agex` → `devex`
divergence above. Re-sync path until guildmaster catches up:

```bash
# Pull ask-colleague fresh from colleague (the origin):
rm -rf .claude/skills/ask-colleague
cp -R ../colleague/.claude/skills/ask-colleague .claude/skills/
# Re-apply the one consumer-identifying clause in SKILL.md Provenance:
#   `which colleague vendors from guildmaster`
#     → `which culture-agent-template vendors from guildmaster`
# (already carries `type: command`; no script bodies edited.)
```

Once guildmaster re-broadcasts `ask-colleague`, switch the upstream column back
to `../guildmaster/.claude/skills/ask-colleague/` and re-sync from there.

### Local divergence — `scope` / `challenge` / `deviate` / `summarize-delivery` vendored directly from devague (2026-07-15)

These four skills were synced (`scope`, `deviate`, `summarize-delivery`) and
added (`challenge`, the seventh origin skill) from a **fixed devague source**
(devague#74/#75/#76). At sync time, guildmaster's re-broadcast copies of all
four had already picked up an added `scripts/*.sh` wrapper per skill
(guildmaster `292feac`, "fix(skills): vendor scripts/ wrappers for 4
script-less devague skills") that the devague originals do not carry — the
four skills are method-only / CLI-invoking `SKILL.md`s with no entry-point
script of their own. Diffing the vendored copies against both siblings
confirms it: `diff -ru ../devague/.claude/skills/<skill> .claude/skills/<skill>`
is byte-identical for all four, while the same diff against
`../guildmaster/.claude/skills/<skill>` shows guildmaster's extra `scripts/`
directory.

So, parallel to the `ask-colleague` divergence above, these four were vendored
**directly from the sibling `devague` checkout**
(`../devague/.claude/skills/<skill>/`), not from guildmaster — citing
guildmaster's copy here would silently pull in the unwanted wrapper scripts.
This is a tracked exception to "cite guildmaster's copy". Re-sync path:

```bash
# Diff against both siblings before pulling, to confirm devague is still the
# byte-identical match (i.e. guildmaster hasn't dropped the extra wrapper):
for s in scope challenge deviate summarize-delivery; do
  diff -ru ../devague/.claude/skills/$s .claude/skills/$s
  diff -ru ../guildmaster/.claude/skills/$s .claude/skills/$s
done

# Pull fresh from devague (the origin):
for s in scope challenge deviate summarize-delivery; do
  rm -rf .claude/skills/$s
  cp -R ../devague/.claude/skills/$s .claude/skills/
done
# All four already carry `type: command`; no script bodies to edit (there are
# no scripts in the devague originals) and no consumer-identifying prose to
# adapt (each SKILL.md's Provenance section already speaks generically of
# "downstream repos").
```

If guildmaster ever re-broadcasts these four **without** the extra `scripts/`
wrapper (i.e. its copy goes back to matching devague byte-for-byte), switch
the upstream column back to `../guildmaster/.claude/skills/<skill>/` and
re-sync from there per the normal procedure.

## Tooling prerequisites

- **`devex`** (>=0.21) on PATH — `cicd` delegates the PR lifecycle to `devex pr`.
- **`agtag`** (>=0.1) on PATH — `communicate` issue I/O wraps `agtag issue`.

Both ship on PATH in the standard AgentCulture dev setup (installed per the
devex / agtag READMEs).

- **`colleague`** on PATH — *optional*; only the `ask-colleague` skill needs it,
  and only when invoked (`uv tool install colleague`). The wrapper exits
  with a clear install hint if it is absent, so the skill degrades gracefully
  rather than blocking a clone that never uses it. `ask-colleague` also needs a
  reachable backend — a local vLLM by default, overridable via `--engine` /
  `--model` / `--base-url` or `COLLEAGUE_*` env (the legacy `CONVERTIBLE_*` names
  still work as a deprecated fallback).
