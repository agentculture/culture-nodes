"""Tests for the issue-type dimension scripts/triage-report.py grew.

The type read is deliberately the only new GitHub call, and it is per-issue
GraphQL. The search `type:` / `no:type` qualifiers are not an option here: they
fail OPEN (`type:NotARealType` returns 0 results rather than an error) and the
search index lags writes, so a report built on them would show a confident zero
for a type that exists and a stale count right after a backfill.
"""

from __future__ import annotations

import csv
import importlib.util
import json
import re
from pathlib import Path

import pytest

ROOT = Path(__file__).parents[1]
SCRIPT = ROOT / "scripts" / "triage-report.py"
SPEC = importlib.util.spec_from_file_location("triage_report", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader
SPEC.loader.exec_module(MODULE)

ORG_TYPES = [
    {"id": "IT_task", "name": "Task", "isEnabled": True},
    {"id": "IT_bug", "name": "Bug", "isEnabled": True},
    {"id": "IT_feature", "name": "Feature", "isEnabled": True},
    {"id": "IT_record", "name": "Record", "isEnabled": True},
]
SINCE = "2026-08-01T00:00:00Z"


class FakeGh:
    """The `gh` seam: (command) -> (returncode, stdout, stderr)."""

    def __init__(self, open_issues=(), closed_issues=(), org_types=None, fail=False):
        self.open_issues = list(open_issues)
        self.closed_issues = list(closed_issues)
        self.org_types = ORG_TYPES if org_types is None else org_types
        self.fail = fail
        self.commands: list[list[str]] = []

    def __call__(self, command):
        self.commands.append(list(command))
        if self.fail:
            return 1, "", "HTTP 403: Resource not accessible by integration"
        query = ""
        for index, token in enumerate(command):
            if token == "-f" and command[index + 1].startswith("query="):
                query = command[index + 1]
        if "issueTypes" in query:
            payload = {"data": {"organization": {"issueTypes": {"nodes": self.org_types}}}}
            return 0, json.dumps(payload), ""
        nodes = self.closed_issues if "CLOSED" in query else self.open_issues
        payload = {
            "data": {
                "repository": {
                    "issues": {
                        "pageInfo": {"hasNextPage": False, "endCursor": None},
                        "nodes": nodes,
                    }
                }
            }
        }
        return 0, json.dumps(payload), ""


def issue(number, type_name=None, closed_at=None):
    node = {"number": number, "issueType": None if type_name is None else {"name": type_name}}
    if closed_at:
        node["closedAt"] = closed_at
    return node


def fixtures(tmp_path, numbers):
    issues_json = tmp_path / "issues.json"
    issues_json.write_text(json.dumps([{"number": n} for n in numbers]), encoding="utf-8")
    table = tmp_path / "dispositions.csv"
    with table.open("w", encoding="utf-8", newline="") as handle:
        writer = csv.writer(handle)
        writer.writerow(["issue", "bucket", "disposition", "evidence_pointer"])
        for number in numbers:
            writer.writerow([number, "finish work", "planned", "fixture"])
    return issues_json, table


def run_main(tmp_path, numbers, gh, extra=()):
    issues_json, table = fixtures(tmp_path, numbers)
    argv = [
        "--issues-json",
        str(issues_json),
        "--table",
        str(table),
        "--output",
        str(tmp_path / "open-issues.md"),
        "--closed-since",
        SINCE,
        "--backoff-seconds",
        "0",
        *extra,
    ]
    return MODULE.main(argv, invoke=gh)


def test_types_are_never_read_through_a_search_query(tmp_path):
    gh = FakeGh(open_issues=[issue(5, "Bug"), issue(6)])

    assert run_main(tmp_path, [5, 6], gh) == 0

    assert gh.commands, "the report must actually read types"
    for command in gh.commands:
        joined = " ".join(command)
        assert command[:3] == ["gh", "api", "graphql"], command
        assert "search" not in joined
        assert "no:type" not in joined
        assert "type:" not in joined


def test_the_source_carries_no_search_qualifier():
    source = SCRIPT.read_text(encoding="utf-8")
    body = "\n".join(line for line in source.splitlines() if not line.lstrip().startswith("#"))
    assert "no:type" not in body
    assert '"search"' not in body


def test_unknown_type_name_exits_non_zero_rather_than_counting_zero(tmp_path, capsys):
    gh = FakeGh(open_issues=[issue(5, "Bogus"), issue(6, "Task")])

    code = run_main(tmp_path, [5, 6], gh)

    err = capsys.readouterr().err
    assert code == 1
    assert "Bogus" in err


def test_type_names_come_from_the_org_at_run_time(tmp_path):
    gh = FakeGh(open_issues=[issue(5, "Task")])
    run_main(tmp_path, [5], gh)

    queries = [
        argument for command in gh.commands for argument in command if argument.startswith("query=")
    ]
    assert any("issueTypes" in query and "organization" in query for query in queries)


def test_a_failing_type_read_exits_2_not_1(tmp_path, capsys):
    gh = FakeGh(fail=True)

    code = run_main(tmp_path, [5], gh)

    assert code == 2
    assert "could not" in capsys.readouterr().err.lower()


def test_a_malformed_disposition_table_exits_1_not_2(tmp_path, capsys):
    issues_json, table = fixtures(tmp_path, [5])
    table.write_text("issue,bucket,disposition\n5,finish work,planned\n", encoding="utf-8")

    code = MODULE.main(
        [
            "--issues-json",
            str(issues_json),
            "--table",
            str(table),
            "--output",
            str(tmp_path / "open-issues.md"),
            "--closed-since",
            SINCE,
            "--backoff-seconds",
            "0",
        ],
        invoke=FakeGh(),
    )

    assert code == 1
    assert "expected columns" in capsys.readouterr().err.lower()


def test_the_type_read_reuses_the_shared_retry(tmp_path):
    gh = FakeGh(fail=True)
    run_main(tmp_path, [5], gh)
    assert len(gh.commands) == MODULE.GH_ATTEMPTS

    source = SCRIPT.read_text(encoding="utf-8")
    assert source.count("for attempt in range(1, GH_ATTEMPTS + 1)") == 1
    assert source.count("class GitHubUnreachable") == 1


def test_rendered_table_carries_open_and_closed_type_blocks(tmp_path):
    gh = FakeGh(
        open_issues=[issue(5, "Bug"), issue(6, "Task"), issue(7)],
        closed_issues=[
            issue(1, "Record", "2026-08-10T00:00:00Z"),
            issue(2, "Task", "2026-08-11T00:00:00Z"),
            issue(3, "Record", "2026-07-01T00:00:00Z"),
        ],
    )
    output = tmp_path / "open-issues.md"
    issues_json, table = fixtures(tmp_path, [5, 6, 7])
    code = MODULE.main(
        [
            "--issues-json",
            str(issues_json),
            "--table",
            str(table),
            "--output",
            str(output),
            "--closed-since",
            SINCE,
            "--backoff-seconds",
            "0",
        ],
        invoke=gh,
    )
    assert code == 0

    content = output.read_text(encoding="utf-8")
    assert "Open issues by type" in content
    assert "closed since" in content.lower()
    open_block, _, closed_block = content.partition("closed since")
    assert "| Bug | 1 |" in open_block
    assert "| (no type) | 1 |" in open_block
    # The closed block counts one Record: the third closed before the boundary.
    assert "| Record | 1 |" in closed_block
    assert "| Task | 1 |" in closed_block


def test_every_type_count_names_the_date_typing_began(tmp_path):
    gh = FakeGh(
        open_issues=[issue(5, "Bug"), issue(6)],
        closed_issues=[issue(1, "Record", "2026-08-10T00:00:00Z")],
    )
    output = tmp_path / "open-issues.md"
    issues_json, table = fixtures(tmp_path, [5, 6])
    MODULE.main(
        [
            "--issues-json",
            str(issues_json),
            "--table",
            str(table),
            "--output",
            str(output),
            "--closed-since",
            SINCE,
            "--backoff-seconds",
            "0",
        ],
        invoke=gh,
    )

    content = output.read_text(encoding="utf-8")
    type_rows = [
        line
        for line in content.splitlines()
        if re.match(r"^\| (Task|Bug|Feature|Record|\(no type\)) \|", line)
    ]
    # Bug, (no type) in the open block; Record in the closed block.
    assert len(type_rows) >= 3, content
    for line in type_rows:
        assert MODULE.TYPING_BEGAN in line, line


def test_the_bucket_table_and_its_six_buckets_are_untouched(tmp_path):
    gh = FakeGh(open_issues=[issue(5, "Bug")])
    output = tmp_path / "open-issues.md"
    issues_json, table = fixtures(tmp_path, [5])
    MODULE.main(
        [
            "--issues-json",
            str(issues_json),
            "--table",
            str(table),
            "--output",
            str(output),
            "--closed-since",
            SINCE,
            "--backoff-seconds",
            "0",
        ],
        invoke=gh,
    )

    content = output.read_text(encoding="utf-8")
    assert "| Issue | Bucket | Disposition | Evidence pointer |" in content
    assert "| #5 | finish work | planned | fixture |" in content
    assert "Open issues with dispositions: 1" in content
    assert MODULE.BUCKETS == {
        "verify-then-close",
        "operator-lane enablers",
        "bug tail",
        "finish work",
        "owner decisions",
        "large bets",
    }


def test_dispositions_csv_keeps_its_exact_four_column_header():
    header = (
        (ROOT / "docs" / "triage" / "dispositions.csv").read_text(encoding="utf-8").splitlines()[0]
    )
    assert header == "issue,bucket,disposition,evidence_pointer"


def test_count_by_type_rejects_a_name_the_org_does_not_define():
    with pytest.raises(ValueError):
        MODULE.count_by_type([{"type": "Bogus"}], ["Task", "Bug"])


def test_issue_types_is_one_replaceable_function():
    """c31: a future `gitculture issue list --json issueType` replaces one call."""
    assert callable(MODULE.issue_types)
    assert "gitculture" in (MODULE.issue_types.__doc__ or "")


def test_a_truncated_org_type_menu_is_refused_not_used():
    """Half a menu is worse than none: a real type would read as unknown.

    `issueTypes` was requested with a bare `first: 20` and no truncation check,
    so an org with more types than one page would silently lose the tail — and
    `count_by_type` would then reject a perfectly valid type as unknown. Same
    fail-open shape as the search qualifier this whole lane avoids. Flagged in
    review of PR #163.
    """
    calls = []

    def invoke(command):
        calls.append(command)
        return (
            0,
            json.dumps(
                {
                    "data": {
                        "organization": {
                            "issueTypes": {
                                "pageInfo": {"hasNextPage": True},
                                "nodes": [{"name": "Task", "isEnabled": True}],
                            }
                        }
                    }
                }
            ),
            "",
        )

    with pytest.raises(MODULE.GitHubUnreachable) as excinfo:
        MODULE.org_type_names("agentculture", invoke=invoke, backoff=0)
    assert "not read in full" in str(excinfo.value)


def test_a_failed_read_names_which_read_failed():
    """ "Could not read from GitHub" cannot tell two different failures apart.

    The issue reads are repository-scoped; the org type menu is an
    ORGANISATION object no repository-scoped token (every Actions GITHUB_TOKEN
    is one) can see. Conflating them cost a CI round-trip to diagnose, so the
    message names the read.
    """

    def forbidden(command):
        return 1, "", "gh: Resource not accessible by integration"

    with pytest.raises(MODULE.GitHubUnreachable) as excinfo:
        MODULE.org_type_names("agentculture", invoke=forbidden, backoff=0)
    message = str(excinfo.value)
    assert "issue-type menu" in message
    assert "ORGANISATION" in message, "the message names why the privilege differs"


def _no_org_reads(command):
    """A seam that fails loudly if anything tries to read the org."""
    joined = " ".join(command)
    assert "organization" not in joined, "--check must not read the org's type menu"
    assert "issueType" not in joined, "--check must not read per-issue types"
    return 0, json.dumps([{"number": 1}]), ""


def test_check_reads_no_types_at_all(tmp_path, monkeypatch):
    """--check verifies the disposition table and nothing that needs org access.

    CI's GITHUB_TOKEN is repository-scoped, so `organization.issueTypes` is
    unreachable there no matter what permissions the workflow requests — proven
    in CI, not assumed. Attempting it made every lint run exit 2 ("could not
    measure"), so --check now verifies the half it can verify.

    The cost is deliberate and recorded as deviation d1: a stale type block is
    not caught by CI. This test exists so the *scope* of that cost cannot grow
    silently — if someone reintroduces a type read on the --check path, this
    fails.
    """
    output = tmp_path / "open-issues.md"
    table = tmp_path / "dispositions.csv"
    table.write_text(
        "issue,bucket,disposition,evidence_pointer\n1,bug tail,do the thing,somewhere\n",
        encoding="utf-8",
    )
    issues = tmp_path / "issues.json"
    issues.write_text(json.dumps([{"number": 1}]), encoding="utf-8")

    # Generate the table body the way a real run would, then append a type
    # section that --check must ignore rather than validate.
    body = MODULE.render([1], MODULE.dispositions(table))
    output.write_text(body + "\n" + MODULE.TYPES_HEADING + "\n\nstale nonsense\n", encoding="utf-8")

    code = MODULE.main(
        [
            "--check",
            "--issues-json",
            str(issues),
            "--table",
            str(table),
            "--output",
            str(output),
        ],
        invoke=_no_org_reads,
    )
    assert code == 0, "a correct table with an unchecked type section passes"


def test_check_still_catches_a_stale_disposition_table(tmp_path):
    """Dropping the type check must not soften the check that remains."""
    output = tmp_path / "open-issues.md"
    table = tmp_path / "dispositions.csv"
    table.write_text(
        "issue,bucket,disposition,evidence_pointer\n1,bug tail,do the thing,somewhere\n",
        encoding="utf-8",
    )
    issues = tmp_path / "issues.json"
    issues.write_text(json.dumps([{"number": 1}]), encoding="utf-8")
    output.write_text(
        "# Open-issue triage\n\nwrong content\n" + MODULE.TYPES_HEADING + "\n", encoding="utf-8"
    )

    code = MODULE.main(
        [
            "--check",
            "--issues-json",
            str(issues),
            "--table",
            str(table),
            "--output",
            str(output),
        ],
        invoke=_no_org_reads,
    )
    assert code == 1, "a stale table is a finding, and findings exit 1"


def test_check_does_not_need_git_history(tmp_path, monkeypatch):
    """--check must not resolve the previous-cycle commit.

    previous_cycle_start() reads a hardcoded SHA through `git show`, which
    fails on a SHALLOW checkout — and publish.yml checks out shallow. Computing
    it unconditionally made --check exit 2 there for a boundary date it never
    uses; only the closed-issue type block needs one.

    An independent review flagged the hardcoded SHA as a rot hazard and it was
    dismissed after checking tests.yml (fetch-depth: 0) and not publish.yml.
    This test is the correction: it fails if the boundary is ever computed on
    the --check path again, whatever the checkout depth.
    """

    def explode():
        raise AssertionError("--check must not resolve the previous-cycle commit")

    monkeypatch.setattr(MODULE, "previous_cycle_start", explode)

    output = tmp_path / "open-issues.md"
    table = tmp_path / "dispositions.csv"
    table.write_text(
        "issue,bucket,disposition,evidence_pointer\n1,bug tail,do the thing,somewhere\n",
        encoding="utf-8",
    )
    issues = tmp_path / "issues.json"
    issues.write_text(json.dumps([{"number": 1}]), encoding="utf-8")
    output.write_text(
        MODULE.render([1], MODULE.dispositions(table)) + "\n" + MODULE.TYPES_HEADING + "\n",
        encoding="utf-8",
    )

    assert (
        MODULE.main(
            [
                "--check",
                "--issues-json",
                str(issues),
                "--table",
                str(table),
                "--output",
                str(output),
            ],
            invoke=_no_org_reads,
        )
        == 0
    )


def test_an_unquoted_comma_in_a_row_is_refused_not_silently_truncated(tmp_path):
    """A malformed row must not render as an accurate-looking disposition (#215).

    Only the HEADER's column set was validated, so an unquoted comma inside a
    disposition shifted every later field left: the evidence pointer became a
    fragment of the disposition and the tail vanished under DictReader's
    restkey. The generated table then showed a plausible wrong row, and
    `--check` approved it. Two rows in the real file were corrupted this way
    before the check existed.
    """
    csv_path = tmp_path / "dispositions.csv"
    # A LATER row is the real case: the first row is well-formed, so the
    # header-shape check that already existed passes and the file reaches the
    # per-row walk.
    csv_path.write_text(
        "issue,bucket,disposition,evidence_pointer\n"
        "1,bug tail,fine,issue #1\n"
        "2,bug tail,a disposition, with an unquoted comma,issue #2\n",
        encoding="utf-8",
    )
    with pytest.raises(ValueError, match="must be quoted"):
        MODULE.dispositions(csv_path)


def test_a_short_row_is_refused_too(tmp_path):
    csv_path = tmp_path / "dispositions.csv"
    csv_path.write_text(
        "issue,bucket,disposition,evidence_pointer\n"
        "1,bug tail,fine,issue #1\n"
        "2,bug tail,only three\n",
        encoding="utf-8",
    )
    with pytest.raises(ValueError, match="must be quoted"):
        MODULE.dispositions(csv_path)


def test_a_correctly_quoted_comma_still_round_trips(tmp_path):
    csv_path = tmp_path / "dispositions.csv"
    body = "a disposition, with a quoted comma"
    with csv_path.open("w", encoding="utf-8", newline="") as handle:
        writer = csv.writer(handle)
        writer.writerow(["issue", "bucket", "disposition", "evidence_pointer"])
        writer.writerow(["1", "bug tail", body, "issue #1"])
    assert MODULE.dispositions(csv_path)[1]["disposition"] == body


def test_the_checked_in_dispositions_file_has_no_unquoted_commas():
    """The real file is the thing that was broken; pin it directly."""
    MODULE.dispositions(ROOT / "docs" / "triage" / "dispositions.csv")
