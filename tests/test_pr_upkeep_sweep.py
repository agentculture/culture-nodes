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


class TestSonarWorkItemsPrContext:
    """The gap that made a live sweep miss PR #70's nine issues: SonarCloud
    answers a plain componentKeys query with main-branch results only, so
    findings need their PR analysis context named explicitly."""

    def test_main_branch_items_carry_no_pr(self, sonar_payload):
        items = sweep.sonar_work_items(sonar_payload)
        assert all(item["pr"] is None for item in items)

    def test_pr_scoped_items_carry_the_queried_pr(self, sonar_payload):
        items = sweep.sonar_work_items(sonar_payload, pr=70)
        assert items  # the fixture is non-empty
        assert all(item["pr"] == 70 for item in items)

    def test_fetch_sonar_issues_url_names_the_pull_request_param(self, monkeypatch):
        seen = []
        monkeypatch.setattr(sweep, "_get_json", lambda url, token=None: seen.append(url) or {})
        sweep.fetch_sonar_issues()
        sweep.fetch_sonar_issues(pr=70)
        assert "pullRequest=" not in seen[0]
        assert seen[0].startswith(sweep.SONAR_ISSUES_URL.split("?")[0])
        assert "pullRequest=70" in seen[1]


class TestDedupeSonarItems:
    def test_no_collision_keeps_every_item(self, sonar_payload):
        main_items = sweep.sonar_work_items(sonar_payload)
        deduped = sweep.dedupe_sonar_items(main_items)
        assert deduped == main_items

    def test_same_key_on_main_and_a_pr_collapses_to_the_pr_scoped_entry(self, sonar_payload):
        main_items = sweep.sonar_work_items(sonar_payload)
        # Simulate the SAME underlying issue also showing up on a PR-scoped
        # query (identical key, transiently visible on both around a merge).
        pr_items = sweep.sonar_work_items(sonar_payload, pr=70)
        deduped = sweep.dedupe_sonar_items(main_items + pr_items)
        assert len(deduped) == len(main_items)
        assert all(item["pr"] == 70 for item in deduped)

    def test_same_fingerprint_different_key_still_collapses(self, sonar_payload):
        # SonarCloud's key-stability across analysis contexts is not
        # guaranteed (module docstring's stated assumption); the fallback
        # (rule, file, line, title) fingerprint must catch this case too.
        main_items = sweep.sonar_work_items(sonar_payload)
        one = dict(main_items[0])
        one["id"] = "AZ-a-totally-different-key"
        one["pr"] = 70
        deduped = sweep.dedupe_sonar_items(main_items + [one])
        assert len(deduped) == len(main_items)
        matching = [item for item in deduped if item["rule"] == one["rule"]]
        assert len(matching) == 1
        assert matching[0]["pr"] == 70

    def test_genuinely_distinct_findings_all_survive(self, sonar_payload):
        pr_items = sweep.sonar_work_items(sonar_payload, pr=70)
        other_pr_item = dict(pr_items[0])
        other_pr_item["id"] = "AZ-different-key-on-a-different-pr"
        other_pr_item["rule"] = "python:S9999-not-a-real-rule"
        other_pr_item["pr"] = 71
        deduped = sweep.dedupe_sonar_items(pr_items + [other_pr_item])
        assert len(deduped) == len(pr_items) + 1


class TestFetchOpenPullNumbers:
    def test_extracts_sorted_numbers_ignoring_malformed_entries(self, monkeypatch):
        pulls = [{"number": 42}, {"number": "not-an-int"}, {}, {"number": 7}]
        monkeypatch.setattr(sweep, "_get_json", lambda url, token=None: pulls)
        numbers = sweep.fetch_open_pull_numbers(None)
        assert numbers == [42, 7]  # unsorted; main() sorts before capping


class TestMaxPrsPerSweepCap:
    def test_default_cap_applies_without_the_env_override(self, monkeypatch):
        monkeypatch.delenv("PR_UPKEEP_MAX_PRS_PER_SWEEP", raising=False)
        assert sweep._max_prs_per_sweep() == sweep.MAX_PRS_PER_SWEEP

    def test_env_override_is_honored(self, monkeypatch):
        monkeypatch.setenv("PR_UPKEEP_MAX_PRS_PER_SWEEP", "3")
        assert sweep._max_prs_per_sweep() == 3

    def test_invalid_or_non_positive_override_falls_back_to_the_default(self, monkeypatch):
        monkeypatch.setenv("PR_UPKEEP_MAX_PRS_PER_SWEEP", "not-a-number")
        assert sweep._max_prs_per_sweep() == sweep.MAX_PRS_PER_SWEEP
        monkeypatch.setenv("PR_UPKEEP_MAX_PRS_PER_SWEEP", "0")
        assert sweep._max_prs_per_sweep() == sweep.MAX_PRS_PER_SWEEP

    def test_main_sweeps_only_the_capped_prs_and_logs_the_rest(
        self, monkeypatch, capsys, sonar_payload
    ):
        monkeypatch.setenv("PR_UPKEEP_MAX_PRS_PER_SWEEP", "2")
        monkeypatch.setenv("GITHUB_TOKEN", "")
        monkeypatch.setattr(sweep, "fetch_open_pull_numbers", lambda token: [70, 42, 35])

        sonar_calls = []

        def fake_fetch_sonar(pr=None):
            sonar_calls.append(pr)
            return {"issues": []} if pr is not None else sonar_payload

        monkeypatch.setattr(sweep, "fetch_sonar_issues", fake_fetch_sonar)

        qodo_calls = []

        def fake_fetch_qodo(token, pr_numbers):
            qodo_calls.append(list(pr_numbers))
            return [], []

        monkeypatch.setattr(sweep, "fetch_open_pr_comments", fake_fetch_qodo)

        exit_code = sweep.main()

        # Sorted ascending, capped at 2: 35 and 42 swept, 70 dropped.
        assert sonar_calls == [None, 35, 42]
        assert qodo_calls == [[35, 42]]
        assert exit_code == 0  # the main-branch fixture still has 4 items

        stderr = capsys.readouterr().err
        assert "1 open PR(s)" in stderr
        assert "[70]" in stderr
        assert "PR_UPKEEP_MAX_PRS_PER_SWEEP" in stderr

    def test_main_logs_nothing_when_every_open_pr_fits_under_the_cap(
        self, monkeypatch, capsys, sonar_payload
    ):
        monkeypatch.delenv("PR_UPKEEP_MAX_PRS_PER_SWEEP", raising=False)
        monkeypatch.setenv("GITHUB_TOKEN", "")
        monkeypatch.setattr(sweep, "fetch_open_pull_numbers", lambda token: [35])
        monkeypatch.setattr(
            sweep,
            "fetch_sonar_issues",
            lambda pr=None: {"issues": []} if pr is not None else sonar_payload,
        )
        monkeypatch.setattr(sweep, "fetch_open_pr_comments", lambda token, pr_numbers: ([], []))

        sweep.main()
        assert capsys.readouterr().err == ""
        assert sweep.EXIT_EMPTY == 10
