"""Markdown catalog for ``culture-nodes explain <path>``.

Each entry is verbatim markdown. Keys are command-path tuples. The empty tuple
and ``("culture-nodes",)`` both resolve to the root entry.

Keep bodies self-contained: an agent reading one entry should get enough
context without chaining reads.
"""

from __future__ import annotations

_ROOT = """\
# culture-nodes

A clonable template for AgentCulture mesh agents. It carries an agent-first CLI
(cited from the teken `python-cli` reference), a mesh identity (`culture.yaml` +
`CLAUDE.md`), the canonical guildmaster skill kit under `.claude/skills/`, and a
buildable/deployable package baseline. Clone it, rename the package, edit
`culture.yaml`, and you have a new agent.

## Verbs

- `culture-nodes whoami` — identity probe from `culture.yaml`.
- `culture-nodes learn` — structured self-teaching prompt.
- `culture-nodes explain <path>` — markdown docs for any noun/verb.
- `culture-nodes overview` — descriptive snapshot of the agent.
- `culture-nodes doctor` — check the agent-identity invariants.
- `culture-nodes cli overview` — describe the CLI surface.

## Exit-code policy

- `0` success
- `1` user-input error
- `2` environment / setup error
- `3+` reserved

## See also

- `culture-nodes explain whoami`
- `culture-nodes explain doctor`
"""

_WHOAMI = """\
# culture-nodes whoami

Reports the agent's identity from `culture.yaml`: nick (`suffix`), backend,
served model, and the package version. Read-only.

## Usage

    culture-nodes whoami
    culture-nodes whoami --json
"""

_LEARN = """\
# culture-nodes learn

Prints a structured self-teaching prompt covering purpose, command map,
exit-code policy, `--json` support, and the `explain` pointer.

## Usage

    culture-nodes learn
    culture-nodes learn --json
"""

_EXPLAIN = """\
# culture-nodes explain <path>

Prints markdown documentation for any noun/verb path. Unlike `--help` (terse,
positional), `explain` is global and addressable by path.

## Usage

    culture-nodes explain culture-nodes
    culture-nodes explain whoami
    culture-nodes explain --json <path>
"""

_OVERVIEW = """\
# culture-nodes overview

Read-only descriptive snapshot of the agent: identity (from `culture.yaml`), the
verb surface, and the sibling-pattern artifacts the template carries. Accepts an
ignored `target` so a stray path never hard-fails.

## Usage

    culture-nodes overview
    culture-nodes overview --json
"""

_DOCTOR = """\
# culture-nodes doctor

Checks the agent-identity invariants `steward doctor` verifies:
prompt-file-present and backend-consistency (`colleague` → `AGENTS.colleague.md`), plus a
skills-present check. Exits 1 when unhealthy.

## Usage

    culture-nodes doctor
    culture-nodes doctor --json
"""

_CLI = """\
# culture-nodes cli

Noun group for CLI-surface introspection. `cli overview` describes the CLI
itself (distinct from the global `overview`, which describes the agent).

## Usage

    culture-nodes cli overview
    culture-nodes cli overview --json
"""


ENTRIES: dict[tuple[str, ...], str] = {
    (): _ROOT,
    ("culture-nodes",): _ROOT,
    ("nodes",): _ROOT,
    ("whoami",): _WHOAMI,
    ("learn",): _LEARN,
    ("explain",): _EXPLAIN,
    ("overview",): _OVERVIEW,
    ("doctor",): _DOCTOR,
    ("cli",): _CLI,
    ("cli", "overview"): _CLI,
}
