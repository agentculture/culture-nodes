"""Unit tests for the pr-upkeep sweep extractor (plan task t21).

The extractor lives in examples/pr-upkeep/sweep.py — outside the
culture_nodes package, because it is a code-node payload the workflow's
runner executes, not part of the agent CLI. It is loaded here by file path
so its deterministic parsing (SonarCloud issues JSON -> work items, Qodo
PR-comment markup -> findings, GitHub check-runs JSON -> work items) runs
in the normal pytest suite against the RECORDED fixtures in
examples/pr-upkeep/fixtures/:

* ``sonarcloud-issues.json`` — the live SonarCloud issues-search response
  for componentKeys of this repo, recorded 2026-08-13 (the four standing
  unresolved issues the t22 live run targets).
* ``qodo-pr35-code-review.body.txt`` / ``qodo-pr42-code-review.body.txt``
  — the verbatim ``Code Review by Qodo`` comment bodies from this repo's
  PR #35 and PR #42 (``.txt`` so the repo-wide markdownlint sweep does not
  lint a recorded HTTP payload as if it were repo documentation).
* ``github-check-runs-pr60.json`` — the live
  ``GET /repos/{repo}/commits/{sha}/check-runs`` response for PR #60's head
  commit ``67672519``, recorded 2026-08-14. This is the exact payload issue
  #61 was filed on: a red ``lint`` job that the two-source sweep could not
  see, sitting beside a PASSING ``SonarCloud Code Analysis`` check.
* ``github-check-runs-pr60-sonar-gate-failed.json`` — the SAME recording
  with the SonarCloud check's quality gate flipped to failed, so ONE
  payload carries both a failed Sonar-named check and a failed non-Sonar
  check (the double-count case, plan task t7's second acceptance
  criterion). It is derived rather than recorded because culture-nodes has
  never had a red SonarCloud check run in its recorded history — checked
  across the last 60 commits on main on 2026-08-14, which turned up exactly
  two failing check runs, both ``github-actions``. Exactly three fields
  differ from the verbatim recording (``conclusion``, ``output.title``,
  ``output.summary`` on that one check run), and
  ``test_derived_fixture_differs_only_in_the_sonar_gate_fields`` asserts
  that rather than leaving it as prose.

Both recorded reviews carry only resolved/dismissed findings (the counts
header honestly reads ``Bugs (0)``), so the open-finding path is exercised
through a deterministic transformation of the SAME recorded body: stripping
the ``<s>``/``✓ Resolved``/``✗ Dismissed`` resolution markers Qodo adds
when a finding is closed, which is exactly what an open finding's summary
line looks like (an open finding simply lacks those markers).
"""

import ast
import importlib.util
import json
import sys
import urllib.error
from pathlib import Path

import pytest

EXAMPLE_DIR = Path(__file__).resolve().parents[1] / "examples" / "pr-upkeep"
FIXTURES = EXAMPLE_DIR / "fixtures"

REPOSITORY_GRANT = json.dumps(
    {
        "cycle": 0,
        "repositories": [
            {
                "github_repo": "agentculture/culture-nodes",
                "sonar_component": "agentculture_culture-nodes",
            }
        ],
    }
)


def _load_sweep():
    spec = importlib.util.spec_from_file_location("pr_upkeep_sweep", EXAMPLE_DIR / "sweep.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


sweep = _load_sweep()


@pytest.fixture(autouse=True)
def repository_grant(monkeypatch):
    monkeypatch.setenv("PR_UPKEEP_REPOSITORIES", REPOSITORY_GRANT)


@pytest.fixture(scope="module")
def sonar_payload():
    return json.loads((FIXTURES / "sonarcloud-issues.json").read_text())


@pytest.fixture(scope="module")
def qodo_pr35_body():
    return (FIXTURES / "qodo-pr35-code-review.body.txt").read_text()


@pytest.fixture(scope="module")
def qodo_pr42_body():
    return (FIXTURES / "qodo-pr42-code-review.body.txt").read_text()


@pytest.fixture(scope="module")
def check_runs_payload():
    return json.loads((FIXTURES / "github-check-runs-pr60.json").read_text())


@pytest.fixture(scope="module")
def check_runs_sonar_failed_payload():
    return json.loads((FIXTURES / "github-check-runs-pr60-sonar-gate-failed.json").read_text())


@pytest.fixture(scope="module")
def jira_payload():
    return json.loads((FIXTURES / "jira-search.json").read_text())


def _reopen(body):
    """Strip Qodo's resolution markers, yielding open-finding summaries."""
    return (
        body.replace("<code>✓ Resolved</code> ", "")
        .replace("<code>✗ Dismissed</code> ", "")
        .replace("<s>", "")
        .replace("</s>", "")
    )


def _stub_sweep(
    monkeypatch,
    *,
    pulls,
    sonar_main,
    sonar_pr=None,
    qodo=None,
    check_runs=None,
    check_runs_error=None,
):
    """Stub every network call main() makes; return the per-source call log.

    `check_runs` maps head sha -> recorded check-runs payload; a sha with no
    entry answers an empty payload, which is what a green PR looks like.
    """
    monkeypatch.setenv("GITHUB_TOKEN", "")
    monkeypatch.setattr(
        sweep, "fetch_open_pulls", lambda token, repository: [dict(p) for p in pulls]
    )
    calls = {"sonar": [], "qodo": [], "checks": []}

    def fake_sonar(component, pr=None):
        calls["sonar"].append(pr)
        return sonar_main if pr is None else (sonar_pr or {"issues": []})

    def fake_qodo(token, repository, pr_numbers):
        calls["qodo"].append(list(pr_numbers))
        return qodo or ([], [])

    def fake_checks(token, repository, sha):
        calls["checks"].append(sha)
        if check_runs_error is not None:
            raise check_runs_error
        return (check_runs or {}).get(sha, {"check_runs": []})

    monkeypatch.setattr(sweep, "fetch_sonar_issues", fake_sonar)
    monkeypatch.setattr(sweep, "fetch_open_pr_comments", fake_qodo)
    monkeypatch.setattr(sweep, "fetch_check_runs", fake_checks)
    return calls


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
            {"user": {"login": "human-reviewer"}, "body": "human comment"},
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
        report = sweep.build_report(items, sweep.selected_repository())
        assert report["count"] == 4
        assert report["items"][0]["severity"] == "BLOCKER"
        assert sweep.exit_code_for(items) == 0
        assert sweep.exit_code_for([]) == sweep.EXIT_EMPTY


class TestJiraWorkItems:
    """Issue #76 acceptance is recorded-fixture-only; the live backlog is empty."""

    def test_recorded_backlog_item_enters_the_priority_list(self, jira_payload):
        items = sweep.jira_work_items(jira_payload, site="team.example.com", project="EX")
        assert len(items) == 1
        assert items[0]["source"] == "jira"
        assert items[0]["id"] == "EX-17"
        assert items[0]["severity"] == "High"
        assert sweep.prioritise(items)[0]["title"] == ("Make the recorded backlog item actionable")

    def test_jira_provenance_uses_only_reserved_example_configuration(self, jira_payload):
        (item,) = sweep.jira_work_items(jira_payload, site="team.example.com", project="EX")
        assert item["project"] == "EX"
        assert item["details_url"] == "https://team.example.com/browse/EX-17"

    def test_basic_auth_is_built_in_the_request_not_argv_or_output(self, monkeypatch):
        seen = {}

        def fake_get(url, token=None, *, basic=None):
            seen.update(url=url, token=token, basic=basic)
            return {"issues": []}

        monkeypatch.setattr(sweep, "_get_json", fake_get)
        assert sweep.fetch_jira_issues(
            "team.example.com", "EX", "robot@example.com", "fixture-token"
        ) == {"issues": []}
        assert seen["basic"] == ("robot@example.com", "fixture-token")
        assert seen["token"] is None
        assert "robot@example.com" not in seen["url"]
        assert "fixture-token" not in seen["url"]
        assert "maxResults=100" in seen["url"]
        assert 100 < sweep.JIRA_RATE_LIMIT_PER_WINDOW == 350

    def test_jira_configuration_requires_both_host_and_project(self):
        raw = json.dumps(
            {
                "repositories": [
                    {
                        "github_repo": "owner.example/repo",
                        "sonar_component": "owner_repo",
                        "jira_site": "team.example.com",
                    }
                ]
            }
        )
        with pytest.raises(ValueError, match="configured together"):
            sweep.selected_repository(raw)


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
        sweep.fetch_sonar_issues("agentculture_culture-nodes")
        sweep.fetch_sonar_issues("agentculture_culture-nodes", pr=70)
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


class TestFetchOpenPulls:
    def test_extracts_number_and_head_sha_ignoring_malformed_entries(self, monkeypatch):
        pulls = [
            {"number": 42, "head": {"sha": "cafe1234"}},
            {"number": "not-an-int", "head": {"sha": "ignored"}},
            {},
            {"number": 7},  # a PR object with no head block at all
        ]
        monkeypatch.setattr(sweep, "_get_json", lambda url, token=None: pulls)
        # Unsorted; main() sorts before capping. One request serves BOTH the
        # per-PR SonarCloud/Qodo fetches (which need the number) and the
        # check-runs fetch (which needs the head sha).
        assert sweep.fetch_open_pulls(None, "agentculture/culture-nodes") == [
            {"number": 42, "head_sha": "cafe1234"},
            {"number": 7, "head_sha": ""},
        ]


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
        calls = _stub_sweep(
            monkeypatch,
            pulls=[
                {"number": 70, "head_sha": "sha70"},
                {"number": 42, "head_sha": "sha42"},
                {"number": 35, "head_sha": "sha35"},
            ],
            sonar_main=sonar_payload,
        )

        exit_code = sweep.main()

        # Sorted ascending, capped at 2: 35 and 42 swept, 70 dropped — and
        # ALL THREE per-PR sources honour the same swept set.
        assert calls["sonar"] == [None, 35, 42]
        assert calls["qodo"] == [[35, 42]]
        assert calls["checks"] == ["sha35", "sha42"]
        assert exit_code == 0  # the main-branch fixture still has 4 items

        stderr = capsys.readouterr().err
        assert "1 open PR(s)" in stderr
        assert "[70]" in stderr
        assert "PR_UPKEEP_MAX_PRS_PER_SWEEP" in stderr

    def test_main_logs_nothing_when_every_open_pr_fits_under_the_cap(
        self, monkeypatch, capsys, sonar_payload
    ):
        monkeypatch.delenv("PR_UPKEEP_MAX_PRS_PER_SWEEP", raising=False)
        _stub_sweep(
            monkeypatch,
            pulls=[{"number": 35, "head_sha": "sha35"}],
            sonar_main=sonar_payload,
        )

        sweep.main()
        assert capsys.readouterr().err == ""
        assert sweep.EXIT_EMPTY == 10

    def test_a_pr_with_no_head_sha_is_skipped_loudly_not_silently(
        self, monkeypatch, capsys, sonar_payload
    ):
        # A check-runs query needs a head sha; a PR object that arrived
        # without one is reported rather than dropped behind the scenes.
        calls = _stub_sweep(
            monkeypatch,
            pulls=[{"number": 35, "head_sha": ""}],
            sonar_main=sonar_payload,
        )

        assert sweep.main() == 0
        assert calls["checks"] == []
        stderr = capsys.readouterr().err
        assert "35" in stderr
        assert "head sha" in stderr


class TestCheckRunWorkItems:
    """The third finding source (issue #61): a red CI check on an open PR is
    work, and the two-source sweep reported nothing for it."""

    def test_recorded_fixture_yields_the_failed_lint_check(self, check_runs_payload):
        items = sweep.check_run_work_items(check_runs_payload, pr=60)
        assert [item["check"] for item in items] == ["lint"]
        assert all(item["source"] == "github-check" for item in items)

    def test_items_carry_check_name_pr_number_and_details_url(self, check_runs_payload):
        (item,) = sweep.check_run_work_items(check_runs_payload, pr=60)
        assert item["check"] == "lint"
        assert item["pr"] == 60
        assert item["details_url"] == (
            "https://github.com/agentculture/culture-nodes/actions/runs/31718320289/job/94508621029"
        )
        assert item["conclusion"] == "failure"
        assert item["id"] == "pr60-check-94508621029"
        assert "lint" in item["title"]

    def test_a_failed_required_check_lands_in_the_high_critical_band(self, check_runs_payload):
        (item,) = sweep.check_run_work_items(check_runs_payload, pr=60)
        assert item["required"] is True
        assert item["severity"] == sweep.REQUIRED_CHECK_SEVERITY
        # HIGH and CRITICAL share rank 1 in the unified ladder; the BAND is
        # what the priority order actually reads.
        assert sweep.severity_rank(item["severity"]) == sweep.severity_rank("HIGH") == 1

    def test_a_failed_optional_check_lands_in_the_medium_band(self, check_runs_payload):
        items = sweep.check_run_work_items(check_runs_payload, pr=60, required=frozenset())
        (item,) = items
        assert item["required"] is False
        assert item["severity"] == sweep.OPTIONAL_CHECK_SEVERITY
        assert sweep.severity_rank(item["severity"]) == sweep.severity_rank("MEDIUM") == 2
        # And the two bands really do order required before optional.
        assert sweep.severity_rank(sweep.REQUIRED_CHECK_SEVERITY) < sweep.severity_rank(
            sweep.OPTIONAL_CHECK_SEVERITY
        )

    def test_passing_skipped_and_incomplete_checks_are_not_work(self, check_runs_payload):
        # The recorded payload carries 8 check runs: 6 success, 1 skipped
        # (publish), 1 failure. Only the failure is work.
        assert len(check_runs_payload["check_runs"]) == 8
        assert len(sweep.check_run_work_items(check_runs_payload, pr=60)) == 1

    def test_a_still_running_check_is_not_yet_a_failure(self, check_runs_payload):
        payload = json.loads(json.dumps(check_runs_payload))
        for check in payload["check_runs"]:
            if check["name"] == "lint":
                check["status"] = "in_progress"
                check["conclusion"] = None
        assert sweep.check_run_work_items(payload, pr=60) == []

    def test_timed_out_and_action_required_count_as_failures(self, check_runs_payload):
        for conclusion in ("timed_out", "action_required"):
            payload = json.loads(json.dumps(check_runs_payload))
            for check in payload["check_runs"]:
                if check["name"] == "version-check":
                    check["conclusion"] = conclusion
            checks = {item["check"] for item in sweep.check_run_work_items(payload, pr=60)}
            assert checks == {"lint", "version-check"}

    def test_a_cancelled_check_is_not_reported_as_work(self, check_runs_payload):
        # A cancelled run is a superseded or human-interrupted job, not a
        # finding — reporting it would put noise at the top of the list.
        payload = json.loads(json.dumps(check_runs_payload))
        for check in payload["check_runs"]:
            if check["name"] == "version-check":
                check["conclusion"] = "cancelled"
        assert [item["check"] for item in sweep.check_run_work_items(payload, pr=60)] == ["lint"]

    def test_empty_payload_yields_no_items(self):
        assert sweep.check_run_work_items({"check_runs": []}, pr=60) == []
        assert sweep.check_run_work_items({}, pr=60) == []

    def test_check_items_merge_into_the_unified_priority_order(
        self, sonar_payload, check_runs_payload
    ):
        merged = sweep.prioritise(
            sweep.sonar_work_items(sonar_payload)
            + sweep.check_run_work_items(check_runs_payload, pr=60)
        )
        ranks = [sweep.severity_rank(item["severity"]) for item in merged]
        assert ranks == sorted(ranks)
        assert merged[0]["severity"] == "BLOCKER"  # the Sonar blocker still leads

    def test_fetch_check_runs_names_the_commit_check_runs_endpoint(self, monkeypatch):
        seen = []
        monkeypatch.setattr(sweep, "_get_json", lambda url, token=None: seen.append(url) or {})
        sweep.fetch_check_runs(None, "agentculture/culture-nodes", "67672519")
        assert seen == [
            "https://api.github.com/repos/agentculture/culture-nodes"
            "/commits/67672519/check-runs?per_page=100"
        ]


class TestSonarNamedChecksAreSkipped:
    """A failing SonarCloud quality gate is ALREADY a work item via the Sonar
    issues feed; counting its check run too would double-book the same work."""

    def test_a_failed_sonar_gate_does_not_become_a_second_work_item(
        self, check_runs_sonar_failed_payload
    ):
        failures = {
            check["name"]
            for check in check_runs_sonar_failed_payload["check_runs"]
            if check["conclusion"] == "failure"
        }
        assert failures == {"SonarCloud Code Analysis", "lint"}  # the fixture has BOTH

        items = sweep.check_run_work_items(check_runs_sonar_failed_payload, pr=60)
        assert [item["check"] for item in items] == ["lint"]

    def test_skip_also_matches_on_the_app_slug(self, check_runs_payload):
        # A Sonar check renamed by a workflow author is still Sonar's; the
        # app slug (`sonarqubecloud`) is the identity that does not drift.
        payload = json.loads(json.dumps(check_runs_payload))
        for check in payload["check_runs"]:
            if check["app"]["slug"] == "sonarqubecloud":
                check["name"] = "Quality Gate"
                check["conclusion"] = "failure"
        assert [item["check"] for item in sweep.check_run_work_items(payload, pr=60)] == ["lint"]

    def test_non_sonar_checks_are_not_swept_up_by_the_skip(self, check_runs_payload):
        payload = json.loads(json.dumps(check_runs_payload))
        for check in payload["check_runs"]:
            if check["name"] == "test-publish":
                check["conclusion"] = "failure"
        checks = {item["check"] for item in sweep.check_run_work_items(payload, pr=60)}
        assert checks == {"lint", "test-publish"}

    def test_derived_fixture_differs_only_in_the_sonar_gate_fields(
        self, check_runs_payload, check_runs_sonar_failed_payload
    ):
        """The provenance claim in this module's docstring, machine-checked."""
        recorded = json.loads(json.dumps(check_runs_payload))
        derived = json.loads(json.dumps(check_runs_sonar_failed_payload))
        for payload in (recorded, derived):
            for check in payload["check_runs"]:
                if check["name"] == "SonarCloud Code Analysis":
                    del check["conclusion"]
                    del check["output"]["title"]
                    del check["output"]["summary"]
        assert recorded == derived


class TestRequiredChecks:
    def test_the_declared_required_set_names_this_repo_s_merge_gates(self):
        assert sweep.REQUIRED_CHECKS == frozenset({"test", "lint", "version-check"})
        assert sweep._required_checks() == sweep.REQUIRED_CHECKS

    def test_env_override_replaces_the_declared_set(self, monkeypatch):
        monkeypatch.setenv("PR_UPKEEP_REQUIRED_CHECKS", "lint, conformance ,")
        assert sweep._required_checks() == frozenset({"lint", "conformance"})

    def test_an_empty_override_means_nothing_is_required(self, monkeypatch):
        # Explicitly empty is a real answer (this repo's main branch is NOT
        # protected), not a reason to fall back to the declared default.
        monkeypatch.setenv("PR_UPKEEP_REQUIRED_CHECKS", "  ")
        assert sweep._required_checks() == frozenset()

    def test_the_override_reaches_the_work_items(self, monkeypatch, check_runs_payload):
        monkeypatch.setenv("PR_UPKEEP_REQUIRED_CHECKS", "version-check")
        (item,) = sweep.check_run_work_items(check_runs_payload, pr=60)
        assert item["required"] is False
        assert item["severity"] == sweep.OPTIONAL_CHECK_SEVERITY


class TestStdlibOnlyImports:
    """The sweep runs inside a pinned `python:3.12-slim` image with no PyPI
    graph — an import outside the stdlib breaks it in production, not here."""

    def test_sweep_imports_nothing_outside_the_python_312_stdlib(self):
        tree = ast.parse((EXAMPLE_DIR / "sweep.py").read_text())
        roots = set()
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                roots.update(alias.name.split(".")[0] for alias in node.names)
            elif isinstance(node, ast.ImportFrom):
                if node.level:  # a relative import has no package to resolve to
                    raise AssertionError(f"relative import at line {node.lineno}")
                roots.add((node.module or "").split(".")[0])
        assert roots, "the extractor imports nothing at all — did the parse work?"
        assert {"json", "urllib"} <= roots  # sanity: the walk really sees them
        non_stdlib = {
            root for root in roots if root != "__future__" and root not in sys.stdlib_module_names
        }
        assert non_stdlib == set()


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
        "PR_UPKEEP_MAX_PRS_PER_SWEEP",
        # Safe: this value is supplied by the trusted deployment, not run
        # input. It is the closed ordered repo/component set and cycle index,
        # so callers still cannot redirect the sweep credential.
        "PR_UPKEEP_REPOSITORIES",
        "PR_UPKEEP_REQUIRED_CHECKS",
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
        for ref in ("PR_UPKEEP_SWEEP_SOURCE_URL", "PR_UPKEEP_SWEEP_SOURCE_SHA256"):
            assert ref in readme, f"the README never names the granted value {ref}"


class TestExitCodeBranches:
    """The routable domain fact is the exit code — triage reads nothing else
    (a code node's persisted output is runner metadata, not stdout)."""

    def test_work_found_exits_zero(self, monkeypatch, capsys, sonar_payload):
        _stub_sweep(
            monkeypatch,
            pulls=[{"number": 35, "head_sha": "sha35"}],
            sonar_main=sonar_payload,
        )
        assert sweep.main() == 0
        report = json.loads(capsys.readouterr().out)
        assert report["count"] == 4

    def test_a_check_failure_alone_is_enough_to_exit_zero(self, monkeypatch, capsys):
        # The whole point of issue #61: with both older sources silent, a red
        # required check still routes the loop to `fix` instead of `backoff`.
        payload = json.loads((FIXTURES / "github-check-runs-pr60.json").read_text())
        _stub_sweep(
            monkeypatch,
            pulls=[{"number": 60, "head_sha": "sha60"}],
            sonar_main={"issues": []},
            check_runs={"sha60": payload},
        )
        assert sweep.main() == 0
        report = json.loads(capsys.readouterr().out)
        assert report["count"] == 1
        assert report["items"][0]["source"] == "github-check"
        assert report["items"][0]["check"] == "lint"
        assert report["items"][0]["pr"] == 60

    def test_recorded_jira_item_surfaces_from_a_fixture_sweep_run(
        self, monkeypatch, capsys, jira_payload
    ):
        monkeypatch.setenv(
            "PR_UPKEEP_REPOSITORIES",
            json.dumps(
                {
                    "cycle": 0,
                    "repositories": [
                        {
                            "github_repo": "owner.example/repo",
                            "sonar_component": "owner_repo",
                            "jira_site": "team.example.com",
                            "jira_project": "EX",
                        }
                    ],
                }
            ),
        )
        monkeypatch.setenv("JIRA_ACCOUNT_EMAIL", "robot@example.com")
        monkeypatch.setenv("JIRA_API_TOKEN", "fixture-token")
        _stub_sweep(monkeypatch, pulls=[], sonar_main={"issues": []})
        monkeypatch.setattr(
            sweep,
            "fetch_jira_issues",
            lambda site, project, email, token: jira_payload,
        )

        assert sweep.main() == 0
        report = json.loads(capsys.readouterr().out)
        assert report["github_repo"] == "owner.example/repo"
        assert report["count"] == 1
        assert report["items"][0]["source"] == "jira"
        assert report["items"][0]["id"] == "EX-17"

    def test_a_clean_empty_sweep_exits_ten(self, monkeypatch, capsys):
        _stub_sweep(
            monkeypatch,
            pulls=[{"number": 35, "head_sha": "sha35"}],
            sonar_main={"issues": []},
        )
        assert sweep.main() == sweep.EXIT_EMPTY == 10
        report = json.loads(capsys.readouterr().out)
        assert report["count"] == 0  # still a well-formed report, just empty

    def test_a_broken_sweep_exits_neither_zero_nor_empty(self, monkeypatch, capsys):
        _stub_sweep(
            monkeypatch,
            pulls=[{"number": 60, "head_sha": "sha60"}],
            sonar_main={"issues": []},
            check_runs_error=urllib.error.URLError("check-runs unreachable"),
        )
        exit_code = sweep.main()
        assert exit_code not in (0, sweep.EXIT_EMPTY)
        assert exit_code == 1
        captured = capsys.readouterr()
        assert "sweep failed" in captured.err
        assert captured.out == ""  # no invented empty report on a broken sweep
