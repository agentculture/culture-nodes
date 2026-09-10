"""examples/land/land_gate.py -- the land node's gate chain and its single
version bump (loop-closure task t7; spec c3/c7/c15/c36, honesty h9/h12/h16).

Same scratch-remote fixtures as tests/test_land_node.py (imported, not
copied), with two differences that are the point of this file:

* the ORIGIN carries what a gate needs -- a ``pyproject.toml`` at 0.1.0, a
  ``CHANGELOG.md``, a stub ``scripts/lint-all.sh`` that records its argv, and
  the REAL ``.claude/skills/version-bump/scripts/bump.py`` copied from this
  repository, so the bump the node performs is the bump the operator's
  skill performs;
* the TOOLCHAIN is faked on PATH: ``go``, ``uv``, ``node`` and
  ``markdownlint-cli2`` are recording shims in a temporary bin directory, so
  the chain runs in milliseconds, its ORDER and ARGV are asserted from the
  shim log, and one shim can be told to fail (``FAKE_GATE_FAIL``) or be
  removed altogether to stand in for the host where Go is not on PATH.

Nothing here reaches a network, a real toolchain, or this repository's own
pyproject.toml.
"""

from __future__ import annotations

import concurrent.futures
import importlib.util
import json
import os
import re
import shutil
import stat
import sys
from pathlib import Path

import pytest

from tests.test_land_node import (  # noqa: F401 - fixtures are collected by name
    ACTOR_ID,
    EXAMPLE_DIR,
    LAND_RUN,
    PRODUCING_RUN,
    TARGET,
    WORK_ITEM,
    WORKFLOW,
    actor_checkout,
    code_only,
    commit_file,
    git,
    land,
    land_ws,
    log_subjects,
    mint_handover,
    result,
    run_land,
    steps,
)

ROOT = Path(__file__).resolve().parents[1]
GATE_SCRIPT = EXAMPLE_DIR / "land_gate.py"
REAL_BUMP = ROOT / ".claude/skills/version-bump/scripts/bump.py"
FAKE_TOOLCHAINS = ("go", "uv", "node", "markdownlint-cli2")


def _load_gate():
    spec = importlib.util.spec_from_file_location("land_gate", GATE_SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


land_gate = _load_gate()


# ---------------------------------------------------------------------------
# the fake toolchain and the gate-shaped origin
# ---------------------------------------------------------------------------

_SHIM = """#!/usr/bin/env bash
name=$(basename "$0")
printf '%s %s\\n' "$name" "$*" >> "$FAKE_GATE_LOG"
if [ "${FAKE_GATE_FAIL:-}" = "$name" ]; then
  echo "$name: fake failure line 1"
  echo "$name: fake failure line 2 -- the tail the human reads" >&2
  exit 1
fi
echo "$name: ok"
"""

_LINT_ALL_STUB = """#!/usr/bin/env bash
printf 'lint-all %s\\n' "$*" >> "$FAKE_GATE_LOG"
if [ "${FAKE_GATE_FAIL:-}" = lint-all ]; then
  echo "<<< FAILED flake8 (fake)"
  exit 1
fi
echo "lint-all: every job green (fake)"
"""


def _write_exec(path: Path, body: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(body)
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


@pytest.fixture
def toolchain(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Every required binary, as a recording shim first on PATH."""
    bin_dir = tmp_path / "toolchain-bin"
    for name in FAKE_TOOLCHAINS:
        _write_exec(bin_dir / name, _SHIM)
    monkeypatch.setenv("PATH", f"{bin_dir}{os.pathsep}{os.environ['PATH']}")
    log = tmp_path / "gate-calls.log"
    log.touch()
    monkeypatch.setenv("FAKE_GATE_LOG", str(log))
    monkeypatch.delenv("FAKE_GATE_FAIL", raising=False)
    return bin_dir


def gate_log(tmp_path: Path) -> list[str]:
    return (tmp_path / "gate-calls.log").read_text().splitlines()


@pytest.fixture
def origin(tmp_path: Path) -> Path:
    """The scratch bare remote, shaped like a repository the gate can run
    in: pyproject at 0.1.0, a CHANGELOG, the lint-all stub and the real
    bump.py. main plus the PR branch TARGET, as in test_land_node."""
    seed = tmp_path / "seed"
    seed.mkdir()
    git(seed, "init", "-q", "-b", "main")
    (seed / "README.md").write_text("base\n")
    (seed / "pyproject.toml").write_text('[project]\nname = "scratch"\nversion = "0.1.0"\n')
    (seed / "CHANGELOG.md").write_text(
        "# Changelog\n\n## [0.1.0] - 2026-01-01\n\n### Added\n\n- seed\n"
    )
    _write_exec(seed / "scripts/lint-all.sh", _LINT_ALL_STUB)
    bump = seed / ".claude/skills/version-bump/scripts/bump.py"
    bump.parent.mkdir(parents=True)
    shutil.copy(REAL_BUMP, bump)
    git(seed, "add", "-A")
    git(seed, "commit", "-q", "-m", "base")
    git(seed, "checkout", "-q", "-b", TARGET)
    commit_file(seed, "feature.txt", "feature work\n", "feature work")
    bare = tmp_path / "origin.git"
    git(tmp_path, "clone", "-q", "--bare", str(seed), str(bare))
    return bare


@pytest.fixture(autouse=True)
def _gate_env(monkeypatch: pytest.MonkeyPatch):
    """test_land_node's quiet environment, WITHOUT its gate stub: this file
    is where the real gate runs."""
    monkeypatch.delenv("NODES_API_URL", raising=False)
    monkeypatch.delenv("GITHUB_TOKEN_WORKER", raising=False)
    monkeypatch.setenv("LAND_LEASE_WAIT_SECONDS", "0")
    monkeypatch.setenv("LAND_PUSH_ENV_FILE", "/nonexistent/bridge-push.env")
    for name in ("LAND_GATE_TESTS", "LAND_GATE_JOB", "LAND_FINDINGS_JSON", "LAND_BUMP_PART"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setattr(land, "active_attempts", lambda *_a, **_k: None)


def mint_fixes(repo: Path, run_id: str, subjects: list[str]) -> tuple[str, str]:
    """A handover ref pointing at a CHAIN of fix commits, the way one actor
    session that fixed N findings hands them over: one ref, N commits."""
    original = git(repo, "rev-parse", "--abbrev-ref", "HEAD")
    git(repo, "checkout", "-q", "--detach")
    sha = ""
    for i, subject in enumerate(subjects):
        sha = commit_file(repo, f"src/fix_{i}.py", f"FIX_{i} = {i}\n", subject)
    ref = f"refs/culture-nodes/{run_id}/20260907T000000Z-fixes"
    git(repo, "update-ref", ref, sha)
    git(repo, "checkout", "-q", original)
    return ref, sha


def show(origin: Path, rev: str, path: str) -> str:
    return git(origin, "show", f"{rev}:{path}")


def routing_records(records: list[dict]) -> list[dict]:
    return [r for r in records if r.get("record") == "routing"]


# ---------------------------------------------------------------------------
# the declaration: what the gate needs, said before it runs
# ---------------------------------------------------------------------------


def test_the_gate_declares_go_uv_node_and_markdownlint_and_the_workflow_names_them():
    assert set(land_gate.REQUIRED_TOOLCHAINS) == set(FAKE_TOOLCHAINS)
    for name, needed_for in land_gate.REQUIRED_TOOLCHAINS.items():
        assert needed_for, f"{name} is declared without saying what needs it"
    prose = WORKFLOW.read_text(encoding="utf-8")
    for name in FAKE_TOOLCHAINS:
        assert name in prose, f"workflow.yaml never names the {name} toolchain"
    # The bootstrap fetches the gate module beside land.py, by granted digest.
    yaml = pytest.importorskip("yaml")
    node = yaml.safe_load(prose)["spec"]["nodes"]["land"]
    for ref in ("LAND_GATE_SOURCE_URL", "LAND_GATE_SOURCE_SHA256"):
        assert ref in node["operation"]["environmentRefs"]
        assert ref in prose.split("apiVersion:")[0], f"{ref} is granted but undocumented"
    assert "land_gate.py" in " ".join(node["operation"]["argv"])
    # And land.py's hook reaches for it rather than answering not_implemented.
    hook = (EXAMPLE_DIR / "land.py").read_text(encoding="utf-8")
    assert "from land_gate import run_gate" in hook


# ---------------------------------------------------------------------------
# the happy path: N fixes, one bump, one CHANGELOG entry, chain in order
# ---------------------------------------------------------------------------


def test_landing_n_fixes_yields_one_bump_and_one_changelog_entry_naming_them(
    monkeypatch, capsys, tmp_path, toolchain, origin, land_ws, actor_checkout
):
    before = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    subjects = ["fix S9073 in a.py", "fix S9073 in b.py", "fix S5778 in c.py"]
    ref, sha = mint_fixes(actor_checkout, PRODUCING_RUN, subjects)
    findings = ["S9073:a.py", "S9073:b.py", "S5778:c.py"]
    monkeypatch.setenv("LAND_FINDINGS_JSON", json.dumps(findings))

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_LANDED, records
    tip = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    # Three fix commits, untouched, then EXACTLY ONE bump commit on top.
    assert git(origin, "rev-parse", f"{tip}^") == sha, "the bump sits on the handover commits"
    assert git(origin, "rev-parse", f"{tip}~4") == before
    assert log_subjects(origin, tip)[1:4] == list(reversed(subjects))
    assert log_subjects(origin, tip)[0].startswith("land ")
    assert git(origin, "rev-list", "--merges", "--count", tip) == "0"

    assert 'version = "0.1.1"' in show(origin, tip, "pyproject.toml")
    assert 'version = "0.1.0"' in show(origin, f"{tip}^", "pyproject.toml"), "not bumped twice"
    changelog = show(origin, tip, "CHANGELOG.md")
    assert changelog.count("## [") == 2, changelog
    assert changelog.count("## [0.1.1]") == 1
    entry = changelog.split("## [0.1.0]")[0]
    for finding in findings:
        assert finding in entry, f"{finding} is not named in the CHANGELOG entry"
    assert WORK_ITEM in entry
    # The bump commit body names the findings too, so the trail runs both ways.
    body = git(origin, "log", "-1", "--format=%B", tip)
    for finding in findings:
        assert finding in body

    gate = steps(records)["gate"]
    assert gate["outcome"] == "ok"
    assert [s["name"] for s in gate["steps"]] == ["tests", "go_lint", "lint_all", "file_length"]
    assert all(s["exit_code"] == 0 for s in gate["steps"])
    assert gate["bump"] == {
        **gate["bump"],
        "old": "0.1.0",
        "new": "0.1.1",
        "commit": tip,
        "findings": findings,
    }
    assert result(records)["landed_commit"] == tip
    assert steps(records)["push"]["commit"] == tip
    # The producing checkout was reset to the tip INCLUDING the bump.
    assert git(actor_checkout, "rev-parse", "HEAD") == tip

    # The chain ran in the operator's order, with the operator's argv.
    log = gate_log(tmp_path)
    assert log == [
        "uv run pytest -n auto -q",
        "go test ./tests/lint/...",
        "lint-all root",
    ], log


def test_findings_default_to_the_landed_commit_subjects(
    monkeypatch, capsys, toolchain, origin, land_ws, actor_checkout
):
    subjects = ["review(#307): S9073 split composite assertion", "review(#307): S5778 one call"]
    ref, _ = mint_fixes(actor_checkout, PRODUCING_RUN, subjects)

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_LANDED, records
    tip = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    entry = show(origin, tip, "CHANGELOG.md").split("## [0.1.0]")[0]
    for subject in subjects:
        assert subject in entry
    assert steps(records)["gate"]["bump"]["findings"] == subjects


def test_gate_tests_and_job_are_configurable(
    monkeypatch, capsys, tmp_path, toolchain, origin, land_ws, actor_checkout
):
    monkeypatch.setenv("LAND_GATE_TESTS", "uv run pytest tests/test_land_node.py -q")
    monkeypatch.setenv("LAND_GATE_JOB", "adapter-codex")
    ref, _ = mint_fixes(actor_checkout, PRODUCING_RUN, ["one fix"])

    code, _records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_LANDED
    assert gate_log(tmp_path)[0] == "uv run pytest tests/test_land_node.py -q"
    assert gate_log(tmp_path)[2] == "lint-all adapter-codex"


# ---------------------------------------------------------------------------
# bump.py: stdin JSON, never a TTY wait
# ---------------------------------------------------------------------------


def test_bump_py_is_fed_the_changelog_on_stdin_and_does_not_block(tmp_path, origin):
    """bump.py reads the changelog JSON from stdin and, run with no stdin at
    all under a non-TTY, waits forever (the hazard the last cycle met). The
    gate must therefore hand it the JSON explicitly -- asserted by running
    the bump under a hard timeout, in a thread so a regression is a
    test failure and not a hung suite."""
    clone = tmp_path / "bump-clone"
    git(tmp_path, "clone", "-q", "-b", TARGET, str(origin), str(clone))
    findings = ["S9073:a.py", "S5778:c.py"]

    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
        future = pool.submit(
            land_gate.bump_version,
            land.git,
            clone,
            "patch",
            {"changed": [f"{WORK_ITEM}: {f}" for f in findings]},
        )
        old, new, stdout = future.result(timeout=30)

    assert (old, new) == ("0.1.0", "0.1.1")
    assert "0.1.0 -> 0.1.1" in stdout
    assert 'version = "0.1.1"' in (clone / "pyproject.toml").read_text()
    changelog = (clone / "CHANGELOG.md").read_text()
    assert changelog.count("## [0.1.1]") == 1
    for finding in findings:
        assert finding in changelog
    # The source itself passes stdin content: no `stdin=None` default reaches
    # the child, and the timeout is a constant a reader can find.
    code = code_only(GATE_SCRIPT.read_text(encoding="utf-8"))
    assert re.search(r"input\s*=\s*json\s*\.\s*dumps", code)
    assert land_gate.BUMP_TIMEOUT_SECONDS > 0


def test_the_bump_is_exactly_one_patch_by_default():
    assert land_gate.DEFAULT_BUMP_PART == "patch"
    assert land_gate.gate_config().bump_part == "patch"


# ---------------------------------------------------------------------------
# a red gate: routed to a human, nothing pushed, branch untouched
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("failing_binary", "failing_step"),
    [("uv", "tests"), ("go", "go_lint"), ("lint-all", "lint_all")],
)
def test_a_red_gate_routes_to_a_human_and_leaves_the_branch_untouched(
    monkeypatch,
    capsys,
    tmp_path,
    toolchain,
    origin,
    land_ws,
    actor_checkout,
    failing_binary,
    failing_step,
):
    before = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    ref, _ = mint_fixes(actor_checkout, PRODUCING_RUN, ["fix a", "fix b"])
    monkeypatch.setenv("FAKE_GATE_FAIL", failing_binary)

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_ROUTED_HUMAN, records
    assert result(records)["outcome"] == "routed_human"
    assert git(origin, "rev-parse", f"refs/heads/{TARGET}") == before, "the branch moved"
    assert "push" not in steps(records) and "reset" not in steps(records)

    gate = steps(records)["gate"]
    assert gate["outcome"] == "routed"
    assert gate["reason"] == land_gate.REASON_GATE_FAILED == "gate_failed"
    assert gate["failing_step"] == failing_step

    routing = routing_records(records)
    assert len(routing) == 1
    data = routing[0]["data"]
    assert data["reason"] == "gate_failed"
    assert data["selected"] == "human" and data["dispatched"] is False
    assert data["router"] == land.ROUTER_COLLECTION_METHOD == "land_routing"
    assert data["failing_step"] == failing_step
    assert failing_step in data["rationale"]
    assert "fake failure" in data["tail"], "the tail of the failing step rides on the record"
    assert "fake failure" in data["rationale"]
    # Steps after the failing one never ran; the bump never happened.
    ran = [line.split()[0] for line in gate_log(tmp_path)]
    order = ["uv", "go", "lint-all"]
    assert ran == order[: order.index(failing_binary) + 1], ran
    # The producing checkout was not reset: it still sits where it was.
    assert git(actor_checkout, "rev-parse", "HEAD") == before


def test_an_oversized_source_file_in_the_handover_is_a_red_gate(
    monkeypatch, capsys, toolchain, origin, land_ws, actor_checkout
):
    before = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    original = git(actor_checkout, "rev-parse", "--abbrev-ref", "HEAD")
    git(actor_checkout, "checkout", "-q", "--detach")
    body = "".join(f"X_{i} = {i}\n" for i in range(land_gate.MAX_SOURCE_FILE_LINES + 1))
    sha = commit_file(actor_checkout, "src/huge.py", body, "one giant module")
    ref = f"refs/culture-nodes/{PRODUCING_RUN}/20260907T000000Z-huge"
    git(actor_checkout, "update-ref", ref, sha)
    git(actor_checkout, "checkout", "-q", original)

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_ROUTED_HUMAN, records
    gate = steps(records)["gate"]
    assert gate["failing_step"] == "file_length"
    assert "src/huge.py: 1001 lines" in routing_records(records)[0]["data"]["tail"]
    assert git(origin, "rev-parse", f"refs/heads/{TARGET}") == before


def test_a_hung_gate_step_is_a_red_gate_not_a_hung_landing(
    monkeypatch, capsys, toolchain, origin, land_ws, actor_checkout
):
    _write_exec(toolchain / "uv", "#!/usr/bin/env bash\nsleep 30\n")
    monkeypatch.setenv("LAND_GATE_STEP_TIMEOUT_SECONDS", "1")
    ref, _ = mint_fixes(actor_checkout, PRODUCING_RUN, ["fix a"])

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_ROUTED_HUMAN, records
    gate = steps(records)["gate"]
    assert gate["failing_step"] == "tests"
    assert gate["timed_out"] is True


# ---------------------------------------------------------------------------
# a missing toolchain: refused by name, before anything runs
# ---------------------------------------------------------------------------


def test_a_missing_toolchain_refuses_by_name_before_running_anything(
    monkeypatch, capsys, tmp_path, toolchain, origin, land_ws, actor_checkout
):
    """The thor fact (spec s26): go is not on the account's PATH. The gate
    must say so by name -- not fail three steps in with `go: command not
    found` in a tail -- and must not have started the chain."""
    (toolchain / "go").unlink()
    # No other `go` may answer for the missing one: PATH becomes the shim
    # directory alone, plus the real git and the shells the shims need.
    for helper in ("git", "bash", "sh", "basename", "env"):
        real = shutil.which(helper)
        assert real, helper
        os.symlink(real, toolchain / helper)
    monkeypatch.setenv("PATH", str(toolchain))
    before = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    ref, _ = mint_fixes(actor_checkout, PRODUCING_RUN, ["fix a"])

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_ENVIRONMENT, records
    res = result(records)
    assert res["outcome"] == "environment"
    assert "go" in res["error"]
    missing = [r for r in records if r.get("record") == "toolchain_missing"]
    assert [r["binary"] for r in missing] == ["go"]
    assert missing[0]["needed_for"] == land_gate.REQUIRED_TOOLCHAINS["go"]
    assert missing[0]["work_item"] == WORK_ITEM and missing[0]["land_run_id"] == LAND_RUN
    gate = steps(records)["gate"]
    assert gate["outcome"] == "refused" and gate["reason"] == "toolchain_missing"
    assert gate["missing"] == ["go"]
    assert gate_log(tmp_path) == [], "the chain started although a toolchain was missing"
    assert git(origin, "rev-parse", f"refs/heads/{TARGET}") == before
    assert "push" not in steps(records)


def test_missing_toolchains_are_computed_from_path_not_assumed(tmp_path):
    empty = tmp_path / "empty-bin"
    empty.mkdir()
    assert land_gate.missing_toolchains({"PATH": str(empty)}) == list(FAKE_TOOLCHAINS)
    for name in FAKE_TOOLCHAINS:
        _write_exec(empty / name, "#!/bin/sh\n")
    assert land_gate.missing_toolchains({"PATH": str(empty)}) == []


# ---------------------------------------------------------------------------
# exactly one bump per landing, across re-runs and pre-bumped handovers
# ---------------------------------------------------------------------------


def test_a_rerun_after_the_landing_bumps_nothing_again(
    monkeypatch, capsys, tmp_path, toolchain, origin, land_ws, actor_checkout
):
    ref, _ = mint_fixes(actor_checkout, PRODUCING_RUN, ["fix a", "fix b"])
    code, _ = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )
    assert code == land.EXIT_LANDED
    tip = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    (tmp_path / "gate-calls.log").write_text("")

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_LANDED, records
    assert git(origin, "rev-parse", f"refs/heads/{TARGET}") == tip, "a re-run moved the branch"
    gate = steps(records)["gate"]
    assert gate["outcome"] == "skipped" and gate["reason"] == "already_on_branch"
    assert gate_log(tmp_path) == [], "a re-run re-ran the chain on a landed commit"
    assert show(origin, tip, "CHANGELOG.md").count("## [") == 2


def test_a_handover_that_already_bumps_the_version_is_not_bumped_again(
    monkeypatch, capsys, toolchain, origin, land_ws, actor_checkout
):
    """The old behaviour: a fix session read CLAUDE.md's bump rule and bumped
    itself. Landing it must not stack a second bump on the first."""
    original = git(actor_checkout, "rev-parse", "--abbrev-ref", "HEAD")
    git(actor_checkout, "checkout", "-q", "--detach")
    commit_file(actor_checkout, "src/a.py", "a = 1\n", "fix a")
    sha = commit_file(
        actor_checkout, "pyproject.toml", '[project]\nname = "scratch"\nversion = "0.1.1"\n', "bump"
    )
    ref = f"refs/culture-nodes/{PRODUCING_RUN}/20260907T000000Z-prebumped"
    git(actor_checkout, "update-ref", ref, sha)
    git(actor_checkout, "checkout", "-q", original)

    code, records = run_land(
        monkeypatch, capsys, workspace=land_ws, handover_ref=ref, handover_remote=actor_checkout
    )

    assert code == land.EXIT_LANDED, records
    tip = git(origin, "rev-parse", f"refs/heads/{TARGET}")
    assert tip == sha, "the lander added a commit on a handover that already bumped"
    assert 'version = "0.1.1"' in show(origin, tip, "pyproject.toml")
    gate = steps(records)["gate"]
    assert gate["outcome"] == "ok"
    assert gate["bump"] == {"skipped": "handover_already_bumps"}


# ---------------------------------------------------------------------------
# the fences
# ---------------------------------------------------------------------------


def test_the_gate_never_pushes_forces_or_merges():
    code = code_only(GATE_SCRIPT.read_text(encoding="utf-8"))
    assert '"push"' not in code and "'push'" not in code, "only land.py pushes"
    assert "--force" not in code
    assert not re.search(r"/pulls/[^\n\"']*/merge", code)
    assert "api.github.com" not in code
    assert "urlopen" not in code, "the gate performs no HTTP at all"
    assert "amend" not in code, "the bump is a commit ON TOP of the handover, never a rewrite"
    assert "rebase" not in code.replace("rebased", ""), "rebasing is land.py's step, not the gate's"


def test_the_gate_module_stays_under_the_file_length_guard():
    for path in (GATE_SCRIPT, EXAMPLE_DIR / "land.py"):
        assert len(path.read_text(encoding="utf-8").splitlines()) <= 1000, path


def test_the_readme_documents_the_gate_row():
    readme = (EXAMPLE_DIR / "README.md").read_text(encoding="utf-8")
    assert "not_implemented" not in readme.split("| `gate` |")[1].split("\n")[0]
    for token in ("land_gate.py", "gate_failed", "toolchain_missing", "bump.py"):
        assert token in readme, token
