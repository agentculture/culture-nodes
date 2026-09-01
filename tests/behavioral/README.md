# Behavioral tests

This folder is the repo's **behavioral-test convention** for the devague
execution-to-evidence leg (`/validate-delivery`, vendored at
`.claude/skills/validate-delivery/SKILL.md`). A behavioral test asserts a
behavior that a confirmed claim or an acceptance criterion *promised* — it is
the thing a `devague evidence` record cites, so an obligation can be met by a
test that ran rather than by a sentence that says it did.

The skill accepts either a dedicated folder or a marker. This repo uses
**both, together**: the folder is where behavioral tests live, and each
language carries the selector its own toolchain understands.

## The convention

| Language | Where | Selector | Run |
|---|---|---|---|
| Go | `tests/behavioral/*_test.go` | build tag `//go:build behavioral` | `go test -tags behavioral ./tests/behavioral/...` |
| Python | `tests/behavioral/test_*.py` | `@pytest.mark.behavioral` | `uv run pytest -m behavioral tests/behavioral` |

- **Go**: every file in this folder carries `//go:build behavioral` as its
  first line. Without the tag, `go test ./...` and `go vet ./...` see no
  buildable files here and skip the package — the ordinary suite is
  unchanged, and a behavioral test runs only when the leg asks for it.
- **Python**: every test function (or module, via `pytestmark`) carries the
  `behavioral` marker. The marker is registered in `pyproject.toml`
  (`[tool.pytest.ini_options] markers`) so `--strict-markers` and the
  unknown-marker warning stay clean. It is **not** excluded by default:
  `addopts` is `-ra` only, and CI's `tests.yml` inherits it, so adding
  `-m "not behavioral"` there would change what CI runs. Select with
  `-m behavioral`, deselect with `-m "not behavioral"`; excluding by default
  is a separate decision to record, not a drift.
- **Lint does not run behavioral tests.** `scripts/lint-all.sh --list` prints
  exactly the three CI jobs named `lint` (`root`, `adapter-codex`,
  `adapter-claude-code`) and no behavioral step; `tests/test_lint_all.py`
  pins that list. The Python files here are still *formatted* to the root
  black/isort/flake8 config like every other file under `tests/`, and the Go
  files still count toward `tests/lint`'s file-length guard — formatting is
  lint's business, running behavior is not.

## What goes here

A behavioral test is named after the obligation it discharges. Cite the
claim (`cN`) or task criterion (`tN` / criterion `N`) in the test's doc
comment so `devague evidence --test <ref>` and the ledger point at each
other. The test asserts the behavior at the seam the obligation names; it
does not modify code to make itself pass (`/validate-delivery` is read-only
against the codebase).

Unit tests that exercise an implementation detail stay where they are — this
folder is for the promised, observable behavior, and it is deliberately
small.

## Placeholders

`placeholder_behavioral_test.go` and `test_placeholder_behavioral.py` exist so
the selectors are exercised from day one: each runs, passes, and proves the
convention's plumbing (tag, marker, folder) before any real obligation lands.
Delete them once a real behavioral test exists in the same language.

## Where it plugs in

`docs/operations/validate-delivery-lane.md` explains when the leg runs, how
evidence and deltas are filed, and the strength ladder a filing declares.
