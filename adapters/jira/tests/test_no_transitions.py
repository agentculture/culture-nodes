"""Audit the shipped Jira adapter, not merely a mocked request."""

from pathlib import Path


def test_only_the_narrow_transition_module_can_reach_a_jira_transition_endpoint():
    package = Path(__file__).parents[1] / "src" / "jira_bridge"
    forbidden = "trans" + "itions"
    offenders = []
    for path in package.glob("*.py"):
        if forbidden.casefold() in path.read_text(encoding="utf-8").casefold():
            offenders.append(path.name)
    assert offenders == ["transition_issue.py"], f"unexpected Jira transition endpoint surface: {offenders}"
