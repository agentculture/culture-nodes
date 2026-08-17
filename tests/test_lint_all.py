"""One script carries every lint job CI runs, and CI calls it (issue #123, task t8).

Three CI jobs are literally named `lint`, and they invoke the same four
linters differently:

    tests.yml                root scope, from the repo root
    adapter-codex.yml        from the REPO ROOT against adapters/codex paths,
                             so the ROOT black/isort/flake8 config applies
    adapter-claude-code.yml  from the ADAPTER DIRECTORY, so that adapter's own
                             config applies

The difference is not cosmetic and it is invisible to anyone reading one file.
Run the adapter-directory form for codex and it passes locally; CI runs the
repo-root form and it fails. PR #122 went red on all three jobs named `lint`
after a fully green local run, because there was no one command whose green
meant CI's lint would be green.

The fix is structural, not documentary: the commands live once, in
`scripts/lint-all.sh`, and each workflow calls it with a job selector. So what
these tests guard is that nobody quietly puts a linter invocation back into a
workflow -- the moment a workflow spells out its own `black --check`, the two
copies can disagree again and the defect is back with the docs still claiming
otherwise.

The scope is deliberately the three jobs named `lint`. go.yml's `go vet` and
web.yml's `webglass` are a parked question, and `test_scope_is_exactly_the_jobs_named_lint`
pins today's answer so widening it has to be a decision rather than a drift.
"""

import re
import subprocess  # nosec B404 - runs an in-repo shell script, no external input
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "lint-all.sh"
WORKFLOWS = ROOT / ".github" / "workflows"

# job name in lint-all.sh -> workflow file whose `lint` job it reproduces.
JOB_WORKFLOWS = {
    "root": "tests.yml",
    "adapter-codex": "adapter-codex.yml",
    "adapter-claude-code": "adapter-claude-code.yml",
}

# The linters whose invocation differs between the three jobs. A workflow that
# names one of these directly has started keeping a second copy.
LINTERS = ("black", "isort", "flake8", "bandit", "markdownlint-cli2")


def workflow_text(name):
    return (WORKFLOWS / name).read_text(encoding="utf-8")


def lint_job_block(name):
    """The `lint:` job of a workflow, up to the next job at the same indent."""
    text = workflow_text(name)
    match = re.search(r"^  lint:\n(.*?)(?=^  [a-z0-9_-]+:\n|\Z)", text, re.S | re.M)
    assert match, f"{name} has no job named `lint`"
    return match.group(0)


def script_text():
    return SCRIPT.read_text(encoding="utf-8")


def run_script(*args):
    return subprocess.run(  # nosec B603 - fixed argv, no shell
        [str(SCRIPT), *args], text=True, capture_output=True, cwd=ROOT, timeout=120
    )


def test_the_script_is_executable():
    """CI invokes it as a bare path, the way it invokes check-zero-runtime-deps.sh."""
    assert SCRIPT.exists()
    assert SCRIPT.stat().st_mode & 0o111


def test_it_lists_exactly_the_three_jobs():
    result = run_script("--list")

    assert result.returncode == 0, result.stderr
    assert result.stdout.split() == list(JOB_WORKFLOWS)


def test_an_unknown_job_is_refused_with_a_hint():
    """A typo must not silently lint nothing and report success."""
    result = run_script("no-such-job")

    assert result.returncode == 2
    assert "error:" in result.stderr
    assert "hint:" in result.stderr


def test_scope_is_exactly_the_jobs_named_lint():
    """Every workflow job literally named `lint`, and no other, has a job here.

    go.yml's `go vet` and web.yml's `webglass` are deliberately outside this
    script's remit. If that changes, this test is the place the decision gets
    recorded.
    """
    named_lint = {
        path.name
        for path in sorted(WORKFLOWS.glob("*.yml"))
        if re.search(r"^  lint:$", path.read_text(encoding="utf-8"), re.M)
    }

    assert named_lint == set(JOB_WORKFLOWS.values())


@pytest.mark.parametrize("job,workflow", sorted(JOB_WORKFLOWS.items()))
def test_each_lint_job_invokes_the_script_with_its_selector(job, workflow):
    block = lint_job_block(workflow)

    assert f"lint-all.sh {job}" in block, f"{workflow}'s lint job does not call the script"


@pytest.mark.parametrize("workflow", sorted(JOB_WORKFLOWS.values()))
def test_no_lint_job_spells_out_a_linter_itself(workflow):
    """The whole point: one copy of the commands, not four.

    Comments are stripped first -- the workflows explain WHY the two adapter
    forms differ, and naming `black` in that explanation is not a second copy.
    """
    lines = [
        line for line in lint_job_block(workflow).splitlines() if not line.lstrip().startswith("#")
    ]
    body = "\n".join(lines)

    for linter in LINTERS:
        assert linter not in body, f"{workflow}'s lint job still invokes {linter} directly"


def test_the_script_carries_the_codex_form_from_the_repo_root():
    """Adapter-prefixed paths, so the ROOT config applies -- the #122 trap."""
    text = script_text()

    assert "uv run black --check adapters/codex/src adapters/codex/tests" in text
    assert "uv run isort --check-only adapters/codex/src adapters/codex/tests" in text
    assert "uv run flake8 adapters/codex/src adapters/codex/tests" in text
    assert "uv run bandit -c pyproject.toml -r adapters/codex/src" in text


def test_the_script_carries_the_claude_code_form_from_the_adapter_directory():
    """Bare `src tests` after a cd, so the ADAPTER's own config applies."""
    text = script_text()

    assert 'in_dir "$dir" uv run black --check src tests' in text
    assert 'in_dir "$dir" uv run isort --check-only src tests' in text
    assert 'in_dir "$dir" uv run flake8 src tests' in text
    assert 'local dir="$ROOT/adapters/claude-code"' in text


def test_the_script_carries_the_root_job_steps():
    text = script_text()

    assert "uv run black --check culture_nodes tests" in text
    assert "uv run isort --check-only culture_nodes tests" in text
    assert "uv run flake8 culture_nodes tests" in text
    assert "uv run bandit -c pyproject.toml -r culture_nodes" in text
    assert "scripts/check-zero-runtime-deps.sh" in text
    assert "scripts/check-vendored-skill-diff.py" in text
    assert "scripts/triage-report.py" in text
    assert "uv run teken cli doctor . --strict" in text
    assert 'markdownlint-cli2 "**/*.md" "#node_modules" "#.local"' in text


def test_the_markdownlint_pin_matches_the_one_tests_yml_used():
    """A different markdownlint here than in CI is the same gap, smaller.

    tests.yml no longer names the version -- the script owns it now -- so this
    pins it against the version the workflow carried when it was moved.
    """
    assert "MARKDOWNLINT_VERSION=0.21.0" in script_text()
