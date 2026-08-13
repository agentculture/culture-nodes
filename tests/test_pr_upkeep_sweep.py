"""Unit tests for the pr-upkeep sweep extractor (plan task t21).

The extractor lives in examples/pr-upkeep/sweep.py — outside the
culture_nodes package, because it is a code-node payload the workflow's
runner executes, not part of the agent CLI. It is loaded here by file path
so its deterministic parsing (SonarCloud issues JSON -> work items, Qodo
PR-comment markup -> findings) runs in the normal pytest suite against the
RECORDED fixtures in examples/pr-upkeep/fixtures/:

* ``sonarcloud-issues.json`` — the live SonarCloud issues-search response
  for componentKeys of this repo, recorded 2026-08-13 (the four standing
  unresolved issues the t22 live run targets).
* ``qodo-pr35-code-review.body.txt`` / ``qodo-pr42-code-review.body.txt``
  — the verbatim ``Code Review by Qodo`` comment bodies from this repo's
  PR #35 and PR #42 (``.txt`` so the repo-wide markdownlint sweep does not
  lint a recorded HTTP payload as if it were repo documentation).

Both recorded reviews carry only resolved/dismissed findings (the counts
header honestly reads ``Bugs (0)``), so the open-finding path is exercised
through a deterministic transformation of the SAME recorded body: stripping
the ``<s>``/``✓ Resolved``/``✗ Dismissed`` resolution markers Qodo adds
when a finding is closed, which is exactly what an open finding's summary
line looks like (an open finding simply lacks those markers).
"""

import importlib.util
import json
from pathlib import Path

import pytest

EXAMPLE_DIR = Path(__file__).resolve().parents[1] / "examples" / "pr-upkeep"
FIXTURES = EXAMPLE_DIR / "fixtures"


def _load_sweep():
    spec = importlib.util.spec_from_file_location("pr_upkeep_sweep", EXAMPLE_DIR / "sweep.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


sweep = _load_sweep()


@pytest.fixture(scope="module")
def sonar_payload():
    return json.loads((FIXTURES / "sonarcloud-issues.json").read_text())


@pytest.fixture(scope="module")
def qodo_pr35_body():
    return (FIXTURES / "qodo-pr35-code-review.body.txt").read_text()


@pytest.fixture(scope="module")
def qodo_pr42_body():
    return (FIXTURES / "qodo-pr42-code-review.body.txt").read_text()


def _reopen(body):
    """Strip Qodo's resolution markers, yielding open-finding summaries."""
    return (
        body.replace("<code>✓ Resolved</code> ", "")
        .replace("<code>✗ Dismissed</code> ", "")
        .replace("<s>", "")
        .replace("</s>", "")
    )


class TestSonarWorkItems:
    def test_recorded_response_yields_all_four_standing_issues(self, sonar_payload):
        items = sweep.sonar_work_items(sonar_payload)
        assert len(items) == 4
        assert all(item["source"] == "sonarcloud" for item in items)

    def test_component_prefix_is_stripped_to_repo_relative_paths(self, sonar_payload):
        files = {item["file"] for item in sweep.sonar_work_items(sonar_payload)}
        assert files == {
            "culture_nodes/cli/_commands/node_runs.py",
            "tests/deploy/codexsmoke_test.go",
            "tests/deploy/codexworkerenv_test.go",
            "tests/deploy/registeractor_test.go",
        }

    def test_items_carry_rule_line_message_and_severity(self, sonar_payload):
        by_rule = {item["rule"]: item for item in sweep.sonar_work_items(sonar_payload)}
        blocker = by_rule["python:S3516"]
        assert blocker["severity"] == "BLOCKER"
        assert blocker["line"] == 42
        assert blocker["file"] == "culture_nodes/cli/_commands/node_runs.py"
        assert "Refactor" in blocker["title"]
        assert blocker["id"]  # the SonarCloud issue key rides along

    def test_resolved_issues_are_dropped_defensively(self, sonar_payload):
        payload = json.loads(json.dumps(sonar_payload))
        payload["issues"][0]["status"] = "RESOLVED"
        assert len(sweep.sonar_work_items(payload)) == 3

    def test_empty_issue_list_yields_no_items(self):
        assert sweep.sonar_work_items({"issues": []}) == []


class TestQodoFindings:
    def test_counts_header_parses(self, qodo_pr35_body):
        assert sweep.qodo_counts(qodo_pr35_body) == {
            "bugs": 0,
            "rule_violations": 0,
            "skill_insights": 0,
        }

    def test_recorded_pr35_review_has_no_open_findings(self, qodo_pr35_body):
        # All three recorded findings are resolved or dismissed; an honest
        # sweep must not resurrect them as work items.
        assert sweep.qodo_findings(qodo_pr35_body) == []

    def test_recorded_pr42_review_has_no_open_findings(self, qodo_pr42_body):
        assert sweep.qodo_findings(qodo_pr42_body) == []

    def test_reopened_pr35_variant_yields_three_findings(self, qodo_pr35_body):
        findings = sweep.qodo_findings(_reopen(qodo_pr35_body))
        assert [f["title"] for f in findings] == [
            "Non-atomic run creation",
            "Human grade bypasses review separation",
            "Git diff executes repo config",
        ]

    def test_severity_badges_group_subsequent_findings(self, qodo_pr35_body):
        findings = sweep.qodo_findings(_reopen(qodo_pr35_body))
        # Finding 1 sits under the High badge; findings 2 and 3 share the
        # Medium badge (Qodo emits one badge per severity group).
        assert [f["severity"] for f in findings] == ["High", "Medium", "Medium"]

    def test_finding_kind_and_category_parse(self, qodo_pr35_body):
        findings = sweep.qodo_findings(_reopen(qodo_pr35_body))
        assert [f["kind"] for f in findings] == [
            "Bug",
            "Rule violation",
            "Bug",
        ]
        assert [f["category"] for f in findings] == [
            "Reliability",
            "Correctness",
            "Security",
        ]

    def test_finding_file_comes_from_first_code_reference(self, qodo_pr35_body):
        findings = sweep.qodo_findings(_reopen(qodo_pr35_body))
        assert [f["file"] for f in findings] == [
            "internal/api/runs.go",
            "internal/ledger/authority.go",
            "adapters/colleague/src/colleague_bridge/workspace.py",
        ]

    def test_reopened_pr42_variant_yields_its_single_finding(self, qodo_pr42_body):
        findings = sweep.qodo_findings(_reopen(qodo_pr42_body))
        assert len(findings) == 1
        assert findings[0]["severity"] == "Medium"
        assert findings[0]["kind"] == "Bug"

    def test_tip_and_context_summaries_are_not_findings(self, qodo_pr42_body):
        # "Tip of the day" and "Context" <summary> blocks carry no finding
        # number and must never parse as work.
        reopened = _reopen(qodo_pr42_body)
        assert len(sweep.qodo_findings(reopened)) == 1


class TestQodoCommentFilter:
    def test_only_code_review_bodies_from_the_qodo_bot_are_kept(
        self, qodo_pr35_body, qodo_pr42_body
    ):
        comments = [
            {"user": {"login": "qodo-code-review[bot]"}, "body": qodo_pr35_body},
            # The bot's PR-summary comment is not a findings surface.
            {
                "user": {"login": "qodo-code-review[bot]"},
                "body": "<h3>PR Summary by Qodo</h3>\n\nsummary prose",
            },
            {"user": {"login": "sonarqubecloud[bot]"}, "body": "## Quality Gate"},
            {"user": {"login": "OriNachum"}, "body": "human comment"},
            {"user": {"login": "qodo-code-review[bot]"}, "body": qodo_pr42_body},
        ]
        bodies = sweep.qodo_review_bodies(comments)
        assert bodies == [qodo_pr35_body, qodo_pr42_body]


class TestPrioritise:
    def test_merged_items_order_by_severity_rank_stably(self, sonar_payload):
        sonar_items = sweep.sonar_work_items(sonar_payload)
        qodo_items = sweep.qodo_work_items(
            [_reopen((FIXTURES / "qodo-pr35-code-review.body.txt").read_text())],
            pr_numbers=[35],
        )
        merged = sweep.prioritise(sonar_items + qodo_items)
        severities = [item["severity"] for item in merged]
        # BLOCKER first, then the CRITICAL/High band, then Medium/MINOR.
        assert severities[0] == "BLOCKER"
        ranks = [sweep.severity_rank(s) for s in severities]
        assert ranks == sorted(ranks)

    def test_qodo_work_items_carry_pr_provenance(self, qodo_pr35_body):
        items = sweep.qodo_work_items([_reopen(qodo_pr35_body)], pr_numbers=[35])
        assert len(items) == 3
        assert all(item["source"] == "qodo" for item in items)
        assert all(item["pr"] == 35 for item in items)
        assert items[0]["id"] == "pr35-qodo-1"

    def test_report_encodes_the_exit_code_contract(self, sonar_payload):
        items = sweep.sonar_work_items(sonar_payload)
        report = sweep.build_report(items)
        assert report["count"] == 4
        assert report["items"][0]["severity"] == "BLOCKER"
        assert sweep.exit_code_for(items) == 0
        assert sweep.exit_code_for([]) == sweep.EXIT_EMPTY
        assert sweep.EXIT_EMPTY == 10
