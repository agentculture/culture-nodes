"""Repository-grant / self-echo configuration tests, split from test_pr_upkeep_sweep.py to keep
it under the 1000-line hard limit (tests/lint filelength guard)."""

import ast
import importlib.util
import json
import sys
import urllib.error
from pathlib import Path

import pytest

from tests.test_pr_upkeep_sweep import EXAMPLE_DIR, FIXTURES, _stub_sweep, sweep  # noqa: F401


class TestTheSweptRepoIsDeploymentGrantedAndSaysSo:
    """The blast radius is a closed deployment grant, never run input."""

    #: Every environment value the sweep is allowed to read. An exact set, not
    #: a subset: the whole point of the boundary is that no environment value
    #: re-points the swept repo, and "we only added one more" is exactly the
    #: change that should have to be made deliberately, in a diff that edits
    #: this line and says why.
    ALLOWED_ENVIRONMENT_READS = {
        "GITHUB_TOKEN",
        # Safe: these are the two halves of Jira Cloud's measured Basic-auth
        # requirement. They authenticate only the configured Jira source and
        # are never copied into a report, argv, fixture, or diagnostic.
        "JIRA_ACCOUNT_EMAIL",
        "JIRA_API_TOKEN",
        # Safe: these identify and authenticate the one control-plane event
        # ingress; they cannot redirect a source credential to another repo.
        "NODES_API_URL",
        "NODES_EVENT_TOKEN",
        "PR_UPKEEP_MAX_PRS_PER_SWEEP",
        # Safe: this value is supplied by the trusted deployment, not run
        # input. It is the closed ordered repo/component set and cycle index,
        # so callers still cannot redirect the sweep credential.
        "PR_UPKEEP_REPOSITORIES",
        "PR_UPKEEP_REQUIRED_CHECKS",
        # Safe: read-only credential for ONE fixed host (sonarcloud.io) whose
        # URLs are module constants, so no caller can point it elsewhere. It
        # is optional — both Sonar queries succeed anonymously against a
        # public project, measured on `agentculture_culture-nodes` — and it
        # exists for the two cases where anonymous stops working: SonarCloud
        # rate-limits unauthenticated requests harder, and a project turning
        # private makes every anonymous query 401 while nothing else about the
        # sweep changes.
        "SONAR_TOKEN",
    }

    @staticmethod
    def _source():
        return (EXAMPLE_DIR / "sweep.py").read_text()

    @staticmethod
    def _prose(text):
        """Wrapped prose as one line, so an assertion about a phrase is not
        really an assertion about where the author's line breaks fell."""
        return " ".join(text.replace("#", " ").split())

    @classmethod
    def _preceding_comment_block(cls, source, needle):
        """The contiguous run of `#` comment lines directly above `needle`,
        normalised to a single line of prose."""
        lines = source.splitlines()
        for index, line in enumerate(lines):
            if line.startswith(needle):
                block = []
                cursor = index - 1
                while cursor >= 0 and lines[cursor].lstrip().startswith("#"):
                    block.append(lines[cursor])
                    cursor -= 1
                return cls._prose("\n".join(reversed(block)))
        raise AssertionError(f"sweep.py has no line starting with {needle!r}")

    def test_configured_order_and_cycle_select_exactly_one_repo(self):
        raw = json.dumps(
            {
                "cycle": 1,
                "repositories": [
                    {"github_repo": "one.example/repo", "sonar_component": "one_repo"},
                    {"github_repo": "two.example/repo", "sonar_component": "two_repo"},
                ],
            }
        )
        selected = sweep.selected_repository(raw)
        assert selected["github_repo"] == "two.example/repo"
        assert selected["sonar_component"] == "two_repo"

    def test_repo_pair_is_not_a_module_constant(self):
        source = self._source()
        assert "SONAR_COMPONENT_KEY =" not in source
        assert "GITHUB_REPO =" not in source

    def test_environment_reads_are_exactly_the_deliberately_granted_set(self):
        names = set()
        for node in ast.walk(ast.parse(self._source())):
            if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute):
                target = node.func.value
                if (
                    node.func.attr in {"get", "getenv"}
                    and node.args
                    and isinstance(node.args[0], ast.Constant)
                    and isinstance(target, ast.Attribute)
                    and target.attr == "environ"
                ):
                    names.add(node.args[0].value)
            elif isinstance(node, ast.Subscript):
                target = node.value
                if (
                    isinstance(target, ast.Attribute)
                    and target.attr == "environ"
                    and isinstance(node.slice, ast.Constant)
                ):
                    names.add(node.slice.value)
        assert names == self.ALLOWED_ENVIRONMENT_READS, (
            "the sweep reads environment values this test does not sanction. If a new "
            "one is legitimate, add it above deliberately and explain why it is safe."
        )

    def test_the_moved_boundary_is_explained_where_the_grant_is_named(self):
        block = self._preceding_comment_block(self._source(), "REPOSITORIES_ENV =")
        assert (
            "blast radius" in block.lower()
        ), "the comment does not preserve why repository selection is a trust boundary"
        assert "closed" in block.lower()
        assert "run input" in block.lower()

    def test_the_readme_carries_the_deployment_configuration_section(self):
        # Criterion 2 for this example: every environment-specific value is
        # pointed at, with its source named. The workflow's granted
        # environment values are the ones a reader cannot otherwise trace —
        # they resolve in the worker process's environment, not in the file.
        readme = (EXAMPLE_DIR / "README.md").read_text()
        assert "## Deployment configuration" in readme
        for ref in (
            "PR_UPKEEP_SWEEP_SOURCE_URL",
            "PR_UPKEEP_SWEEP_SOURCE_SHA256",
            "PR_UPKEEP_SWEEP_JIRA_SOURCE_URL",
            "PR_UPKEEP_SWEEP_JIRA_SOURCE_SHA256",
        ):
            assert ref in readme, f"the README never names the granted value {ref}"
